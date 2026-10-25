package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func metaContent(body, name string) string {
	needle := `<meta name="` + name + `" content="`
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	rest := body[i+len(needle):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func enableIndexing(a *app) {
	a.cfg.Env = "prod"
	a.cfg.AllowIndexing = true
}

func TestIndexingStaysOffOutsideProduction(t *testing.T) {
	a := newTestApp(t)
	a.cfg.AllowIndexing = true // ignored while ENV is dev
	h := a.routes()

	home := get(t, h, "/")
	if home.Code != http.StatusOK {
		t.Fatalf("home status %d", home.Code)
	}
	body := home.Body.String()
	if got := metaContent(body, "robots"); got != "noindex, nofollow" {
		t.Fatalf("robots meta = %q", got)
	}
	if got := home.Header().Get("X-Robots-Tag"); got != "noindex, nofollow" {
		t.Fatalf("X-Robots-Tag = %q", got)
	}
	if !strings.Contains(body, `<link rel="canonical" href="http://example.test/">`) {
		t.Fatalf("canonical missing:\n%s", body)
	}
	if !strings.Contains(body, `<meta property="og:url" content="http://example.test/">`) {
		t.Fatal("og:url should be the canonical home URL")
	}
	if !strings.Contains(body, "Song requests, not chaos. Scan a QR code and request instantly. No app needed.") {
		t.Fatal("home description missing")
	}
	if !strings.Contains(body, "<title>DJMagic.io — Song requests, not chaos</title>") {
		t.Fatal("home title missing")
	}

	robots := get(t, h, "/robots.txt")
	if robots.Code != http.StatusOK || robots.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("robots = %d %q", robots.Code, robots.Header().Get("Content-Type"))
	}
	if robots.Body.String() != "User-agent: *\nDisallow: /\n" {
		t.Fatalf("robots.txt =\n%s", robots.Body.String())
	}
	if strings.Contains(robots.Body.String(), "/account") || strings.Contains(robots.Body.String(), "/admin") {
		t.Fatal("robots.txt lists private routes")
	}
	if rec := get(t, h, "/sitemap.xml"); rec.Code != http.StatusNotFound {
		t.Fatalf("sitemap while indexing is off: %d", rec.Code)
	}
}

func TestPublicPagesAreIndexableWhenEnabled(t *testing.T) {
	a := newTestApp(t)
	enableIndexing(a)
	h := a.routes()

	home := get(t, h, "/?utm_source=share")
	body := home.Body.String()
	if home.Header().Get("X-Robots-Tag") != "" {
		t.Fatalf("home X-Robots-Tag = %q", home.Header().Get("X-Robots-Tag"))
	}
	if metaContent(body, "robots") != "" {
		t.Fatalf("indexable home should omit robots meta, got %q", metaContent(body, "robots"))
	}
	if !strings.Contains(body, `<link rel="canonical" href="http://example.test/">`) {
		t.Fatal("query string leaked into the canonical URL")
	}
	if strings.Contains(body, "utm_source") {
		t.Fatal("tracking parameter was copied into the document")
	}
	if !strings.Contains(body, `property="og:title" content="DJMagic.io — Song requests, not chaos"`) {
		t.Fatal("og:title should match the document title")
	}
	if !strings.Contains(body, `property="og:image:alt" content="DJMagic.io: Beta is Live. Song requests, not chaos."`) {
		t.Fatal("og:image:alt missing")
	}
	if !strings.Contains(body, `<template id="page-meta"`) {
		t.Fatal("page meta was not included for boosted navigation")
	}

	docs := get(t, h, "/docs/csv-format")
	docsBody := docs.Body.String()
	if docs.Header().Get("X-Robots-Tag") != "" {
		t.Fatalf("docs X-Robots-Tag = %q", docs.Header().Get("X-Robots-Tag"))
	}
	if !strings.Contains(docsBody, "<title>CSV Format Guide · DJMagic.io</title>") {
		t.Fatal("docs title missing")
	}
	if !strings.Contains(docsBody, "title and artist are required") {
		t.Fatal("docs description missing")
	}
	if !strings.Contains(docsBody, `<link rel="canonical" href="http://example.test/docs/csv-format">`) {
		t.Fatal("docs canonical missing")
	}
	if strings.Contains(docsBody, "Song requests, not chaos. Scan a QR code") {
		t.Fatal("docs page reused the home description")
	}

	robots := get(t, h, "/robots.txt")
	robotsBody := robots.Body.String()
	if !strings.Contains(robotsBody, "Disallow: /auth/\n") || !strings.Contains(robotsBody, "Sitemap: http://example.test/sitemap.xml\n") {
		t.Fatalf("robots.txt =\n%s", robotsBody)
	}
	if strings.Contains(robotsBody, "Disallow: /\n") || strings.Contains(robotsBody, "/account") || strings.Contains(robotsBody, "/admin") || strings.Contains(robotsBody, "/shows") || strings.Contains(robotsBody, "/dj/") {
		t.Fatalf("robots.txt exposes or blocks the wrong paths:\n%s", robotsBody)
	}

	sitemap := get(t, h, "/sitemap.xml")
	if sitemap.Code != http.StatusOK || !strings.Contains(sitemap.Header().Get("Content-Type"), "xml") {
		t.Fatalf("sitemap = %d %q", sitemap.Code, sitemap.Header().Get("Content-Type"))
	}
	site := sitemap.Body.String()
	if !strings.Contains(site, "<loc>http://example.test/</loc>") || !strings.Contains(site, "<loc>http://example.test/docs/csv-format</loc>") {
		t.Fatalf("sitemap =\n%s", site)
	}
	for _, hidden := range []string{"/signin", "/signup", "/feedback", "/account", "/admin", "/dj/", "/auth/", "/healthz", "lastmod"} {
		if strings.Contains(site, hidden) {
			t.Fatalf("sitemap contains %q\n%s", hidden, site)
		}
	}
}

func TestPrivateAndSharedPagesStayUnindexed(t *testing.T) {
	a := newTestApp(t)
	enableIndexing(a)
	h := a.routes()

	signedIn := newBrowser(t, a)
	signIn(t, a, signedIn, "member@example.com", "audience")
	home := signedIn.get("/")
	if metaContent(home.Body.String(), "robots") != "noindex, nofollow" || home.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("signed-in home indexed: meta %q header %q", metaContent(home.Body.String(), "robots"), home.Header().Get("X-Robots-Tag"))
	}
	if strings.Contains(metaContent(home.Body.String(), "description"), "member@example.com") {
		t.Fatal("dashboard description includes the email")
	}
	if !strings.Contains(metaContent(home.Body.String(), "description"), "Your DJMagic.io home.") {
		t.Fatalf("dashboard description = %q", metaContent(home.Body.String(), "description"))
	}

	for _, path := range []string{"/signin", "/signup", "/feedback"} {
		rec := get(t, h, path)
		if rec.Code != http.StatusOK || metaContent(rec.Body.String(), "robots") != "noindex, nofollow" {
			t.Fatalf("%s: %d robots %q", path, rec.Code, metaContent(rec.Body.String(), "robots"))
		}
	}
	if metaContent(get(t, h, "/signin").Body.String(), "description") == metaContent(get(t, h, "/signup").Body.String(), "description") {
		t.Fatal("sign-in and sign-up share a description")
	}

	account := get(t, h, "/account")
	if account.Code != http.StatusSeeOther || account.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("account redirect = %d %q", account.Code, account.Header().Get("X-Robots-Tag"))
	}

	_, _, path := audienceShow(t, a, url.Values{"name": {"Friday Night"}})
	show := get(t, h, path+"?q=hello")
	if show.Code != http.StatusOK {
		t.Fatalf("show %d", show.Code)
	}
	if metaContent(show.Body.String(), "robots") != "noindex, nofollow" {
		t.Fatal("public show page should not be indexed")
	}
	if !strings.Contains(show.Body.String(), `<link rel="canonical" href="http://example.test`+path+`">`) {
		t.Fatal("show canonical should drop the search query")
	}
	if !strings.Contains(show.Body.String(), "Request a song at Friday Night") {
		t.Fatalf("show description missing: %q", metaContent(show.Body.String(), "description"))
	}
	if strings.Contains(metaContent(show.Body.String(), "description"), "Alice") {
		t.Fatal("show description includes a singer")
	}
	handlePath := path[:strings.Index(path, "/shows/")]
	profile := get(t, h, handlePath)
	if profile.Code != http.StatusOK || metaContent(profile.Body.String(), "robots") != "noindex, nofollow" {
		t.Fatalf("profile %d robots %q", profile.Code, metaContent(profile.Body.String(), "robots"))
	}

	missing := get(t, h, "/no-such-page")
	if missing.Code != http.StatusNotFound || metaContent(missing.Body.String(), "robots") != "noindex, nofollow" {
		t.Fatalf("missing page %d robots %q", missing.Code, metaContent(missing.Body.String(), "robots"))
	}
	if !strings.Contains(missing.Body.String(), "<h1>Not found</h1>") {
		t.Fatal("404 is not a document")
	}

	health := get(t, h, "/healthz")
	if health.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("healthz robots %q", health.Header().Get("X-Robots-Tag"))
	}
	if css := get(t, h, "/static/app.css"); css.Header().Get("X-Robots-Tag") != "" {
		t.Fatalf("static file robots %q", css.Header().Get("X-Robots-Tag"))
	}
}

