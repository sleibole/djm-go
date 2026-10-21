package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func signInHost(t *testing.T, a *app, email string) (*browser, User) {
	t.Helper()
	b := newBrowser(t, a)
	return b, signIn(t, a, b, email, roleDJ)
}

func createCatalog(t *testing.T, a *app, b *browser, name string) Catalog {
	t.Helper()
	rec := b.post("/catalogs", url.Values{"name": {name}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create catalog: %d %s", rec.Code, rec.Body)
	}
	var c Catalog
	if err := a.db.Where("name = ?", name).Order("id DESC").First(&c).Error; err != nil {
		t.Fatal(err)
	}
	return c
}

func (b *browser) upload(target, filename string, body io.Reader) *httptest.ResponseRecorder {
	b.t.Helper()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", filename)
		if err == nil {
			_, err = io.Copy(part, body)
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	req := httptest.NewRequest(http.MethodPost, target, pr)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return b.do(req)
}

func uploadCSV(t *testing.T, b *browser, c Catalog, content string) *httptest.ResponseRecorder {
	t.Helper()
	return b.upload(fmt.Sprintf("/catalogs/%d/upload", c.ID), "songs.csv", strings.NewReader(content))
}

// build runs the builder until it has nothing left to do.
func build(t *testing.T, a *app) {
	t.Helper()
	for i := 0; a.buildNext(context.Background()); i++ {
		if i > 10 {
			t.Fatal("builder never finished")
		}
	}
}

func reload(t *testing.T, a *app, c Catalog) Catalog {
	t.Helper()
	var fresh Catalog
	if err := a.db.First(&fresh, c.ID).Error; err != nil {
		t.Fatal(err)
	}
	return fresh
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/csvs/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func search(b *browser, c Catalog, q string) string {
	b.t.Helper()
	return b.get(fmt.Sprintf("/catalogs/%d/search?q=%s", c.ID, url.QueryEscape(q))).Body.String()
}

func TestCatalogUploadBuildAndSearch(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Karaoke Night")

	if body := b.get(fmt.Sprintf("/catalogs/%d?saved=created", c.ID)).Body.String(); !strings.Contains(body, "Catalog created.") || !strings.Contains(body, "No songs yet") {
		t.Fatalf("new catalog page:\n%s", body)
	}

	rec := uploadCSV(t, b, c, fixture(t, "valid_kj_versions.csv"))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	if c = reload(t, a, c); c.Status != catalogPending || c.BuildingVersion == nil || *c.BuildingVersion != 1 {
		t.Fatalf("after upload: %+v", c)
	}
	if body := b.get(fmt.Sprintf("/catalogs/%d", c.ID)).Body.String(); !strings.Contains(body, "Processing…") {
		t.Fatalf("pending page:\n%s", body)
	}
	if rec := b.get(fmt.Sprintf("/catalogs/%d/status", c.ID)); rec.Code != http.StatusNoContent || rec.Header().Get("HX-Redirect") != "" {
		t.Fatalf("status while pending: %d %v", rec.Code, rec.Header())
	}

	build(t, a)
	c = reload(t, a, c)
	if c.Status != catalogReady || *c.ActiveVersion != 1 || c.BuildingVersion != nil || c.SongCount != 5 || c.RowCount != 14 {
		t.Fatalf("after build: %+v", c)
	}
	if m := c.ImportMetadata(); m.SourceEncoding != "UTF-8" || m.NormalizedEncoding != "UTF-8" || m.HasReplacementCharacters {
		t.Errorf("metadata: %+v", m)
	}
	if rec := b.get(fmt.Sprintf("/catalogs/%d/status", c.ID)); rec.Header().Get("HX-Redirect") != fmt.Sprintf("/catalogs/%d?built=1", c.ID) {
		t.Fatalf("status when done: %v", rec.Header())
	}
	if body := b.get(fmt.Sprintf("/catalogs/%d?built=1", c.ID)).Body.String(); !strings.Contains(body, "Catalog is ready!") || !strings.Contains(body, "5 songs") {
		t.Fatalf("ready page:\n%s", body)
	}

	body := search(b, c, "bohemian")
	if !strings.Contains(body, "1 result<") || !strings.Contains(body, "Bohemian Rhapsody") || strings.Contains(body, "<select") {
		t.Fatalf("search:\n%s", body)
	}
	if body := search(b, c, "zzz"); !strings.Contains(body, "No songs found.") {
		t.Fatalf("no match:\n%s", body)
	}
	if body := search(b, c, "   "); strings.Contains(body, "No songs found.") {
		t.Fatalf("blank query should show nothing:\n%s", body)
	}

	// The variant picker only appears once the catalog asks for it.
	b.post(fmt.Sprintf("/catalogs/%d", c.ID), url.Values{"variant_display": {"version"}})
	body = search(b, c, "bohemian")
	if !strings.Contains(body, "<th>Version</th>") || !strings.Contains(body, "<option>Key &#43;2</option>") {
		t.Fatalf("version picker:\n%s", body)
	}
	// Without JavaScript the page itself runs the search.
	if body := b.get(fmt.Sprintf("/catalogs/%d?q=journey", c.ID)).Body.String(); !strings.Contains(body, "Don&#39;t Stop Believin&#39;") {
		t.Fatalf("page search:\n%s", body)
	}
}

func TestCatalogFailedUploadKeepsOldVersion(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	uploadCSV(t, b, c, fixture(t, "valid_full.csv"))
	build(t, a)

	uploadCSV(t, b, c, fixture(t, "invalid_blank_fields.csv"))
	// While the new file waits, the old one still answers searches.
	if body := search(b, c, "queen"); !strings.Contains(body, "Bohemian Rhapsody") {
		t.Fatalf("search during rebuild:\n%s", body)
	}
	build(t, a)

	c = reload(t, a, c)
	if c.Status != catalogFailed || *c.ActiveVersion != 1 || len(c.Errors()) != 4 {
		t.Fatalf("after failed rebuild: %+v", c)
	}
	body := b.get(fmt.Sprintf("/catalogs/%d", c.ID)).Body.String()
	for _, want := range []string{"Latest upload failed.", "Title is blank on row 3", "Artist is blank on row 5", "15 songs"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	if body := search(b, c, "queen"); !strings.Contains(body, "Bohemian Rhapsody") {
		t.Fatalf("search after failed rebuild:\n%s", body)
	}

	// A good upload replaces version 1 and clears the errors.
	uploadCSV(t, b, c, fixture(t, "valid_minimal.csv"))
	build(t, a)
	c = reload(t, a, c)
	if c.Status != catalogReady || *c.ActiveVersion != 3 || c.ErrorsJSON != "" || c.SongCount != 3 {
		t.Fatalf("after good rebuild: %+v", c)
	}
	if _, err := os.Stat(a.cache.path(c.ID, 1)); err == nil {
		t.Error("replaced version is still cached")
	}
}

func TestCatalogFirstUploadFails(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	uploadCSV(t, b, c, fixture(t, "edge_unclosed_quote.csv"))
	build(t, a)
	body := b.get(fmt.Sprintf("/catalogs/%d", c.ID)).Body.String()
	if !strings.Contains(body, "Processing failed.") || !strings.Contains(body, "Invalid CSV: Unclosed quoted field in line 2.") || strings.Contains(body, "Search Songs") {
		t.Fatalf("page:\n%s", body)
	}
}

func TestCatalogReplacementCharacterWarning(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	uploadCSV(t, b, c, fixture(t, "edge_replacement_chars.csv"))
	build(t, a)
	body := b.get(fmt.Sprintf("/catalogs/%d?built=1", c.ID)).Body.String()
	if !strings.Contains(body, "Some song titles have funny or incorrect characters (2 songs affected).") || strings.Contains(body, "Catalog is ready!") {
		t.Fatalf("page:\n%s", body)
	}
}

func TestCatalogIdenticalUploadIsSkipped(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	csv := fixture(t, "valid_full.csv")
	uploadCSV(t, b, c, csv)
	build(t, a)

	rec := uploadCSV(t, b, c, csv)
	if loc := rec.Header().Get("Location"); loc != fmt.Sprintf("/catalogs/%d?saved=unchanged", c.ID) {
		t.Fatalf("location = %q", loc)
	}
	var versions int64
	a.db.Model(&CatalogVersion{}).Where("catalog_id = ?", c.ID).Count(&versions)
	if c = reload(t, a, c); versions != 1 || c.Status != catalogReady {
		t.Fatalf("versions %d, %+v", versions, c)
	}
}

func TestCatalogUploadErrors(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	target := fmt.Sprintf("/catalogs/%d/upload", c.ID)

	rec := b.post(target, url.Values{})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "No file provided") {
		t.Fatalf("no file: %d %s", rec.Code, rec.Body)
	}

	// Streamed, so the server stops reading as soon as it is over the limit.
	big := io.MultiReader(strings.NewReader("title,artist\n"), io.LimitReader(zeros{}, maxUploadBytes))
	rec = b.upload(target, "big.csv", big)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "File too large. Maximum size is 100MB.") {
		t.Fatalf("too large: %d", rec.Code)
	}
	if c = reload(t, a, c); c.Status != catalogEmpty {
		t.Fatalf("rejected uploads changed the catalog: %+v", c)
	}
	if left, _ := filepath.Glob(filepath.Join(a.cfg.CatalogCacheDir, "upload-*")); len(left) > 0 {
		t.Errorf("temp files left: %v", left)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestCatalogUploadRateLimit(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	for i := range 5 {
		if rec := uploadCSV(t, b, c, fmt.Sprintf("title,artist\nSong %d,Band\n", i)); rec.Code != http.StatusSeeOther {
			t.Fatalf("upload %d: %d", i, rec.Code)
		}
	}
	rec := uploadCSV(t, b, c, "title,artist\nSong,Band\n")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th upload: %d", rec.Code)
	}
	// Five pending uploads collapse into one build of the newest.
	build(t, a)
	if c = reload(t, a, c); *c.ActiveVersion != 5 || c.SongCount != 1 {
		t.Fatalf("%+v", c)
	}
}

func TestCatalogAccess(t *testing.T) {
	a := newTestApp(t)
	owner, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, owner, "Mine")
	other, _ := signInHost(t, a, "other@example.com")
	fan := newBrowser(t, a)
	signIn(t, a, fan, "fan@example.com", roleAudience)

	for _, path := range []string{"", "/status", "/search?q=a"} {
		if rec := other.get(fmt.Sprintf("/catalogs/%d%s", c.ID, path)); rec.Code != http.StatusNotFound {
			t.Errorf("other DJ GET %s: %d", path, rec.Code)
		}
	}
	for _, path := range []string{"", "/delete"} {
		if rec := other.post(fmt.Sprintf("/catalogs/%d%s", c.ID, path), url.Values{"variant_display": {"id"}}); rec.Code != http.StatusNotFound {
			t.Errorf("other DJ POST %s: %d", path, rec.Code)
		}
	}
	if rec := uploadCSV(t, other, c, "title,artist\na,b\n"); rec.Code != http.StatusNotFound {
		t.Errorf("other DJ upload: %d", rec.Code)
	}
	if rec := fan.get("/catalogs"); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Start hosting") {
		t.Errorf("audience: %d", rec.Code)
	}
	if rec := newBrowser(t, a).get("/catalogs"); rec.Code != http.StatusSeeOther {
		t.Errorf("signed out: %d", rec.Code)
	}
	if c = reload(t, a, c); c.VariantDisplay != "none" {
		t.Errorf("other DJ changed settings")
	}
}

func TestCatalogNames(t *testing.T) {
	a := newTestApp(t)
	b, u := signInHost(t, a, "dj@example.com")
	createCatalog(t, a, b, "Main")

	for name, want := range map[string]string{
		"   ":                    "Name can&#39;t be blank",
		"Main":                   "Name has already been taken",
		strings.Repeat("x", 101): "Name is too long (maximum is 100 characters)",
	} {
		rec := b.post("/catalogs", url.Values{"name": {name}})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%q: %d, want %q", name, rec.Code, want)
		}
	}
	// Another DJ can use the same name.
	other, _ := signInHost(t, a, "other@example.com")
	createCatalog(t, a, other, "Main")

	for i := 1; i < maxCatalogsPerAccount; i++ {
		a.db.Create(&Catalog{UserID: u.ID, Name: fmt.Sprintf("Extra %d", i)})
	}
	rec := b.post("/catalogs", url.Values{"name": {"One too many"}})
	if !strings.Contains(rec.Body.String(), "You can have up to 100 catalogs.") {
		t.Errorf("limit: %d", rec.Code)
	}
	if body := b.get("/catalogs").Body.String(); strings.Contains(body, "/catalogs/new") {
		t.Error("index offers New Catalog at the limit")
	}
}

