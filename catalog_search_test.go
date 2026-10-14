package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

// testdata/search/golden.json is what the Rails search service returned for
// each query in queries.json against catalog.csv.
func TestSearchMatchesRails(t *testing.T) {
	db := buildTestCatalog(t, "testdata/search/catalog.csv")

	var cases []struct {
		Q          string        `json:"q"`
		Limit      int           `json:"limit"`
		Offset     int           `json:"offset"`
		Sort       *string       `json:"sort"`
		Total      int           `json:"total"`
		ResultSort string        `json:"result_sort"`
		Songs      []catalogSong `json:"songs"`
	}
	readJSON(t, "testdata/search/golden.json", &cases)
	if len(cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range cases {
		sort := ""
		if c.Sort != nil {
			sort = *c.Sort
		}
		got, err := searchCatalog(db, c.Q, c.Limit, c.Offset, sort)
		if err != nil {
			t.Errorf("%q: %v", c.Q, err)
			continue
		}
		if got.Total != c.Total || got.Sort != c.ResultSort || !reflect.DeepEqual(got.Songs, c.Songs) {
			t.Errorf("q=%q limit=%d offset=%d sort=%q:\n got  total=%d sort=%s %v\n want total=%d sort=%s %v",
				c.Q, c.Limit, c.Offset, sort, got.Total, got.Sort, titles(got.Songs), c.Total, c.ResultSort, titles(c.Songs))
		}
	}
}

func titles(songs []catalogSong) []string {
	var out []string
	for _, s := range songs {
		out = append(out, s.Title+" / "+s.Artist)
	}
	return out
}

func buildTestCatalog(t *testing.T, csvPath string) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	res, err := importCSV(context.Background(), csvPath, dbPath, noRowLimit)
	if err != nil || len(res.Errors) > 0 {
		t.Fatal(err, res.Errors)
	}
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestFTSQuery(t *testing.T) {
	for in, want := range map[string]string{
		"":                "",
		"   ":             "",
		"\u00a0":          "",
		"!!!":             "",
		"AC/DC":           "AC* DC*",
		"Mr. B":           "Mr* B*",
		"not and OR near": "",
		"rock and roll":   "rock* roll*",
		"Beyonce\u0301":   "Beyoncé*",
	} {
		if got := ftsQuery(in); got != want {
			t.Errorf("ftsQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
