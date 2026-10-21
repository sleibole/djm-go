package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// catalogCache keeps built catalog databases on local disk. Every file can be
// fetched again from the blob store, so the directory can be deleted at any
// time; past maxBytes the least recently used files are removed.
type catalogCache struct {
	dir      string
	maxBytes int64
	blobs    blobStore
	logger   *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	files   map[string]*cachedCatalog // by file name
	loading map[string]chan struct{}  // downloads in progress
	pinned  map[uint]bool             // catalogs of live shows; never evicted
	total   int64
	hits    int64
	misses  int64
}

type cachedCatalog struct {
	size     int64
	lastUsed time.Time
	touched  time.Time // last mtime update, so use survives a restart
	db       *sql.DB   // opened on first search
}

func newCatalogCache(dir string, maxBytes int64, blobs blobStore, logger *slog.Logger, now func() time.Time) (*catalogCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	c := &catalogCache{
		dir: dir, maxBytes: maxBytes, blobs: blobs, logger: logger, now: now,
		files: map[string]*cachedCatalog{}, loading: map[string]chan struct{}{},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".db") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		c.files[name] = &cachedCatalog{size: info.Size(), lastUsed: info.ModTime(), touched: info.ModTime()}
		c.total += info.Size()
	}
	c.mu.Lock()
	c.evictLocked("")
	c.mu.Unlock()
	return c, nil
}

// removeStaleTemps deletes temp files left by a build, download, or upload
// that didn't finish. During a deploy two processes share this directory, so
// startup can't simply clear it: a file another process is still writing has
// a recent modification time, and only files untouched for olderThan go.
func (c *catalogCache) removeStaleTemps(olderThan time.Duration) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		c.logger.Error("catalog cache temp sweep", "err", err)
		return
	}
	cutoff := time.Now().Add(-olderThan)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(c.dir, e.Name()))
		}
	}
}

func catalogFileName(catalogID uint, version int) string {
	return fmt.Sprintf("%d_v%d.db", catalogID, version)
}

// path is where a version's database lives in the cache. The builder writes
// there directly and then calls add.
func (c *catalogCache) path(catalogID uint, version int) string {
	return filepath.Join(c.dir, catalogFileName(catalogID, version))
}

// open returns a read-only handle on a catalog version, downloading it first
// if it isn't on disk. Callers must not close it.
func (c *catalogCache) open(ctx context.Context, catalogID uint, version int) (*sql.DB, error) {
	name := catalogFileName(catalogID, version)
	for {
		c.mu.Lock()
		if f, ok := c.files[name]; ok {
			c.hits++
			db, err := c.useLocked(name, f)
			c.mu.Unlock()
			return db, err
		}
		if wait, busy := c.loading[name]; busy {
			c.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		c.misses++
		done := make(chan struct{})
		c.loading[name] = done
		c.mu.Unlock()

		size, err := c.download(ctx, dbBlobKey(catalogID, version), c.path(catalogID, version))

		c.mu.Lock()
		delete(c.loading, name)
		close(done)
		if err != nil {
			c.mu.Unlock()
			return nil, err
		}
		f := c.addLocked(name, size)
		db, err := c.useLocked(name, f)
		c.mu.Unlock()
		return db, err
	}
}

func (c *catalogCache) download(ctx context.Context, key, path string) (int64, error) {
	body, err := c.blobs.get(ctx, key)
	if err != nil {
		return 0, fmt.Errorf("fetch %s: %w", key, err)
	}
	defer body.Close()
	if err := writeFileAtomic(path, body); err != nil {
		return 0, fmt.Errorf("fetch %s: %w", key, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (c *catalogCache) useLocked(name string, f *cachedCatalog) (*sql.DB, error) {
	now := c.now()
	f.lastUsed = now
	if now.Sub(f.touched) > time.Minute {
		f.touched = now
		os.Chtimes(filepath.Join(c.dir, name), now, now)
	}
	if f.db == nil {
		// immutable: the file never changes once built, so SQLite can skip locking.
		db, err := sql.Open("sqlite", "file:"+filepath.Join(c.dir, name)+"?mode=ro&immutable=1")
		if err != nil {
			return nil, err
		}
		f.db = db
	}
	return f.db, nil
}

// add registers a database the builder just wrote at path(catalogID, version).
func (c *catalogCache) add(catalogID uint, version int) error {
	info, err := os.Stat(c.path(catalogID, version))
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addLocked(catalogFileName(catalogID, version), info.Size())
	return nil
}

func (c *catalogCache) addLocked(name string, size int64) *cachedCatalog {
	if old, ok := c.files[name]; ok {
		c.dropLocked(name, old)
	}
	now := c.now()
	f := &cachedCatalog{size: size, lastUsed: now, touched: now}
	c.files[name] = f
	c.total += size
	c.evictLocked(name)
	return f
}

// remove deletes one version from the cache, e.g. once a newer one is live.
func (c *catalogCache) remove(catalogID uint, version int) {
	name := catalogFileName(catalogID, version)
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.files[name]; ok {
		c.dropLocked(name, f)
	} else {
		os.Remove(filepath.Join(c.dir, name))
	}
}

// removeCatalog deletes every cached version of a catalog.
func (c *catalogCache) removeCatalog(catalogID uint) {
	prefix := fmt.Sprintf("%d_v", catalogID)
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, f := range c.files {
		if strings.HasPrefix(name, prefix) {
			c.dropLocked(name, f)
		}
	}
}

// setPinned replaces the set of catalogs that must stay cached.
func (c *catalogCache) setPinned(catalogIDs []uint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pinned = map[uint]bool{}
	for _, id := range catalogIDs {
		c.pinned[id] = true
	}
}

func (c *catalogCache) pinnedLocked(name string) bool {
	var id uint
	_, err := fmt.Sscanf(name, "%d_v", &id)
	return err == nil && c.pinned[id]
}

// evictLocked removes least recently used files until the cache fits, never
// the one named keep, which was just added or used, nor a live show's
// catalog. The cache can stay over budget if live shows need the space.
func (c *catalogCache) evictLocked(keep string) {
	for c.total > c.maxBytes {
		var oldest string
		for name, f := range c.files {
			if name != keep && !c.pinnedLocked(name) && (oldest == "" || f.lastUsed.Before(c.files[oldest].lastUsed)) {
				oldest = name
			}
		}
		if oldest == "" {
			return
		}
		c.logger.Info("catalog cache evict", "file", oldest, "bytes", c.files[oldest].size)
		c.dropLocked(oldest, c.files[oldest])
	}
}

// dropLocked closes and deletes a file. sql.DB.Close waits for running
// queries, so a search in progress finishes first.
func (c *catalogCache) dropLocked(name string, f *cachedCatalog) {
	if f.db != nil {
		f.db.Close()
	}
	os.Remove(filepath.Join(c.dir, name))
	delete(c.files, name)
	c.total -= f.size
}

type catalogCacheStats struct {
	Files, Hits, Misses int64
	Bytes, MaxBytes     int64
}

func (c *catalogCache) stats() catalogCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return catalogCacheStats{Files: int64(len(c.files)), Hits: c.hits, Misses: c.misses, Bytes: c.total, MaxBytes: c.maxBytes}
}