func TestCatalogAccountRowLimit(t *testing.T) {
	a := newTestApp(t)
	b, u := signInHost(t, a, "dj@example.com")
	a.db.Create(&Catalog{UserID: u.ID, Name: "Huge", RowCount: maxRowsPerAccount - 2})
	c := createCatalog(t, a, b, "Main")
	uploadCSV(t, b, c, "title,artist\na,b\nc,d\ne,f\n")
	build(t, a)
	c = reload(t, a, c)
	errs := c.Errors()
	if c.Status != catalogFailed || len(errs) != 1 || errs[0].Type != "too_many_rows" ||
		errs[0].Detail != "This file would put your account over 10,000,000 rows. Your other catalogs have 9,999,998." {
		t.Fatalf("%+v %+v", c, errs)
	}
}

func TestCatalogDeleteRemovesEverything(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	uploadCSV(t, b, c, fixture(t, "valid_full.csv"))
	build(t, a)
	search(b, c, "queen")

	rec := b.post(fmt.Sprintf("/catalogs/%d/delete", c.ID), nil)
	if rec.Header().Get("Location") != "/catalogs?saved=deleted" {
		t.Fatalf("delete: %d %v", rec.Code, rec.Header())
	}
	var n int64
	a.db.Model(&CatalogVersion{}).Count(&n)
	if n != 0 {
		t.Error("versions left")
	}
	for _, path := range []string{a.cache.path(c.ID, 1), filepath.Join(a.cfg.BlobDir, "catalogs", fmt.Sprint(c.ID))} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s still exists", path)
		}
	}
}

