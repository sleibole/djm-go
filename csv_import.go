package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	_ "github.com/glebarez/go-sqlite"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/transform"
)

// The rules here are the Rails songs app's (catalog_csv_encoding.rb,
// csv_validator.rb, catalog_db_builder.rb); PLAN.md lists them and
// testdata/golden.rb checks them against the Rails code.

const (
	maxRowErrors         = 20
	replacementCharCap   = 10_000
	maxRowsPerCatalog    = 1_000_000
	encodingErrorMessage = "We couldn't read this CSV file. Please upload a UTF-8 CSV, or try exporting the song list again from the original source."
)

type csvError struct {
	Type   string `json:"type"`
	Row    int    `json:"row,omitempty"`
	Column any    `json:"column,omitempty"` // 1-based position for empty_header, a name otherwise
	Detail string `json:"detail"`
}

type replacementStats struct {
	HasReplacementCharacters  bool `json:"has_replacement_characters"`
	ReplacementCharacterCount int  `json:"replacement_character_count"`
	ReplacementRowCount       int  `json:"replacement_row_count"`
}

type importResult struct {
	SourceEncoding string
	Errors         []csvError
	Replacement    replacementStats
	RowCount       int // data rows in the CSV
	SongCount      int // songs after grouping
}

// rowLimit caps the data rows in one file. The detail is the message shown when
// the cap is passed, since it depends on which limit is the tighter one.
type rowLimit struct {
	max    int
	detail string
}

// importCSV checks the CSV at csvPath and, if it passes, builds the catalog
// database at dbPath. A file that fails the rules comes back with Errors and no
// database; the error return is for everything else (disk, SQLite).
func importCSV(ctx context.Context, csvPath, dbPath string, limit rowLimit) (importResult, error) {
	f, err := os.Open(csvPath)
	if err != nil {
		return importResult{}, err
	}
	defer f.Close()

	enc, bom, err := sniffCSV(f)
	if err != nil {
		return importResult{}, err
	}
	res := importResult{SourceEncoding: enc}
	if _, err := f.Seek(bom, io.SeekStart); err != nil {
		return res, err
	}
	var src io.Reader = f
	switch enc {
	case "Windows-1252":
		src = transform.NewReader(f, charmap.Windows1252.NewDecoder())
	case "ISO-8859-1":
		src = transform.NewReader(f, charmap.ISO8859_1.NewDecoder())
	}
	r := newCSVReader(src)

	fail := func(errs ...csvError) (importResult, error) {
		res.Errors = errs
		return res, nil
	}
	// read returns nil at the end of the file. A parse error is a rule failure,
	// not an error return.
	var parseErr *csvParseError
	read := func() ([]csvField, error) {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return rec, err
	}

	header, err := read()
	var first []csvField
	if err == nil && header != nil {
		first, err = read()
	}
	if errors.As(err, &parseErr) {
		return fail(invalidCSV(err))
	} else if err != nil {
		return res, err
	}
	if header == nil || allNull(header) {
		return fail(csvError{Type: "no_headers", Detail: "CSV has no header row"})
	}
	if first == nil {
		return fail(csvError{Type: "no_rows", Detail: "CSV has no songs"})
	}

	cols, errs := checkHeaders(header)
	if len(errs) > 0 {
		return fail(errs...)
	}

	tmp := dbPath + ".tmp"
	os.Remove(tmp)
	b, err := newCatalogBuilder(tmp)
	if err != nil {
		return res, err
	}
	built := false
	defer func() {
		if !built {
			b.close()
			os.Remove(tmp)
		}
	}()

	var tally replacementTally
	tally.add(header)

	// Rows are inserted until the first error; after that they are only checked,
	// and the database is thrown away.
	checkRow := func(rec []csvField, line int) error {
		tally.add(rec)
		title, artist := rubyStrip(cols.get(rec, cols.title)), rubyStrip(cols.get(rec, cols.artist))
		if title == "" {
			errs = append(errs, csvError{Type: "blank_field", Row: line, Column: "title", Detail: fmt.Sprintf("Title is blank on row %d", line)})
		}
		if artist == "" {
			errs = append(errs, csvError{Type: "blank_field", Row: line, Column: "artist", Detail: fmt.Sprintf("Artist is blank on row %d", line)})
		}
		if len(errs) > 0 {
			return nil
		}
		return b.add(title, artist, optional(cols.get(rec, cols.album)), optional(cols.get(rec, cols.version)), optional(cols.get(rec, cols.id)))
	}

	// As in CsvValidator, the error cap is checked after the next row is read
	// and numbered, so a parse error in that row still counts.
	line := 2
	for rec := first; ; {
		res.RowCount++
		if res.RowCount%4096 == 0 && ctx.Err() != nil {
			return res, ctx.Err()
		}
		if res.RowCount > limit.max {
			return fail(csvError{Type: "too_many_rows", Detail: limit.detail})
		}
		if err := checkRow(rec, line); err != nil {
			return res, err
		}
		if rec, err = read(); errors.As(err, &parseErr) {
			return fail(invalidCSV(err))
		} else if err != nil {
			return res, err
		}
		line++
		if rec == nil || len(errs) >= maxRowErrors {
			break
		}
	}
	if len(errs) > 0 {
		return fail(errs...)
	}

	res.Replacement = tally.stats()
	if res.SongCount, err = b.finish(); err != nil {
		return res, err
	}
	built = true
	if err := os.Rename(tmp, dbPath); err != nil {
		os.Remove(tmp)
		return res, err
	}
	return res, nil
}

