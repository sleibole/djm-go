package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// views holds one parsed template set per page: layout + partials + the page itself.
// Each page defines a "content" block that the layout renders.
type views struct {
	pages map[string]*template.Template
}

func loadViews(fsys fs.FS, static assets) (*views, error) {
	pageFiles, err := fs.Glob(fsys, "templates/*.html")
	if err != nil {
		return nil, err
	}
	partialFiles, err := fs.Glob(fsys, "templates/partials/*.html")
	if err != nil {
		return nil, err
	}
	funcs := template.FuncMap{
		"icon":       icon,
		"asset":      static.url,
		"commas":     commas,
		"humanBytes": humanBytes,
		"localTime":  localTime,
		"qrSVG":      qrSVG,
		"showPath":   showPath,
		// Replaced for each request in renderBlock. Parsing needs the name present.
		"csrfField": func() template.HTML { return "" },

		"cancelView":        cancelView,
		"memoryRequestView": memoryRequestView,
		"pickVariantView":   pickVariantView,
	}

	v := &views{pages: map[string]*template.Template{}}
	for _, page := range pageFiles {
		name := path.Base(page)
		if name == "layout.html" {
			continue
		}
		files := append([]string{"templates/layout.html"}, partialFiles...)
		files = append(files, page)
		t, err := template.New(name).Funcs(funcs).ParseFS(fsys, files...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		v.pages[name] = t
	}
	return v, nil
}

// view is what every template receives: what the layout needs, and the page's own data.
type view struct {
	BaseURL          string
	User             *User
	TurnstileSiteKey string
	Impersonating    bool // an admin is using "log in as"
	CSRFToken        string
	Meta             seoMeta
	Data             any
}

// render writes a full page, or only the page's "content" block for a fragment request.
// Boosted navigations and history restores swap the body, so they get the layout.
// HX-History-Restore-Request wins even when HX-Request is also set.
func (a *app) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	block := "layout"
	if fragmentRequest(r) {
		block = "content"
	}
	a.renderBlock(w, r, status, page, block, data)
}

// fragmentRequest is an HTMX request that swaps a piece of the current document.
// A boosted navigation, a history restore, and a request aimed at the body swap
// the whole document, so they stay full pages.
func fragmentRequest(r *http.Request) bool {
	if r.Header.Get("HX-Boosted") == "true" || r.Header.Get("HX-History-Restore-Request") == "true" || r.Header.Get("HX-Target") == "body" {
		return false
	}
	return r.Header.Get("HX-Request") == "true"
}

// renderBlock writes one named template from a page's set, e.g. a partial for an HTMX swap.
func (a *app) renderBlock(w http.ResponseWriter, r *http.Request, status int, page, block string, data any) {
	t, ok := a.views.pages[page]
	if !ok {
		a.serverError(w, r, fmt.Errorf("unknown page %q", page))
		return
	}

	token, err := a.ensureCSRFToken(w, r)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	// Each response gets its own function so the token is not shared across requests.
	cloned, err := t.Clone()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	cloned.Funcs(template.FuncMap{
		"csrfField": func() template.HTML { return csrfFieldHTML(token) },
	})

	// Render to a buffer first so a template error doesn't send a half-written page.
	var buf bytes.Buffer
	v := view{BaseURL: a.cfg.BaseURL, User: currentUser(r), TurnstileSiteKey: a.cfg.TurnstileSiteKey,
		Impersonating: impersonatorID(r) != 0, CSRFToken: token, Meta: a.seoMeta(r, status, block), Data: data}
	if err := cloned.ExecuteTemplate(&buf, block, v); err != nil {
		a.serverError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// A body that contains this browser's token must not sit in a shared
	// cache, and the browser must not replay it after the token rotates.
	// A fragment with no token keeps the handler's policy, such as the
	// signed-out catalog search cache.
	if bytes.Contains(buf.Bytes(), []byte("csrf-token")) || bytes.Contains(buf.Bytes(), []byte(`name="csrf_token"`)) {
		w.Header().Set("Cache-Control", "private, no-store")
	} else if w.Header().Get("Cache-Control") == "" && noStoreHTML(r) {
		w.Header().Set("Cache-Control", "private, no-store")
	}
	// The same URL can be a full document or a fragment, and the shell
	// changes when a session or guest cookie is present.
	w.Header().Add("Vary", htmlVary)
	if !a.responseIndexable(r, status, block) {
		w.Header().Set("X-Robots-Tag", robotsNoindex)
	}
	w.WriteHeader(status)
	buf.WriteTo(w)
}

// noStoreHTML is true for pages that are personalized or that carry a secret
// in the URL. Search sets its own policy and is left alone when the fragment
// itself has no CSRF token. Static files never reach render.
func noStoreHTML(r *http.Request) bool {
	p := r.URL.Path
	if strings.Contains(p, "/dj/") && strings.HasSuffix(p, "/search") {
		return false
	}
	if currentUser(r) != nil {
		return true
	}
	if strings.HasPrefix(p, "/auth/") || strings.HasSuffix(p, "/queue") {
		return true
	}
	if requestHasGuestCookie(r) && (strings.HasPrefix(p, "/dj/") || strings.HasPrefix(p, "/me/")) {
		return true
	}
	return false
}

// localTime writes a <time> element that app.js rewrites in the viewer's time
// zone. style is "datetime", "time" or "date". The text before that is UTC.
// t may be a time.Time or *time.Time; nil gives "—".
func localTime(t any, style string) template.HTML {
	var tt time.Time
	switch v := t.(type) {
	case time.Time:
		tt = v
	case *time.Time:
		if v == nil {
			return "—"
		}
		tt = *v
	default:
		return "—"
	}
	layout := map[string]string{"time": "3:04 PM UTC", "date": "Jan 2, 2006"}[style]
	if layout == "" {
		layout = "Jan 2, 3:04 PM UTC"
	}
	tt = tt.UTC()
	return template.HTML(fmt.Sprintf(`<time datetime="%s" data-local="%s">%s</time>`,
		tt.Format(time.RFC3339), template.HTMLEscapeString(style), tt.Format(layout)))
}

func (a *app) serverError(w http.ResponseWriter, r *http.Request, err error) {
	a.logger.Error("server error", "method", r.Method, "path", logPath(r.URL.Path), "err", err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}