func TestDeletingUserRemovesCatalogFiles(t *testing.T) {
	a := newTestApp(t)
	b, u := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	uploadCSV(t, b, c, fixture(t, "valid_full.csv"))
	build(t, a)
	if err := a.deleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.BlobDir, "catalogs", fmt.Sprint(c.ID))); err == nil {
		t.Error("blobs left")
	}
}

func TestCatalogCacheMissDownloads(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	uploadCSV(t, b, c, fixture(t, "valid_full.csv"))
	build(t, a)

	// Losing the whole cache directory only costs a download.
	os.RemoveAll(a.cfg.CatalogCacheDir)
	cache, err := newCatalogCache(a.cfg.CatalogCacheDir, a.cfg.CatalogCacheMax, a.blobs, a.logger, a.now)
	if err != nil {
		t.Fatal(err)
	}
	a.cache = cache
	if body := search(b, c, "queen"); !strings.Contains(body, "Bohemian Rhapsody") {
		t.Fatalf("search after cache loss:\n%s", body)
	}
	search(b, c, "piano")
	if s := a.cache.stats(); s.Misses != 1 || s.Hits != 1 || s.Files != 1 {
		t.Errorf("stats %+v", s)
	}
}

func TestCatalogCacheEvictsLeastRecentlyUsed(t *testing.T) {
	blobs := dirBlobStore{dir: t.TempDir()}
	for _, id := range []uint{1, 2, 3} {
		blobs.put(context.Background(), dbBlobKey(id, 1), bytes.NewReader(make([]byte, 100)))
	}
	clock := time.Now()
	now := func() time.Time { clock = clock.Add(time.Second); return clock }
	dir := t.TempDir()
	cache, err := newCatalogCache(dir, 250, blobs, slog.New(slog.NewTextHandler(io.Discard, nil)), now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, id := range []uint{1, 2, 1, 3} {
		if _, err := cache.open(ctx, id, 1); err != nil {
			t.Fatal(err)
		}
	}
	// 2 was used least recently.
	for id, want := range map[uint]bool{1: true, 2: false, 3: true} {
		if _, err := os.Stat(cache.path(id, 1)); (err == nil) != want {
			t.Errorf("catalog %d cached = %v, want %v", id, err == nil, want)
		}
	}
	if s := cache.stats(); s.Bytes != 200 {
		t.Errorf("bytes = %d", s.Bytes)
	}

	// A restart remembers what is on disk and ignores temp files.
	stale, fresh := filepath.Join(dir, "9_v1.db.123.tmp"), filepath.Join(dir, "upload-456.tmp")
	os.WriteFile(stale, []byte("x"), 0o644)
	os.WriteFile(fresh, []byte("x"), 0o644)
	cache, err = newCatalogCache(dir, 250, blobs, slog.New(slog.NewTextHandler(io.Discard, nil)), now)
	if err != nil {
		t.Fatal(err)
	}
	if s := cache.stats(); s.Files != 2 || s.Bytes != 200 {
		t.Errorf("after restart %+v", s)
	}
	if _, err := cache.open(ctx, 4, 1); err == nil {
		t.Error("opened a catalog the blob store doesn't have")
	}

	// Temp files go once nothing has written them for a while; one that may
	// still be in use by another process stays.
	old := time.Now().Add(-staleTempAge - time.Minute)
	os.Chtimes(stale, old, old)
	cache.removeStaleTemps(staleTempAge)
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale temp file left")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("recent temp file removed")
	}
}

