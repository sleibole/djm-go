package main

import (
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

// appModels is every GORM struct backed by a table. Add new models here.
var appModels = []any{&User{}, &Session{}, &MagicToken{}}

// TestMigrationsMatchModels catches a model and its migration drifting apart:
// every model field has a column, and every column has a field.
func TestMigrationsMatchModels(t *testing.T) {
	a := newTestApp(t)
	cache := &sync.Map{}
	for _, model := range appModels {
		s, err := schema.Parse(model, cache, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		var fields []string
		for _, f := range s.Fields {
			if f.DBName != "" {
				fields = append(fields, f.DBName)
			}
		}
		cols, err := a.db.Migrator().ColumnTypes(s.Table)
		if err != nil {
			t.Fatalf("%s: %v", s.Table, err)
		}
		var columns []string
		for _, c := range cols {
			columns = append(columns, c.Name())
		}
		slices.Sort(fields)
		slices.Sort(columns)
		if !slices.Equal(fields, columns) {
			t.Errorf("%s:\n model  %v\n schema %v", s.Table, fields, columns)
		}
	}
}

func openRawDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "m.db")+sqlitePragmas), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

func userVersion(t *testing.T, db *gorm.DB) int {
	t.Helper()
	var v int
	if err := db.Raw("PRAGMA user_version").Scan(&v).Error; err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMigrationsRunOnceInOrder(t *testing.T) {
	db := openRawDB(t)
	fsys := fstest.MapFS{
		"001_a.sql": {Data: []byte("CREATE TABLE a (id INTEGER PRIMARY KEY);")},
		"002_b.sql": {Data: []byte("CREATE TABLE b (id INTEGER PRIMARY KEY); INSERT INTO a DEFAULT VALUES;")},
	}
	for range 2 {
		if err := runMigrations(db, fsys); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	db.Raw("SELECT COUNT(*) FROM a").Scan(&n)
	if n != 1 || userVersion(t, db) != 2 {
		t.Fatalf("rows=%d version=%d; each migration should run once", n, userVersion(t, db))
	}

	fsys["003_c.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE c (id INTEGER PRIMARY KEY);")}
	if err := runMigrations(db, fsys); err != nil || userVersion(t, db) != 3 {
		t.Fatalf("new migration: err=%v version=%d", err, userVersion(t, db))
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	db := openRawDB(t)
	fsys := fstest.MapFS{
		"001_a.sql":   {Data: []byte("CREATE TABLE a (id INTEGER PRIMARY KEY);")},
		"002_bad.sql": {Data: []byte("CREATE TABLE b (id INTEGER PRIMARY KEY); NOT SQL;")},
	}
	err := runMigrations(db, fsys)
	if err == nil || !strings.Contains(err.Error(), "002_bad.sql") {
		t.Fatalf("err = %v", err)
	}
	if userVersion(t, db) != 1 || db.Migrator().HasTable("b") {
		t.Fatalf("version=%d, table b exists=%v; the failed migration should leave nothing", userVersion(t, db), db.Migrator().HasTable("b"))
	}
}

func TestMigrationCanRebuildATableWithForeignKeys(t *testing.T) {
	db := openRawDB(t)
	fsys := fstest.MapFS{
		"001_a.sql": {Data: []byte(`
			CREATE TABLE parents (id INTEGER PRIMARY KEY, name TEXT);
			CREATE TABLE kids (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parents (id) ON DELETE CASCADE);
			INSERT INTO parents (id, name) VALUES (1, 'p');
			INSERT INTO kids (parent_id) VALUES (1);`)},
		// The SQLite table-rebuild steps. With foreign keys on, DROP TABLE parents
		// would cascade and delete the kid.
		"002_rebuild.sql": {Data: []byte(`
			CREATE TABLE parents_new (id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT '');
			INSERT INTO parents_new SELECT id, COALESCE(name, '') FROM parents;
			DROP TABLE parents;
			ALTER TABLE parents_new RENAME TO parents;`)},
	}
	if err := runMigrations(db, fsys); err != nil {
		t.Fatal(err)
	}
	var kids int
	db.Raw("SELECT COUNT(*) FROM kids").Scan(&kids)
	if kids != 1 {
		t.Fatalf("kids = %d; the rebuild cascaded", kids)
	}
	var fk int
	db.Raw("PRAGMA foreign_keys").Scan(&fk)
	if fk != 1 {
		t.Fatal("foreign keys left off after migrating")
	}

	fsys["003_orphan.sql"] = &fstest.MapFile{Data: []byte("INSERT INTO kids (parent_id) VALUES (99);")}
	if err := runMigrations(db, fsys); err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("orphan row: err = %v", err)
	}
	if userVersion(t, db) != 2 {
		t.Fatalf("version = %d", userVersion(t, db))
	}
}

func TestMigrationNamesAndVersions(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"gap":         {"001_a.sql": {}, "003_c.sql": {}},
		"no number":   {"a.sql": {}},
		"starts at 2": {"002_b.sql": {}},
	} {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	db := openRawDB(t)
	db.Exec("PRAGMA user_version = 5")
	err := runMigrations(db, fstest.MapFS{"001_a.sql": {Data: []byte("SELECT 1;")}})
	if err == nil || !strings.Contains(err.Error(), "newer build") {
		t.Fatalf("database ahead of the build: err = %v", err)
	}
}

func TestDeletingUserCascades(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	u := signIn(t, a, b, "a@example.com", "")
	newBrowser(t, a).post("/signin/link", map[string][]string{"email": {"a@example.com"}})
	if err := a.deleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"sessions", "magic_tokens"} {
		var n int
		a.db.Raw("SELECT COUNT(*) FROM " + table).Scan(&n)
		if n != 0 {
			t.Errorf("%s: %d rows left", table, n)
		}
	}
}
