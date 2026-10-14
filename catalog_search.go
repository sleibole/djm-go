package main

import (
	"database/sql"
	"encoding/json"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Ported from the Rails songs app's catalog_search_service.rb.

const (
	searchDefaultLimit = 25
	searchMaxLimit     = 100
	searchMaxTokens    = 20
)

type catalogSong struct {
	ID          int64    `json:"id"`
	Title       string   `json:"title"`
	Artist      string   `json:"artist"`
	Album       *string  `json:"album"`
	Versions    []string `json:"versions"`
	ExternalIDs []string `json:"external_ids"`
}

type searchResult struct {
	Songs []catalogSong `json:"songs"`
	Total int           `json:"total"`
	Sort  string        `json:"sort"`
}

const searchSelect = `
SELECT songs.id, songs.title, songs.artist, songs.album, songs.versions_json, songs.external_ids_json`

const searchFrom = `
FROM songs_fts
JOIN songs ON songs.id = songs_fts.rowid
WHERE songs_fts MATCH ?`

// The ORDER BY strings are fixed per sort mode; nothing from the request goes
// into them.
//
// Rails ended each of these with bm25 rank and then id as tie-breakers. They
// can never break a tie: two songs whose title and artist are equal ignoring
// ASCII case would also be equal lowercased, and the build groups those into
// one song. Leaving bm25 out gives the same order and saves computing it for
// every match, which was most of the time on a large catalog.
var searchOrder = map[string]string{
	"title":  "songs.title COLLATE NOCASE ASC, songs.artist COLLATE NOCASE ASC",
	"artist": "songs.artist COLLATE NOCASE ASC, songs.title COLLATE NOCASE ASC",
	// The bucket puts songs by the artist being searched for first; within a
	// bucket results read like the catalog. SQLite's LOWER only folds ASCII.
	"best_match": "artist_match_bucket ASC, songs.title COLLATE NOCASE ASC, songs.artist COLLATE NOCASE ASC",
}

const artistBucket = `,
  CASE
    WHEN LOWER(songs.artist) = ? THEN 0
    WHEN LENGTH(?) > 0 AND substr(LOWER(songs.artist), 1, LENGTH(?)) = ? THEN 1
    WHEN instr(LOWER(songs.artist), ?) > 0 THEN 2
    ELSE 3
  END AS artist_match_bucket`

// searchSort maps the sort parameter to a mode; anything unknown is best_match.
func searchSort(s string) string {
	s = strings.ToLower(rubyStrip(s))
	if _, ok := searchOrder[s]; ok {
		return s
	}
	return "best_match"
}

func searchCatalog(db *sql.DB, query string, limit, offset int, sort string) (searchResult, error) {
	sort = searchSort(sort)
	res := searchResult{Songs: []catalogSong{}, Sort: sort}
	match := ftsQuery(query)
	if match == "" {
		return res, nil
	}
	limit = min(max(limit, 1), searchMaxLimit)
	offset = max(offset, 0)

	if err := db.QueryRow(`SELECT COUNT(*) FROM songs_fts WHERE songs_fts MATCH ?`, match).Scan(&res.Total); err != nil {
		return res, err
	}

	q := searchSelect
	var args []any
	if sort == "best_match" {
		norm := bucketQuery(query)
		q += artistBucket
		args = append(args, norm, norm, norm, norm, norm)
	}
	q += searchFrom + "\nORDER BY " + searchOrder[sort] + "\nLIMIT ? OFFSET ?"
	args = append(args, match, limit, offset)

	rows, err := db.Query(q, args...)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	for rows.Next() {
		var s catalogSong
		var versions, ids sql.NullString
		dest := []any{&s.ID, &s.Title, &s.Artist, &s.Album, &versions, &ids}
		if sort == "best_match" {
			var bucket int
			dest = append(dest, &bucket)
		}
		if err := rows.Scan(dest...); err != nil {
			return res, err
		}
		s.Versions, s.ExternalIDs = jsonList(versions.String), jsonList(ids.String)
		res.Songs = append(res.Songs, s)
	}
	return res, rows.Err()
}

func jsonList(s string) []string {
	out := []string{}
	if s != "" {
		json.Unmarshal([]byte(s), &out)
	}
	return out
}

// ftsQuery turns what someone typed into an FTS5 MATCH string: each run of
// characters FTS5 allows in a bareword (ASCII letters, digits, _, anything
// above U+007F) becomes a prefix term. Everything else separates terms, so
// punctuation can never reach the FTS5 query parser. Returns "" when nothing
// is left to search for.
func ftsQuery(raw string) string {
	if isBlank(raw) {
		return ""
	}
	var terms []string
	for _, piece := range strings.FieldsFunc(norm.NFC.String(raw), isRubySpace) {
		for _, term := range strings.FieldsFunc(piece, func(r rune) bool { return !isBareword(r) }) {
			if isBlank(term) {
				continue
			}
			switch strings.ToUpper(term) {
			case "AND", "OR", "NOT", "NEAR":
				continue
			}
			terms = append(terms, term+"*")
		}
	}
	if len(terms) > searchMaxTokens {
		terms = terms[:searchMaxTokens]
	}
	return strings.Join(terms, " ")
}

func isBareword(r rune) bool {
	return r > 127 || r == '_' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')
}

// isRubySpace is Ruby's regexp \s, which is ASCII only.
func isRubySpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r'
}

// isBlank is ActiveSupport's blank? for strings: empty or Unicode whitespace.
func isBlank(s string) bool { return strings.TrimFunc(s, unicode.IsSpace) == "" }

// bucketQuery is the raw query as the artist bucket compares it: trimmed,
// lowercased, whitespace runs collapsed to one space.
func bucketQuery(raw string) string {
	s := rubyDowncase(rubyStrip(norm.NFC.String(raw)))
	return strings.Join(strings.FieldsFunc(s, isRubySpace), " ")
}
