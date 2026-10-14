package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Pragmas go in the DSN so they apply to every pooled connection, not just the first.
const sqlitePragmas = "?_pragma=journal_mode(WAL)" +
	"&_pragma=foreign_keys(ON)" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=synchronous(NORMAL)"

func openDB(path string) (*gorm.DB, error) {
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}

	db, err := gorm.Open(sqlite.Open(path+sqlitePragmas), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// One connection keeps writes in order. A pool of deferred transactions can
	// hit SQLITE_BUSY when one connection reads and another writes.
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("sql db: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// migrate creates or updates tables for every model. Add models here as phases introduce them.
func migrate(db *gorm.DB) error {
	return db.AutoMigrate()
}
