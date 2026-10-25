package main

import (
	"encoding/xml"
	"net/http"
	"strings"
)

const (
	robotsNoindex = "noindex, nofollow"
	// htmlVary lists every request header that changes an HTML body.
	// HX-* selects a document or a fragment. Cookie selects the signed-in
	// shell and guest personalization.
	htmlVary = "HX-Request, HX-Boosted, HX-History-Restore-Request, HX-Target, Cookie"
)

// indexingEnabled is true only for a production process that has opted in.
// ENV=prod is also how the staging host runs, so the flag stays off until
// BASE_URL is the origin that should appear in search.
func (c config) indexingEnabled() bool {
	return c.Env == "prod" && c.AllowIndexing
}

// indexableDocument is a public page a crawler should list. Share links,
// search filters, accounts, and the signed-in home are not.
func (a *app) indexableDocument(r *http.Request) bool {
	if !a.cfg.indexingEnabled() {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch r.URL.Path {
	case "/":
		return currentUser(r) == nil
	case "/docs/csv-format":
		return true
	default:
		return false
	}
}

// responseIndexable is a successful full document of an indexable page.
// Fragments, errors, and redirects are not.
func (a *app) responseIndexable(r *http.Request, status int, block string) bool {
	return status == http.StatusOK && block == "layout" && a.indexableDocument(r)
}

// seoMeta is the request-specific part of the document head. Titles and
// descriptions live in the templates, next to the visible copy.
type seoMeta struct {
	Canonical string
	Robots    string // empty means the page may be indexed
}

func (a *app) canonicalURL(r *http.Request) string {
	p := r.URL.EscapedPath()
	if p == "" {
		p = "/"
	}
	return a.cfg.BaseURL + p
}

func (a *app) seoMeta(r *http.Request, status int, block string) seoMeta {
	m := seoMeta{Canonical: a.canonicalURL(r)}
	if !a.responseIndexable(r, status, block) {
		m.Robots = robotsNoindex
	}
	return m
}

func requestHasGuestCookie(r *http.Request) bool {
	for _, name := range []string{guestCookie, "__Host-" + guestCookie} {
		c, err := r.Cookie(name)
		if err == nil && c.Value != "" {
			return true
		}
	}
	return false
}

// robotsExempt paths are not HTML documents. robots.txt has to stay
// readable when the rest of the host is noindex.
func robotsExempt(path string) bool {
	return path == "/robots.txt" || path == "/sitemap.xml" || path == "/favicon.ico" || strings.HasPrefix(path, "/static/")
}

// robotsHeader marks responses that should not be indexed. HTML from
// renderBlock sets the header itself, including fragments of an otherwise
// indexable URL. This wrapper covers redirects, errors, and health checks.
func (a *app) robotsHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&robotsWriter{ResponseWriter: w, r: r, app: a}, r)
	})
}

type robotsWriter struct {
	http.ResponseWriter
	r    *http.Request
	app  *app
	done bool
}

func (w *robotsWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *robotsWriter) WriteHeader(status int) {
	w.finish(status)
}

func (w *robotsWriter) Write(b []byte) (int, error) {
	w.finish(http.StatusOK)
	return w.ResponseWriter.Write(b)
}

func (w *robotsWriter) finish(status int) {
	if w.done {
		return
	}
	w.done = true
	if w.Header().Get("X-Robots-Tag") == "" && !robotsExempt(w.r.URL.Path) {
		if status != http.StatusOK || !w.app.indexableDocument(w.r) {
			w.Header().Set("X-Robots-Tag", robotsNoindex)
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

// trailingSlash sends /page/ to /page. /static/ is a route prefix, and the
// directory URLs under it are 404s rather than redirects.
func trailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) > 1 && strings.HasSuffix(p, "/") && !strings.HasPrefix(p, "/static/") {
			dest := strings.TrimRight(p, "/")
			if dest == "" {
				dest = "/"
			}
			if r.URL.RawQuery != "" {
				dest += "?" + r.URL.RawQuery
			}
			code := http.StatusMovedPermanently
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				code = http.StatusPermanentRedirect
			}
			http.Redirect(w, r, dest, code)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	var b strings.Builder
	b.WriteString("User-agent: *\n")
	if !a.cfg.indexingEnabled() {
		b.WriteString("Disallow: /\n")
	} else {
		// Magic-link paths contain a secret and the account email.
		// Disallow is not access control; the page is also noindex.
		b.WriteString("Disallow: /auth/\n")
		b.WriteString("\nSitemap: ")
		b.WriteString(a.cfg.BaseURL)
		b.WriteString("/sitemap.xml\n")
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(b.String()))
}

func (a *app) handleSitemap(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.indexingEnabled() {
		a.notFound(w, r)
		return
	}
	type urlEntry struct {
		Loc string `xml:"loc"`
	}
	type urlset struct {
		XMLName xml.Name   `xml:"urlset"`
		XMLNS   string     `xml:"xmlns,attr"`
		URLs    []urlEntry `xml:"url"`
	}
	set := urlset{XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	for _, p := range []string{"/", "/docs/csv-format"} {
		set.URLs = append(set.URLs, urlEntry{Loc: a.cfg.BaseURL + p})
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(xml.Header))
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(set); err != nil {
		a.logger.Error("sitemap", "err", err)
	}
}
