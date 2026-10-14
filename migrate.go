package main

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"gorm.io/gorm"
)

// Schema changes are numbered SQL files: migrations/001_name.sql, 002_name.sql, ...
// SQLite's user_version records the last one applied. Each file runs once, in order,
// in its own transaction. Files are never edited after they ship; a change is a new file.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

func migrate(db *gorm.DB) error {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return err
	}
	return runMigrations(db, sub)
}

// loadMigrations reads NNN_name.sql files and requires them to be numbered 1, 2, 3, ...
func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for i, name := range names { // Glob returns names sorted
		prefix, _, ok := strings.Cut(name, "_")
		version, err := strconv.Atoi(prefix)
		if !ok || err != nil {
			return nil, fmt.Errorf("migration %s: name must look like 001_name.sql", name)
		}
		if version != i+1 {
			return nil, fmt.Errorf("migration %s: expected number %d", name, i+1)
		}
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: version, name: path.Base(name), sql: string(b)})
	}
	return ms, nil
}

func runMigrations(db *gorm.DB, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	ctx := context.Background()
	// PRAGMAs apply per connection, so every step runs on this one.
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	var current int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	if current > len(ms) {
		return fmt.Errorf("database is at migration %d but this build only knows %d; run a newer build", current, len(ms))
	}
	if current == len(ms) {
		return nil
	}

	// Rebuilding a table (SQLite's way to change a column) drops and recreates it,
	// which foreign keys would block. They can't be switched inside a transaction,
	// so they are off for the whole run and checked before each commit instead.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, "PRAGMA foreign_keys = ON")

	for _, m := range ms[current:] {
		if err := applyMigration(ctx, conn, m); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *sql.Conn, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	broken := rows.Next()
	rows.Close()
	if broken {
		return fmt.Errorf("leaves rows that break a foreign key")
	}
	// user_version is part of the database file, so it commits with the migration.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}
