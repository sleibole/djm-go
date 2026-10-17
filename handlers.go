package main

import (
	"net/http"
)

func (a *app) handleHealthz(w http.ResponseWriter, r *http.Request) {
	sqlDB, err := a.db.DB()
	if err == nil {
		var one int
		err = sqlDB.QueryRowContext(r.Context(), "SELECT 1").Scan(&one)
	}
	if err != nil {
		a.logger.Error("healthz: database check failed", "err", err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

func (a *app) handleHome(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		a.render(w, r, http.StatusOK, "dashboard.html", nil)
		return
	}
	a.render(w, r, http.StatusOK, "home.html", nil)
}

func (a *app) handleCSVFormatDoc(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "docs_csv_format.html", nil)
}

func (a *app) handleFavicon(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, embedded, "static/favicon.ico")
}

type messagePage struct {
	Title    string
	Text     string
	LinkURL  string // where to go instead of the home page, if set
	LinkText string
}

func (a *app) message(w http.ResponseWriter, r *http.Request, status int, title, text string) {
	a.render(w, r, status, "message.html", messagePage{Title: title, Text: text})
}

// notFound is also the answer for things that exist but the viewer can't see.
func (a *app) notFound(w http.ResponseWriter, r *http.Request) {
	a.message(w, r, http.StatusNotFound, "Not found", "There's nothing here, or you don't have access to it.")
}
