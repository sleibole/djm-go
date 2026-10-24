package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

type sentMail struct {
	To, Subject, Body string
}

type testMailer struct {
	mu   sync.Mutex
	sent []sentMail
}

func (m *testMailer) Send(to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{to, subject, body})
	return nil
}

func (m *testMailer) all() []sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sentMail(nil), m.sent...)
}

// newTestApp uses a temp database, records email instead of sending it, and
// answers Turnstile locally: every token passes except "bad".
func newTestApp(t *testing.T) *app {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	dir := t.TempDir()
	cfg := config{
		Env: "dev", BaseURL: "http://example.test",
		CatalogCacheDir: filepath.Join(dir, "catalogs"), CatalogCacheMax: defaultCatalogCacheMax, BlobDir: filepath.Join(dir, "blobs"),
	}
	a, err := newApp(cfg, db, slog.New(slog.NewTextHandler(io.Discard, nil)), &testMailer{})
	if err != nil {
		t.Fatal(err)
	}
	siteverify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		fmt.Fprintf(w, `{"success":%t}`, r.PostForm.Get("response") != "bad")
	}))
	t.Cleanup(siteverify.Close)
	a.turnstile.endpoint = siteverify.URL
	a.turnstile.client = siteverify.Client()
	return a
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	a := newTestApp(t)
	rec := get(t, a.routes(), "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("healthz = %d %q", rec.Code, rec.Body.String())
	}
}

func TestHealthzDatabaseDown(t *testing.T) {
	a := newTestApp(t)
	sqlDB, err := a.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.Close()
	rec := get(t, a.routes(), "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sql") {
		t.Fatalf("body leaks the error: %q", rec.Body.String())
	}
}

func TestHome(t *testing.T) {
	a := newTestApp(t)
	rec := get(t, a.routes(), "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Make the queue visible. The questions stop.",
		`<meta property="og:image" content="http://example.test/static/images/brand/open_graph_1200_630.png?v=`,
		`<meta property="og:url" content="http://example.test/">`,
		`href="/static/pico.min.css?v=`,
		`src="/static/images/landing/queue/singer_view.png?v=`,
		`data-theme-choice="dark"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home page is missing %q", want)
		}
	}
}

func TestHomeHTMXRendersContentOnly(t *testing.T) {
	a := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, "<html") || !strings.Contains(body, "Make the queue visible") {
		t.Fatalf("HTMX response should be the content block only:\n%s", body)
	}
}

func TestHomeBoostedRendersLayout(t *testing.T) {
	a := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Boosted", "true")
	req.Header.Set("HX-Target", "body")
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "<header") || !strings.Contains(body, "<title>") || !strings.Contains(body, "Make the queue visible") {
		t.Fatalf("boosted response should be a full document:\n%s", body)
	}
}

func TestHomeHistoryRestoreRendersLayout(t *testing.T) {
	a := newTestApp(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-History-Restore-Request", "true")
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "<header") || !strings.Contains(body, "<title>") || strings.Contains(body, "<html") == false {
		t.Fatalf("history restore should be a full document:\n%s", body)
	}
}

func TestNotFound(t *testing.T) {
	a := newTestApp(t)
	rec := get(t, a.routes(), "/nope")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Not found") {
		t.Fatalf("got %d:\n%s", rec.Code, rec.Body.String())
	}
}

func TestStaticCaching(t *testing.T) {
	a := newTestApp(t)
	h := a.routes()
	versioned, err := a.static.url("app.css")
	if err != nil {
		t.Fatal(err)
	}

	rec := get(t, h, versioned)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("versioned: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	rec = get(t, h, "/static/app.css?v=stale")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("stale version: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if rec := get(t, h, "/static/"); rec.Code != http.StatusNotFound {
		t.Fatalf("directory listing: %d", rec.Code)
	}
	if rec := get(t, h, "/static/images/"); rec.Code != http.StatusNotFound {
		t.Fatalf("directory listing: %d", rec.Code)
	}
	rec = get(t, h, "/static/manifest.webmanifest")
	if got := rec.Header().Get("Content-Type"); got != "application/manifest+json" {
		t.Fatalf("manifest content type = %q", got)
	}
	if rec := get(t, h, "/favicon.ico"); rec.Code != http.StatusOK {
		t.Fatalf("favicon: %d", rec.Code)
	}
}

func TestEveryAssetLinkResolves(t *testing.T) {
	a := newTestApp(t)
	h := a.routes()
	body := get(t, h, "/").Body.String()
	links := regexp.MustCompile(`/static/[^"?]+\?v=[0-9a-f]+`).FindAllString(body, -1)
	if len(links) < 8 {
		t.Fatalf("expected the layout and home page to link several assets, found %d", len(links))
	}
	for _, link := range links {
		if rec := get(t, h, link); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", link, rec.Code)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	a := newTestApp(t)
	rec := get(t, a.routes(), "/")
	for name, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "frame-ancestors 'none'",
		"Referrer-Policy":         "same-origin",
	} {
		if got := rec.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	a := newTestApp(t)
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	a.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
}

// The catalog databases in phase 2 depend on these SQLite features being compiled
// into the pure-Go driver.
func TestSQLiteHasFTS5AndJSON(t *testing.T) {
	a := newTestApp(t)
	db := a.db
	steps := []string{
		`CREATE TABLE songs (id INTEGER PRIMARY KEY, title TEXT, artist TEXT, versions_text TEXT)`,
		`CREATE VIRTUAL TABLE songs_fts USING fts5(title, artist, versions_text, content=songs, content_rowid=id, tokenize="unicode61 remove_diacritics 2", prefix='2 3')`,
		`INSERT INTO songs (title, artist, versions_text) VALUES ('Crazy in Love', 'Beyoncé', 'Duet Key +2')`,
		`INSERT INTO songs_fts(songs_fts) VALUES('rebuild')`,
	}
	for _, s := range steps {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	var n int
	if err := db.Raw(`SELECT COUNT(*) FROM songs_fts WHERE songs_fts MATCH ?`, "beyonce* cra*").Scan(&n).Error; err != nil || n != 1 {
		t.Fatalf("diacritic prefix match: n=%d err=%v", n, err)
	}
	var rank float64
	if err := db.Raw(`SELECT bm25(songs_fts, 10.0, 5.0, 1.0) FROM songs_fts WHERE songs_fts MATCH ?`, "duet*").Scan(&rank).Error; err != nil {
		t.Fatalf("bm25: %v", err)
	}
	var arr string
	if err := db.Raw(`SELECT json_group_array(artist) FROM songs`).Scan(&arr).Error; err != nil || arr != `["Beyoncé"]` {
		t.Fatalf("json_group_array = %q err=%v", arr, err)
	}
}
