package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

// assets maps each file under static/ to a short content hash. Templates link to
// /static/{name}?v={hash}, so those responses can be cached for a year and a deploy
// that changes a file changes its URL.
type assets map[string]string

func hashAssets(fsys fs.FS) (assets, error) {
	s := assets{}
	err := fs.WalkDir(fsys, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		s[strings.TrimPrefix(p, "static/")] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
	return s, err
}

// url is the template func behind {{asset "app.css"}}. An unknown name is a
// template error, so a typo fails the page render tests.
func (s assets) url(name string) (string, error) {
	h, ok := s[name]
	if !ok {
		return "", fmt.Errorf("static file %q not found", name)
	}
	return "/static/" + name + "?v=" + h, nil
}

func (s assets) handler(fsys fs.FS) http.Handler {
	files := http.FileServerFS(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		h, ok := s[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("v") == h {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