func TestMagicLinkMetadataOmitsTheAddress(t *testing.T) {
	a := newTestApp(t)
	enableIndexing(a)
	b := newBrowser(t, a)
	if rec := b.post("/signin/link", url.Values{"email": {"singer@example.com"}}); rec.Code != http.StatusOK {
		t.Fatalf("link request %d", rec.Code)
	}
	rec := b.get(lastLink(t, a, "singer@example.com"))
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm page %d", rec.Code)
	}
	if rec.Header().Get("X-Robots-Tag") != "noindex, nofollow" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("magic link cache %q robots %q", rec.Header().Get("Cache-Control"), rec.Header().Get("X-Robots-Tag"))
	}
	desc := metaContent(rec.Body.String(), "description")
	if strings.Contains(desc, "singer@example.com") || strings.Contains(desc, "/auth/") {
		t.Fatalf("description leaks the link: %q", desc)
	}
	if !strings.Contains(desc, "Confirm sign-in") {
		t.Fatalf("description = %q", desc)
	}
}

func TestTrailingSlashRedirectsToTheCanonicalPath(t *testing.T) {
	a := newTestApp(t)
	h := a.routes()

	rec := get(t, h, "/docs/csv-format/?from=old")
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/docs/csv-format?from=old" {
		t.Fatalf("location %q", loc)
	}
	if rec.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("redirect robots %q", rec.Header().Get("X-Robots-Tag"))
	}

	followed := get(t, h, "/docs/csv-format")
	if followed.Code != http.StatusOK {
		t.Fatalf("canonical docs %d", followed.Code)
	}
	if rec := get(t, h, "/static/"); rec.Code != http.StatusNotFound {
		t.Fatalf("/static/ = %d", rec.Code)
	}
	if rec := get(t, h, "/static/images/"); rec.Code != http.StatusNotFound {
		t.Fatalf("/static/images/ = %d", rec.Code)
	}

	post := httptest.NewRequest(http.MethodPost, "/feedback/", strings.NewReader("category=bug&message=hi"))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	redirect := httptest.NewRecorder()
	h.ServeHTTP(redirect, post)
	if redirect.Code != http.StatusPermanentRedirect || redirect.Header().Get("Location") != "/feedback" {
		t.Fatalf("POST slash = %d %q", redirect.Code, redirect.Header().Get("Location"))
	}
}

