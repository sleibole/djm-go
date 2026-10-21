package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"gorm.io/gorm"
)

const (
	maxRowsPerAccount = 10_000_000
	buildErrorMessage = "Something went wrong while processing this file. Please try uploading it again."
	builderLockFile   = ".builder.lock"
	staleTempAge      = 10 * time.Minute
)

// runBuilder builds uploaded catalogs one at a time until ctx is done. The
// queue is the catalogs table itself (status pending), so nothing is lost on
// a restart. Besides wakeBuilder, it looks for pending work every buildPoll,
// which picks up catalogs another process left pending.
func (a *app) runBuilder(ctx context.Context) {
	unlock, err := a.lockBuilder(ctx)
	if err != nil {
		if ctx.Err() == nil {
			a.logger.Error("catalog builder lock", "err", err)
		}
		return
	}
	defer unlock()
	a.requeueBuilds()
	tick := time.NewTicker(a.buildPoll)
	defer tick.Stop()
	for {
		a.cache.removeStaleTemps(staleTempAge)
		for ctx.Err() == nil && a.buildNext(ctx) {
		}
		select {
		case <-ctx.Done():
			return
		case <-a.buildWake:
		case <-tick.C:
		}
	}
}

// lockBuilder waits for an exclusive lock on a file in the cache directory, so
// only one process builds at a time. During a zero-downtime deploy the old
// container holds it until it exits; the kernel releases it however the
// process ends. Holding it means no other builder is alive, so builds marked
// processing were cut off and can be requeued.
func (a *app) lockBuilder(ctx context.Context) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(a.cfg.CatalogCacheDir, builderLockFile), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	waiting := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if waiting {
				a.logger.Info("catalog builder lock acquired")
			}
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		if !waiting {
			a.logger.Info("waiting for another process to finish building catalogs")
			waiting = true
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// requeueBuilds restarts builds that a shutdown or crash cut off. Only call it
// while holding the builder lock.
func (a *app) requeueBuilds() {
	err := a.db.Model(&Catalog{}).Where("status = ?", catalogProcessing).Update("status", catalogPending).Error
	if err != nil {
		a.logger.Error("requeue catalog builds", "err", err)
	}
}

// wakeBuilder tells the builder there is new work, without blocking.
func (a *app) wakeBuilder() {
	select {
	case a.buildWake <- struct{}{}:
	default:
	}
}

// buildNext builds the oldest pending catalog. It returns false when there is
// nothing to do, or when the database can't be updated, so the builder never
// spins on the same catalog.
func (a *app) buildNext(ctx context.Context) bool {
	var c Catalog
	err := a.db.Where("status = ? AND building_version IS NOT NULL", catalogPending).Order("updated_at, id").First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false
	}
	if err != nil {
		a.logger.Error("find pending catalog", "err", err)
		return false
	}
	version := *c.BuildingVersion
	res := a.db.Model(&Catalog{}).Where("id = ? AND status = ? AND building_version = ?", c.ID, catalogPending, version).
		Updates(map[string]any{"status": catalogProcessing, "updated_at": a.now()})
	if res.Error != nil {
		a.logger.Error("start catalog build", "catalog_id", c.ID, "err", res.Error)
		return false
	}
	if res.RowsAffected == 0 {
		return true // changed under us; look again
	}

	start := time.Now()
	result, err := a.buildVersion(ctx, c, version)
	if ctx.Err() != nil {
		return false // shutting down; runBuilder requeues it on the next start
	}
	if err != nil {
		a.logger.Error("catalog build", "catalog_id", c.ID, "version", version, "err", err)
		result.Errors = []csvError{{Type: "build_error", Detail: buildErrorMessage}}
	}
	if err := a.finishBuild(ctx, c, version, result); err != nil {
		a.logger.Error("finish catalog build", "catalog_id", c.ID, "version", version, "err", err)
		return false
	}
	a.logger.Info("catalog build", "catalog_id", c.ID, "version", version, "ok", len(result.Errors) == 0,
		"rows", result.RowCount, "songs", result.SongCount, "encoding", result.SourceEncoding, "ms", time.Since(start).Milliseconds())
	return true
}

// buildVersion turns the uploaded CSV into a catalog database in the cache
// directory and stores a copy in the blob store.
func (a *app) buildVersion(ctx context.Context, c Catalog, version int) (importResult, error) {
	csvPath, err := a.fetchToTemp(ctx, csvBlobKey(c.ID, version))
	if err != nil {
		return importResult{}, err
	}
	defer os.Remove(csvPath)

	limit, err := a.rowLimitFor(c)
	if err != nil {
		return importResult{}, err
	}
	dbPath := a.cache.path(c.ID, version)
	res, err := importCSV(ctx, csvPath, dbPath, limit)
	if err != nil || len(res.Errors) > 0 {
		return res, err
	}
	f, err := os.Open(dbPath)
	if err != nil {
		return res, err
	}
	defer f.Close()
	if err := a.blobs.put(ctx, dbBlobKey(c.ID, version), f); err != nil {
		os.Remove(dbPath)
		return res, err
	}
	return res, nil
}

