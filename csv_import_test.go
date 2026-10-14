package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var noRowLimit = rowLimit{max: maxRowsPerCatalog, detail: "too many rows"}

// Each testdata/csvs/*.json is what the Rails pipeline did with the CSV next
// to it; testdata/golden.rb regenerates them.
// changedFromRails lists fixtures whose result we changed on purpose.
var changedFromRails = map[string][]csvError{
	"edge_header_only": {{Type: "no_rows", Detail: "CSV has no songs"}},
}

func TestImportMixedLineEndings(t *testing.T) {
	csvPath := writeCSV(t, "title,artist,version\nHello,Adele,\r\nHello,Adele,Duet\rHalo,Beyoncé,\n")
	got, err := importCSV(context.Background(), csvPath, filepath.Join(t.TempDir(), "catalog.db"), noRowLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Errors) > 0 || got.RowCount != 3 || got.SongCount != 2 {
		t.Errorf("got errors %+v, %d rows, %d songs; want no errors, 3 rows, 2 songs", got.Errors, got.RowCount, got.SongCount)
	}
}

func TestImportMatchesRails(t *testing.T) {
	files, _ := filepath.Glob("testdata/csvs/*.csv")
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, csvPath := range files {
		name := strings.TrimSuffix(filepath.Base(csvPath), ".csv")
		t.Run(name, func(t *testing.T) {
			var want struct {
				Status         string           `json:"status"`
				SourceEncoding string           `json:"source_encoding"`
				Errors         json.RawMessage  `json:"errors"`
				SongCount      int              `json:"song_count"`
				Replacement    replacementStats `json:"replacement_stats"`
				Songs          []builtSong      `json:"songs"`
				Exception      string           `json:"exception"`
				Message        string           `json:"message"`
			}
			readJSON(t, strings.TrimSuffix(csvPath, ".csv")+".json", &want)

			dbPath := filepath.Join(t.TempDir(), "catalog.db")
			got, err := importCSV(context.Background(), csvPath, dbPath, noRowLimit)
			if err != nil {
				t.Fatal(err)
			}

			switch {
			case changedFromRails[name] != nil:
				if !reflect.DeepEqual(got.Errors, changedFromRails[name]) {
					t.Errorf("errors = %+v, want %+v", got.Errors, changedFromRails[name])
				}
			case want.Exception != "":
				// Rails let parse errors escape as a job failure; here they are a
				// parse_error with Ruby's message.
				wantErrs := []csvError{{Type: "parse_error", Detail: "Invalid CSV: " + want.Message}}
				if !reflect.DeepEqual(got.Errors, wantErrs) {
					t.Errorf("errors = %+v, want %+v", got.Errors, wantErrs)
				}
			case want.Status == "failed":
				if g, w := normalizeJSON(t, got.Errors), normalizeJSON(t, want.Errors); !reflect.DeepEqual(g, w) {
					t.Errorf("errors =\n %v\nwant\n %v", g, w)
				}
			default:
				if len(got.Errors) > 0 {
					t.Fatalf("unexpected errors: %+v", got.Errors)
				}
				if got.SongCount != want.SongCount || got.Replacement != want.Replacement {
					t.Errorf("songs %d stats %+v, want %d %+v", got.SongCount, got.Replacement, want.SongCount, want.Replacement)
				}
				if songs := readBuiltSongs(t, dbPath); !reflect.DeepEqual(songs, want.Songs) {
					t.Errorf("songs =\n %+v\nwant\n %+v", songs, want.Songs)
				}
			}
			if want.SourceEncoding != "" && got.SourceEncoding != want.SourceEncoding {
				t.Errorf("encoding = %q, want %q", got.SourceEncoding, want.SourceEncoding)
			}
			if want.Status == "failed" {
				if _, err := os.Stat(dbPath); err == nil {
					t.Error("failed import left a database")
				}
				if _, err := os.Stat(dbPath + ".tmp"); err == nil {
					t.Error("failed import left a temp file")
				}
			}
		})
	}
}

