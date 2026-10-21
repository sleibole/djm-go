package main

import (
	"context"
	"time"
)

const (
	cleanupInterval = time.Hour
	// unconfirmedLifetime is how long an account whose email link was never used is kept.
	// Typos in the email form create these; they can't sign in, so they hold nothing.
	unconfirmedLifetime = 7 * 24 * time.Hour
	// oldVersionLifetime is how long a replaced or failed upload's files are kept.
	oldVersionLifetime = 48 * time.Hour
	// staleGuestLifetime is how long an audience profile without an account is
	// kept after its last visit. Their singers keep their names in show history.
	staleGuestLifetime = 14 * 24 * time.Hour
)

// cleanup runs in this process and returns when ctx is cancelled.
func (a *app) cleanup(ctx context.Context) {
	a.cleanupOnce()
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cleanupOnce()
		}
	}
}

func (a *app) cleanupOnce() {
	now := a.now()
	if err := a.db.Where("expires_at <= ?", now).Delete(&Session{}).Error; err != nil {
		a.logger.Error("cleanup sessions", "err", err)
	}
	if err := a.db.Where("consumed_at IS NOT NULL OR expires_at <= ?", now).Delete(&MagicToken{}).Error; err != nil {
		a.logger.Error("cleanup magic tokens", "err", err)
	}

	a.cleanupCatalogVersions(now)

	err := a.db.Where("user_id IS NULL AND COALESCE(last_seen_at, created_at) <= ?", now.Add(-staleGuestLifetime)).
		Delete(&AudienceProfile{}).Error
	if err != nil {
		a.logger.Error("cleanup stale guests", "err", err)
	}

	var stale []uint
	err = a.db.Model(&User{}).
		Where("email_confirmed_at IS NULL AND created_at <= ?", now.Add(-unconfirmedLifetime)).
		Pluck("id", &stale).Error
	if err != nil {
		a.logger.Error("cleanup unconfirmed users", "err", err)
		return
	}
	for _, id := range stale {
		if err := a.deleteUser(id); err != nil {
			a.logger.Error("cleanup unconfirmed user", "user_id", id, "err", err)
		}
	}
}

// cleanupCatalogVersions deletes the files of versions that are neither live
// nor building, once they are old enough.
func (a *app) cleanupCatalogVersions(now time.Time) {
	var old []CatalogVersion
	err := a.db.Raw(`
		SELECT v.* FROM catalog_versions v JOIN catalogs c ON c.id = v.catalog_id
		WHERE v.created_at <= ? AND v.version IS NOT c.active_version AND v.version IS NOT c.building_version`,
		now.Add(-oldVersionLifetime)).Scan(&old).Error
	if err != nil {
		a.logger.Error("cleanup catalog versions", "err", err)
		return
	}
	ctx := context.Background()
	for _, v := range old {
		a.cache.remove(v.CatalogID, v.Version)
		err := a.blobs.delete(ctx, csvBlobKey(v.CatalogID, v.Version))
		if err == nil {
			err = a.blobs.delete(ctx, dbBlobKey(v.CatalogID, v.Version))
		}
		if err == nil {
			err = a.db.Delete(&v).Error
		}
		if err != nil {
			a.logger.Error("cleanup catalog version", "catalog_id", v.CatalogID, "version", v.Version, "err", err)
		}
	}
}
