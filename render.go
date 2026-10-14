package main

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path"
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
		"icon":  icon,
		"asset": static.url,
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
	BaseURL string
	Data    any
}

// render writes a full page, or only the page's "content" block for HTMX requests.
func (a *app) render(w http.ResponseWriter, r *http.Request, status int, page string, data any) {
	block := "layout"
	if r.Header.Get("HX-Request") == "true" {
		block = "content"
	}
	a.renderBlock(w, r, status, page, block, data)
}

// renderBlock writes one named template from a page's set, e.g. a partial for an HTMX swap.
func (a *app) renderBlock(w http.ResponseWriter, r *http.Request, status int, page, block string, data any) {
	t, ok := a.views.pages[page]
	if !ok {
		a.serverError(w, r, fmt.Errorf("unknown page %q", page))
		return
	}

	// Render to a buffer first so a template error doesn't send a half-written page.
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, block, view{BaseURL: a.cfg.BaseURL, Data: data}); err != nil {
		a.serverError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (a *app) serverError(w http.ResponseWriter, r *http.Request, err error) {
	a.logger.Error("server error", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}
