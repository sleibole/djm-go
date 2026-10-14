package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestBigCatalog builds a 1,000,000-row catalog and times searches on it. It
// takes a while, so it only runs with DJM_BIG=1. DJM_BIG_DB reuses a built file.
func TestBigCatalog(t *testing.T) {
	if os.Getenv("DJM_BIG") == "" {
		t.Skip("set DJM_BIG=1")
	}
	dbPath := os.Getenv("DJM_BIG_DB")
	if dbPath == "" {
		dbPath = buildBigCatalog(t)
	}

	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	searchCatalog(db, "warm", 50, 0, "") // first query opens the file
	timed := func(label string, f func() error) {
		start := time.Now()
		if err := f(); err != nil {
			t.Fatal(err)
		}
		t.Logf("%-40s %s", label, time.Since(start).Round(time.Microsecond))
	}
	for _, q := range []string{"queen", "love night", "artist 42", "l", "fire dance 123", "key"} {
		for _, sort := range []string{"best_match", "title"} {
			timed(fmt.Sprintf("search %q %s", q, sort), func() error {
				_, err := searchCatalog(db, q, 50, 0, sort)
				return err
			})
		}
		match := ftsQuery(q)
		timed(fmt.Sprintf("  count %q", q), func() error {
			var n int
			return db.QueryRow(`SELECT COUNT(*) FROM songs_fts WHERE songs_fts MATCH ?`, match).Scan(&n)
		})
		timed(fmt.Sprintf("  sort by title, no bm25 %q", q), func() error {
			return db.QueryRow(`SELECT songs.id FROM songs_fts JOIN songs ON songs.id = songs_fts.rowid
				WHERE songs_fts MATCH ? ORDER BY songs.title COLLATE NOCASE, songs.artist COLLATE NOCASE, songs.id LIMIT 1`, match).Scan(new(int))
		})
	}
}

func buildBigCatalog(t *testing.T) string {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "big.csv")
	f, err := os.Create(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	w := bufio.NewWriter(f)
	fmt.Fprintln(w, "title,artist,version,album,id")
	words := []string{"love", "night", "heart", "fire", "dance", "dream", "rain", "summer", "road", "light", "queen", "river"}
	for i := range maxRowsPerCatalog {
		fmt.Fprintf(w, "%s %s %d,Artist %d,Key +%d,Album %d,ID-%d\n",
			words[i%len(words)], words[(i/7)%len(words)], i/3, i%5000, i%3, i%900, i)
	}
	w.Flush()
	f.Close()
	info, _ := os.Stat(csvPath)

	var peak uint64
	stop := make(chan struct{})
	go func() {
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				runtime.ReadMemStats(&m)
				peak = max(peak, m.HeapInuse)
			}
		}
	}()
	start := time.Now()
	dbPath := filepath.Join(dir, "big.db")
	res, err := importCSV(context.Background(), csvPath, dbPath, noRowLimit)
	close(stop)
	if err != nil || len(res.Errors) > 0 {
		t.Fatal(err, res.Errors)
	}
	dbInfo, _ := os.Stat(dbPath)
	t.Logf("csv %d MB, %d rows, %d songs, build %s, peak heap %d MB, db %d MB",
		info.Size()>>20, res.RowCount, res.SongCount, time.Since(start).Round(time.Millisecond), peak>>20, dbInfo.Size()>>20)
	if keep := os.Getenv("DJM_BIG_KEEP"); keep != "" {
		data, _ := os.ReadFile(dbPath)
		os.WriteFile(keep, data, 0o644)
	}
	return dbPath
}