func invalidCSV(err error) csvError {
	return csvError{Type: "parse_error", Detail: "Invalid CSV: " + err.Error()}
}

// sniffCSV reads the whole file once to pick the encoding.
//
// UTF-8 (after an optional BOM) is read as is. Anything else is a Windows
// "ANSI" export and decodes as Windows-1252, unless the file has one of the
// five bytes Windows-1252 leaves undefined, in which case it decodes as
// ISO-8859-1. Rails only looked for those bytes in the first 64 KB and crashed
// on one past that; checking the whole file avoids the crash.
func sniffCSV(f io.Reader) (encoding string, bom int64, err error) {
	buf := make([]byte, 64*1024)
	var carry []byte
	validUTF8, undefined1252, first := true, false, true
	for {
		n, rerr := f.Read(buf)
		chunk := buf[:n]
		if first && n > 0 {
			first = false
			if n >= 3 && string(chunk[:3]) == "\xEF\xBB\xBF" {
				bom, chunk = 3, chunk[3:]
			}
		}
		for _, c := range chunk {
			if c == 0x81 || c == 0x8D || c == 0x8F || c == 0x90 || c == 0x9D {
				undefined1252 = true
				break
			}
		}
		if validUTF8 {
			data := append(carry, chunk...)
			end := completeRunes(data)
			validUTF8 = utf8.Valid(data[:end])
			carry = append([]byte(nil), data[end:]...)
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", 0, rerr
		}
	}
	switch {
	case validUTF8 && len(carry) == 0 && bom > 0:
		encoding = "UTF-8-BOM"
	case validUTF8 && len(carry) == 0:
		encoding = "UTF-8"
	case undefined1252:
		encoding = "ISO-8859-1"
	default:
		encoding = "Windows-1252"
	}
	return encoding, bom, nil
}

// completeRunes returns the length of p without a multibyte sequence cut off
// at the end, so a character split across two reads is checked whole.
func completeRunes(p []byte) int {
	for i := len(p) - 1; i >= 0 && i >= len(p)-utf8.UTFMax+1; i-- {
		if utf8.RuneStart(p[i]) {
			if !utf8.FullRune(p[i:]) {
				return i
			}
			break
		}
	}
	return len(p)
}

type csvColumns struct {
	title, artist, version, album, id int // -1 when absent
}