func TestFragmentAndDocumentAreNotCacheTwins(t *testing.T) {
	a := newTestApp(t)
	enableIndexing(a)
	h := a.routes()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	fragment := httptest.NewRecorder()
	h.ServeHTTP(fragment, req)
	if strings.Contains(fragment.Body.String(), "<html") {
		t.Fatal("fragment included a document")
	}
	if fragment.Header().Get("X-Robots-Tag") != "noindex, nofollow" {
		t.Fatalf("fragment robots %q", fragment.Header().Get("X-Robots-Tag"))
	}
	if !strings.Contains(fragment.Header().Get("Vary"), "HX-Request") || !strings.Contains(fragment.Header().Get("Vary"), "Cookie") {
		t.Fatalf("fragment vary %q", fragment.Header().Get("Vary"))
	}

	boosted := httptest.NewRequest(http.MethodGet, "/docs/csv-format", nil)
	boosted.Header.Set("HX-Request", "true")
	boosted.Header.Set("HX-Boosted", "true")
	boosted.Header.Set("HX-Target", "body")
	full := httptest.NewRecorder()
	h.ServeHTTP(full, boosted)
	if !strings.Contains(full.Body.String(), "<title>CSV Format Guide · DJMagic.io</title>") || !strings.Contains(full.Body.String(), "<html") {
		t.Fatal("boosted navigation was not a document")
	}
	if full.Header().Get("X-Robots-Tag") != "" {
		t.Fatalf("boosted docs should stay indexable, robots %q", full.Header().Get("X-Robots-Tag"))
	}
	if !strings.Contains(full.Header().Get("Vary"), "HX-Boosted") {
		t.Fatalf("boosted vary %q", full.Header().Get("Vary"))
	}
}

func TestGuestShowPageIsNotStored(t *testing.T) {
	a := newTestApp(t)
	_, _, path := audienceShow(t, a, url.Values{"name": {"Friday"}})
	anon := newBrowser(t, a)
	if cc := anon.get(path).Header().Get("Cache-Control"); cc != "" {
		t.Fatalf("anonymous show cache %q", cc)
	}
	if vary := anon.get(path).Header().Get("Vary"); !strings.Contains(vary, "Cookie") {
		t.Fatalf("anonymous show vary %q", vary)
	}

	g := guest(t, a, "192.0.2.55")
	if rec := request(g, path, "Alice", "Hello"); rec.Code != http.StatusSeeOther {
		t.Fatalf("request %d", rec.Code)
	}
	if cc := g.get(path).Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("guest show cache %q", cc)
	}
	if cc := g.get(path + "/queue").Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("queue cache %q", cc)
	}
}