type builtSong struct {
	ID           int64    `json:"id"`
	Title        string   `json:"title"`
	Artist       string   `json:"artist"`
	Album        *string  `json:"album"`
	Versions     []string `json:"versions"`
	ExternalIDs  []string `json:"external_ids"`
	VersionsText *string  `json:"versions_text"`
}

func readBuiltSongs(t *testing.T, path string) []builtSong {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, title, artist, album, versions_json, external_ids_json, versions_text FROM songs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []builtSong
	for rows.Next() {
		var s builtSong
		var versions, ids *string
		if err := rows.Scan(&s.ID, &s.Title, &s.Artist, &s.Album, &versions, &ids, &s.VersionsText); err != nil {
			t.Fatal(err)
		}
		if versions != nil {
			json.Unmarshal([]byte(*versions), &s.Versions)
		}
		if ids != nil {
			json.Unmarshal([]byte(*ids), &s.ExternalIDs)
		}
		out = append(out, s)
	}
	return out
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// normalizeJSON round-trips v through JSON so Go structs and golden files
// compare as the same generic values.
func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	data, ok := v.(json.RawMessage)
	if !ok {
		var err error
		if data, err = json.Marshal(v); err != nil {
			t.Fatal(err)
		}
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func writeCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upload.csv")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestImportUTF8SplitAcrossReads(t *testing.T) {
	// Push "é" so its two bytes straddle the 64 KB read boundary.
	prefix := "title,artist\n"
	pad := strings.Repeat("a", 64*1024-len(prefix)-1)
	path := writeCSV(t, prefix+pad+"é,Artist\n")
	got, err := importCSV(context.Background(), path, filepath.Join(t.TempDir(), "c.db"), noRowLimit)
	if err != nil || len(got.Errors) > 0 {
		t.Fatal(err, got.Errors)
	}
	if got.SourceEncoding != "UTF-8" {
		t.Errorf("encoding = %q", got.SourceEncoding)
	}
}

func TestImportUndefined1252BytePastFirstChunk(t *testing.T) {
	// Rails only sampled the first 64 KB and crashed on a late undefined byte.
	content := "title,artist\nCaf\xe9,Band\n" + strings.Repeat("Song,Band\n", 8000) + "Odd\x81,Band\n"
	got, err := importCSV(context.Background(), writeCSV(t, content), filepath.Join(t.TempDir(), "c.db"), noRowLimit)
	if err != nil || len(got.Errors) > 0 {
		t.Fatal(err, got.Errors)
	}
	if got.SourceEncoding != "ISO-8859-1" {
		t.Errorf("encoding = %q", got.SourceEncoding)
	}
}

func TestImportRowLimit(t *testing.T) {
	content := "title,artist\n" + strings.Repeat("Song,Band\n", 5)
	limit := rowLimit{max: 4, detail: "Too many songs"}
	got, err := importCSV(context.Background(), writeCSV(t, content), filepath.Join(t.TempDir(), "c.db"), limit)
	if err != nil {
		t.Fatal(err)
	}
	want := []csvError{{Type: "too_many_rows", Detail: "Too many songs"}}
	if !reflect.DeepEqual(got.Errors, want) {
		t.Errorf("errors = %+v", got.Errors)
	}

	limit.max = 5
	got, err = importCSV(context.Background(), writeCSV(t, content), filepath.Join(t.TempDir(), "c.db"), limit)
	if err != nil || len(got.Errors) > 0 || got.RowCount != 5 || got.SongCount != 1 {
		t.Fatalf("at the limit: %v %+v", err, got)
	}
}

func TestImportReplacementCharacterCap(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("title,artist\n")
	for range 3 {
		b.WriteString(strings.Repeat("\uFFFD", 4000) + ",Band\n")
	}
	got, err := importCSV(context.Background(), writeCSV(t, b.String()), filepath.Join(t.TempDir(), "c.db"), noRowLimit)
	if err != nil || len(got.Errors) > 0 {
		t.Fatal(err, got.Errors)
	}
	want := replacementStats{true, replacementCharCap, 3}
	if got.Replacement != want {
		t.Errorf("stats = %+v, want %+v", got.Replacement, want)
	}
}