func (c csvColumns) get(rec []csvField, i int) string {
	if i < 0 || i >= len(rec) {
		return ""
	}
	return rec[i].value
}

// checkHeaders applies the header rules. An unquoted empty cell has no name at
// all (Ruby's nil) and is left out of the duplicate check; a quoted or
// whitespace-only one is named "" and is not.
func checkHeaders(header []csvField) (csvColumns, []csvError) {
	var errs []csvError
	names := make([]string, len(header))
	counts := map[string]int{}
	var order []string
	for i, h := range header {
		if h.null {
			errs = append(errs, csvError{Type: "empty_header", Column: i + 1, Detail: fmt.Sprintf("Header cell %d is empty", i+1)})
			continue
		}
		names[i] = rubyDowncase(rubyStrip(h.value))
		if names[i] == "" {
			errs = append(errs, csvError{Type: "empty_header", Column: i + 1, Detail: fmt.Sprintf("Header cell %d is empty", i+1)})
		}
		if counts[names[i]] == 0 {
			order = append(order, names[i])
		}
		counts[names[i]]++
	}
	for _, name := range order {
		if counts[name] > 1 {
			errs = append(errs, csvError{Type: "duplicate_header", Column: name, Detail: fmt.Sprintf("Duplicate header '%s'", name)})
		}
	}

	index := func(name string) int {
		for i, n := range names {
			if n == name {
				return i
			}
		}
		return -1
	}
	cols := csvColumns{index("title"), index("artist"), index("version"), index("album"), index("id")}
	if cols.title < 0 {
		errs = append(errs, csvError{Type: "missing_header", Detail: "Required header 'title' is missing"})
	}
	if cols.artist < 0 {
		errs = append(errs, csvError{Type: "missing_header", Detail: "Required header 'artist' is missing"})
	}
	return cols, errs
}

func allNull(rec []csvField) bool {
	for _, f := range rec {
		if !f.null {
			return false
		}
	}
	return true
}

// replacementTally counts U+FFFD, which in a UTF-8 file means the text was
// damaged before it reached us. Rows are always counted; characters stop at
// replacementCharCap.
type replacementTally struct {
	chars, rows int
}

func (t *replacementTally) add(rec []csvField) {
	hit := false
	for _, f := range rec {
		if n := strings.Count(f.value, "\uFFFD"); n > 0 {
			hit = true
			t.chars = min(t.chars+n, replacementCharCap)
		}
	}
	if hit {
		t.rows++
	}
}

func (t *replacementTally) stats() replacementStats {
	return replacementStats{t.chars > 0 || t.rows > 0, t.chars, t.rows}
}

// rubyStrip trims what Ruby's String#strip does: ASCII whitespace and NUL,
// but not other Unicode spaces.
func rubyStrip(s string) string { return strings.Trim(s, "\x00\t\n\v\f\r ") }

// rubyDowncase is Ruby's String#downcase. Go's lowercasing is rune by rune,
// which differs only for U+0130, whose full lowercase is two code points.
func rubyDowncase(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "İ", "i\u0307"))
}

// optional is a stripped optional field, or nil when it is blank in the
// ActiveSupport sense (Unicode whitespace only).
func optional(s string) any {
	s = rubyStrip(s)
	if strings.TrimFunc(s, unicode.IsSpace) == "" {
		return nil
	}
	return s
}

// catalogBuilder writes one catalog database: rows go into raw_songs, then
// finish groups them into songs and fills the FTS index.
type catalogBuilder struct {
	db   *sql.DB
	tx   *sql.Tx
	stmt *sql.Stmt
}

const catalogSchema = `
CREATE TABLE raw_songs (
  title TEXT,
  artist TEXT,
  album TEXT,
  version TEXT,
  external_id TEXT,
  lower_title TEXT,
  lower_artist TEXT
);
CREATE TABLE songs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  title TEXT NOT NULL,
  artist TEXT NOT NULL,
  album TEXT,
  versions_json TEXT,
  external_ids_json TEXT,
  versions_text TEXT
);
CREATE VIRTUAL TABLE songs_fts USING fts5(
  title,
  artist,
  versions_text,
  content=songs,
  content_rowid=id,
  tokenize="unicode61 remove_diacritics 2",
  prefix='2 3'
);`