func TestCatalogCacheDownloadsOnce(t *testing.T) {
	blobs := dirBlobStore{dir: t.TempDir()}
	blobs.put(context.Background(), dbBlobKey(1, 1), bytes.NewReader(make([]byte, 100)))
	cache, err := newCatalogCache(t.TempDir(), 1<<20, blobs, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := cache.open(context.Background(), 1, 1); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if s := cache.stats(); s.Misses != 1 || s.Hits != 19 {
		t.Errorf("stats %+v", s)
	}
}

func TestBuilderRequeuesInterruptedBuilds(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	uploadCSV(t, b, c, fixture(t, "valid_full.csv"))
	a.db.Model(&Catalog{}).Where("id = ?", c.ID).Update("status", catalogProcessing)

	a.requeueBuilds()
	build(t, a)
	if c = reload(t, a, c); c.Status != catalogReady {
		t.Fatalf("%+v", c)
	}
}

func TestBuilderLockIsExclusive(t *testing.T) {
	a := newTestApp(t)
	unlock, err := a.lockBuilder(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// A second process (here, a second open file) waits for the lock.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := a.lockBuilder(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock: %v", err)
	}

	unlock()
	unlock2, err := a.lockBuilder(context.Background())
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

func TestBuilderFindsPendingWorkWithoutWake(t *testing.T) {
	a := newTestApp(t)
	a.buildPoll = 20 * time.Millisecond
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	uploadCSV(t, b, c, fixture(t, "valid_full.csv"))
	build(t, a)

	// Another process hands version 1 back as pending, without waking this one.
	select {
	case <-a.buildWake:
	default:
	}
	ctx, cancel := context.WithCancel(context.Background())
	var done sync.WaitGroup
	done.Go(func() { a.runBuilder(ctx) })
	defer func() { cancel(); done.Wait() }()
	time.Sleep(50 * time.Millisecond)
	a.db.Model(&Catalog{}).Where("id = ?", c.ID).Updates(map[string]any{"status": catalogPending, "building_version": 1})

	deadline := time.Now().Add(5 * time.Second)
	for reload(t, a, c).Status != catalogReady {
		if time.Now().After(deadline) {
			t.Fatalf("pending catalog never built: %+v", reload(t, a, c))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCleanupRemovesOldCatalogVersions(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Main")
	uploadCSV(t, b, c, fixture(t, "valid_full.csv"))
	build(t, a)
	uploadCSV(t, b, c, fixture(t, "valid_minimal.csv"))
	build(t, a)

	a.cleanupOnce()
	if _, err := os.Stat(filepath.Join(a.cfg.BlobDir, csvBlobKey(c.ID, 1))); err != nil {
		t.Fatal("a fresh old version was removed")
	}

	a.db.Model(&CatalogVersion{}).Where("1 = 1").Update("created_at", time.Now().Add(-oldVersionLifetime-time.Hour))
	a.cleanupOnce()
	for v, want := range map[int]bool{1: false, 2: true} {
		for _, key := range []string{csvBlobKey(c.ID, v), dbBlobKey(c.ID, v)} {
			if _, err := os.Stat(filepath.Join(a.cfg.BlobDir, key)); (err == nil) != want {
				t.Errorf("%s exists = %v, want %v", key, err == nil, want)
			}
		}
	}
	if body := search(b, c, "journey"); !strings.Contains(body, "Journey") {
		t.Fatal("active version broke")
	}
}

func TestCSVFormatDoc(t *testing.T) {
	a := newTestApp(t)
	rec := get(t, a.routes(), "/docs/csv-format")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Maximum rows per catalog") {
		t.Fatalf("%d", rec.Code)
	}
}