func (a *app) fetchToTemp(ctx context.Context, key string) (string, error) {
	body, err := a.blobs.get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", key, err)
	}
	defer body.Close()
	tmp, err := os.CreateTemp(a.cfg.CatalogCacheDir, "upload-*.tmp")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmp, body); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), tmp.Close()
}

// rowLimitFor is whichever is tighter: the per-catalog limit, or what the
// account has left after its other catalogs.
func (a *app) rowLimitFor(c Catalog) (rowLimit, error) {
	var others int
	err := a.db.Model(&Catalog{}).Where("user_id = ? AND id <> ?", c.UserID, c.ID).
		Select("COALESCE(SUM(row_count), 0)").Scan(&others).Error
	if err != nil {
		return rowLimit{}, err
	}
	if room := maxRowsPerAccount - others; room < maxRowsPerCatalog {
		return rowLimit{max: max(room, 0), detail: fmt.Sprintf(
			"This file would put your account over %s rows. Your other catalogs have %s.", commas(maxRowsPerAccount), commas(others))}, nil
	}
	return rowLimit{max: maxRowsPerCatalog, detail: fmt.Sprintf(
		"This file has more than %s rows, the most one catalog can hold.", commas(maxRowsPerCatalog))}, nil
}

// removeCatalogData deletes a catalog's files after its row is gone. A failure
// only leaves orphaned files behind, so it is logged, not returned.
func (a *app) removeCatalogData(ctx context.Context, catalogID uint) {
	a.cache.removeCatalog(catalogID)
	if err := a.blobs.deletePrefix(ctx, catalogBlobPrefix(catalogID)); err != nil {
		a.logger.Error("delete catalog blobs", "catalog_id", catalogID, "err", err)
	}
}

// finishBuild records the outcome. The catalog is only changed if it is still
// waiting for this version: a newer upload or a delete during the build wins.
func (a *app) finishBuild(ctx context.Context, c Catalog, version int, res importResult) error {
	ok := len(res.Errors) == 0
	var dbBytes int64
	if ok {
		if info, err := os.Stat(a.cache.path(c.ID, version)); err == nil {
			dbBytes = info.Size()
		}
	}

	var applied bool
	err := a.db.Transaction(func(tx *gorm.DB) error {
		catalog := map[string]any{"status": catalogFailed, "building_version": nil, "updated_at": a.now()}
		versionRow := map[string]any{"status": catalogFailed, "source_encoding": res.SourceEncoding}
		if ok {
			meta, err := json.Marshal(importMetadata{res.Replacement, res.SourceEncoding, "UTF-8"})
			if err != nil {
				return err
			}
			var sha string
			if err := tx.Model(&CatalogVersion{}).Where("catalog_id = ? AND version = ?", c.ID, version).
				Pluck("csv_sha256", &sha).Error; err != nil {
				return err
			}
			catalog["status"] = catalogReady
			catalog["active_version"] = version
			catalog["song_count"] = res.SongCount
			catalog["row_count"] = res.RowCount
			catalog["errors_json"] = ""
			catalog["import_metadata_json"] = string(meta)
			catalog["last_csv_sha256"] = sha
			versionRow["status"] = catalogReady
			versionRow["row_count"] = res.RowCount
			versionRow["song_count"] = res.SongCount
			versionRow["db_bytes"] = dbBytes
		} else {
			errs, err := json.Marshal(res.Errors)
			if err != nil {
				return err
			}
			catalog["errors_json"] = string(errs)
		}
		if err := tx.Model(&CatalogVersion{}).Where("catalog_id = ? AND version = ?", c.ID, version).Updates(versionRow).Error; err != nil {
			return err
		}
		r := tx.Model(&Catalog{}).Where("id = ? AND building_version = ?", c.ID, version).Updates(catalog)
		applied = r.RowsAffected > 0
		return r.Error
	})
	if err != nil {
		if ok {
			a.cache.remove(c.ID, version)
		}
		return err
	}

	if !ok {
		return nil
	}
	if !applied {
		a.cache.remove(c.ID, version)
		var n int64
		if a.db.Model(&Catalog{}).Where("id = ?", c.ID).Count(&n); n == 0 {
			// Deleted during the build, after its blobs were removed.
			a.removeCatalogData(ctx, c.ID)
		}
		return nil
	}
	if err := a.cache.add(c.ID, version); err != nil {
		a.logger.Error("catalog cache add", "catalog_id", c.ID, "err", err)
	}
	if c.ActiveVersion != nil && *c.ActiveVersion != version {
		a.cache.remove(c.ID, *c.ActiveVersion)
	}
	return nil
}