// Album, title and artist casing: MIN picks the first in byte order (MIN
// skips NULLs). Versions and IDs are deduplicated before being collected.
const catalogGroupSQL = `
INSERT INTO songs (title, artist, album, versions_json, external_ids_json, versions_text)
SELECT
  base.title,
  base.artist,
  base.album,
  v.versions_json,
  e.external_ids_json,
  v.versions_text
FROM (
  SELECT
    lower_title,
    lower_artist,
    MIN(title) AS title,
    MIN(artist) AS artist,
    MIN(album) AS album
  FROM raw_songs
  GROUP BY lower_title, lower_artist
) base
LEFT JOIN (
  SELECT
    lower_title,
    lower_artist,
    json_group_array(version) AS versions_json,
    GROUP_CONCAT(version, ' ') AS versions_text
  FROM (
    SELECT DISTINCT lower_title, lower_artist, version
    FROM raw_songs
    WHERE version IS NOT NULL
  )
  GROUP BY lower_title, lower_artist
) v ON base.lower_title = v.lower_title AND base.lower_artist = v.lower_artist
LEFT JOIN (
  SELECT
    lower_title,
    lower_artist,
    json_group_array(external_id) AS external_ids_json
  FROM (
    SELECT DISTINCT lower_title, lower_artist, external_id
    FROM raw_songs
    WHERE external_id IS NOT NULL
  )
  GROUP BY lower_title, lower_artist
) e ON base.lower_title = e.lower_title AND base.lower_artist = e.lower_artist`

func newCatalogBuilder(path string) (*catalogBuilder, error) {
	// The file is renamed into place only once it is complete, so durability
	// settings would only slow the build down.
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(OFF)&_pragma=synchronous(OFF)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	b := &catalogBuilder{db: db}
	if _, err := db.Exec(catalogSchema); err != nil {
		b.close()
		return nil, fmt.Errorf("catalog schema: %w", err)
	}
	if b.tx, err = db.Begin(); err != nil {
		b.close()
		return nil, err
	}
	b.stmt, err = b.tx.Prepare(`INSERT INTO raw_songs (title, artist, album, version, external_id, lower_title, lower_artist) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		b.close()
		return nil, err
	}
	return b, nil
}

func (b *catalogBuilder) add(title, artist string, album, version, id any) error {
	_, err := b.stmt.Exec(title, artist, album, version, id, rubyDowncase(title), rubyDowncase(artist))
	return err
}

func (b *catalogBuilder) finish() (songs int, err error) {
	defer b.close()
	b.stmt.Close()
	b.stmt = nil
	err = b.tx.Commit()
	b.tx = nil
	if err != nil {
		return 0, err
	}
	for _, q := range []string{
		`CREATE INDEX idx_raw_songs_group ON raw_songs(lower_title, lower_artist)`,
		catalogGroupSQL,
		`DROP TABLE raw_songs`,
		`INSERT INTO songs_fts(songs_fts) VALUES('rebuild')`,
	} {
		if _, err := b.db.Exec(q); err != nil {
			return 0, fmt.Errorf("build catalog: %w", err)
		}
	}
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM songs`).Scan(&songs); err != nil {
		return 0, err
	}
	var check string
	if err := b.db.QueryRow(`PRAGMA quick_check`).Scan(&check); err != nil {
		return 0, err
	}
	if check != "ok" {
		return 0, fmt.Errorf("catalog quick_check: %s", check)
	}
	return songs, nil
}

func (b *catalogBuilder) close() {
	if b.stmt != nil {
		b.stmt.Close()
	}
	if b.tx != nil {
		b.tx.Rollback()
	}
	b.db.Close()
}
