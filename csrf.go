package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"html/template"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"time"

	"gorm.io/gorm"
)

const (
	csrfCookieName = "csrf"
	csrfHeader     = "X-CSRF-Token"
	csrfFormField  = "csrf_token"
	// csrfTokenMaxLen is longer than a newToken value (43) and short enough to reject padding.
	csrfTokenMaxLen = 128

	csrfFailureMessage = "Invalid or missing CSRF token. Reload the page and try again."
)

// csrfBodyLimit caps a multipart body read while the middleware looks for the
// token. part.Close drains the rest of a part, so this is what stops a huge
// first part. It matches the catalog upload limit. Tests lower it.
var csrfBodyLimit int64 = maxUploadBytes + 1<<20

// multipartResume is the unread remainder of a multipart body after the CSRF
// field has been taken off the front. Catalog upload streams that remainder.
type multipartResume struct {
	reader *multipart.Reader
}

type multipartResumeKeyType struct{}

var multipartResumeKey multipartResumeKeyType

func resumedMultipart(r *http.Request) *multipart.Reader {
	resume, _ := r.Context().Value(multipartResumeKey).(*multipartResume)
	if resume == nil {
		return nil
	}
	return resume.reader
}

func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func csrfTokenFromCtx(r *http.Request) string {
	token, _ := r.Context().Value(csrfKey).(string)
	return token
}

// csrfTokensEqual compares a submitted token with the one stored for this browser.
// Hashing both keeps the comparison constant-time when the lengths differ.
func csrfTokensEqual(got, want string) bool {
	if want == "" || got == "" || len(got) > csrfTokenMaxLen || len(want) > csrfTokenMaxLen {
		return false
	}
	sumGot := sha256.Sum256([]byte(got))
	sumWant := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(sumGot[:], sumWant[:]) == 1
}

// csrfFieldHTML is the shared hidden input. The token is escaped here because
// template.HTML is trusted markup.
func csrfFieldHTML(token string) template.HTML {
	return template.HTML(`<input type="hidden" name="` + csrfFormField + `" value="` + template.HTMLEscapeString(token) + `">`)
}

func (a *app) csrfCookie(raw string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     a.cookieName(csrfCookieName),
		Value:    raw,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	}
}

func (a *app) clearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName(csrfCookieName),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

// clearAnonymousCSRF drops the logged-out token so it cannot be reused after
// login, logout, or another new session.
func (a *app) clearAnonymousCSRF(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(a.cookieName(csrfCookieName)); err == nil && c.Value != "" {
		if err := a.db.Delete(&CSRFSession{}, "id = ?", hashToken(c.Value)).Error; err != nil {
			return err
		}
	}
	a.clearCSRFCookie(w)
	return nil
}

// ensureSessionCSRF fills a session that predates the csrf_token column.
// A concurrent request may win the write; both then share the stored token.
func (a *app) ensureSessionCSRF(s *Session) error {
	if s.CSRFToken != "" {
		return nil
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	res := a.db.Model(&Session{}).Where("id = ? AND csrf_token = ''", s.ID).Update("csrf_token", token)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		var fresh Session
		if err := a.db.Take(&fresh, "id = ?", s.ID).Error; err != nil {
			return err
		}
		s.CSRFToken = fresh.CSRFToken
		return nil
	}
	s.CSRFToken = token
	return nil
}

// ensureCSRFToken returns the token for this browser, creating an anonymous
// csrf session when the visitor is not logged in. The token stays the same
// until the session is replaced.
func (a *app) ensureCSRFToken(w http.ResponseWriter, r *http.Request) (string, error) {
	if currentUser(r) != nil {
		token := csrfTokenFromCtx(r)
		if token == "" {
			return "", errors.New("session has no csrf token")
		}
		return token, nil
	}
	name := a.cookieName(csrfCookieName)
	if c, err := r.Cookie(name); err == nil && c.Value != "" {
		var row CSRFSession
		err := a.db.Take(&row, "id = ?", hashToken(c.Value)).Error
		if err == nil && a.now().Before(row.ExpiresAt) {
			if row.ExpiresAt.Sub(a.now()) < sessionLifetime/2 {
				row.ExpiresAt = a.now().Add(sessionLifetime)
				if err := a.db.Model(&row).Update("expires_at", row.ExpiresAt).Error; err != nil {
					return "", err
				}
				http.SetCookie(w, a.csrfCookie(c.Value, row.ExpiresAt))
			}
			return row.Token, nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return "", err
		}
		if err == nil {
			a.db.Delete(&row)
		}
	}
	raw, err := newToken()
	if err != nil {
		return "", err
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	expires := a.now().Add(sessionLifetime)
	row := CSRFSession{ID: hashToken(raw), Token: token, ExpiresAt: expires}
	if err := a.db.Create(&row).Error; err != nil {
		return "", err
	}
	http.SetCookie(w, a.csrfCookie(raw, expires))
	return token, nil
}

func (a *app) expectedCSRF(r *http.Request) (string, error) {
	if currentUser(r) != nil {
		return csrfTokenFromCtx(r), nil
	}
	c, err := r.Cookie(a.cookieName(csrfCookieName))
	if err != nil || c.Value == "" {
		return "", nil
	}
	var row CSRFSession
	err = a.db.Take(&row, "id = ?", hashToken(c.Value)).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !a.now().Before(row.ExpiresAt) {
		return "", nil
	}
	return row.Token, nil
}

// readCSRFToken returns the submitted token. A header wins and leaves the body
// alone. Multipart bodies are not parsed with ParseMultipartForm: the CSRF
// field has to be the first part so the file can keep streaming.
func (a *app) readCSRFToken(w http.ResponseWriter, r *http.Request) (string, *multipartResume, error) {
	if h := r.Header.Get(csrfHeader); h != "" {
		if len(h) > csrfTokenMaxLen {
			return "", nil, nil
		}
		return h, nil, nil
	}
	media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch media {
	case "application/x-www-form-urlencoded":
		token := r.PostFormValue(csrfFormField)
		if len(token) > csrfTokenMaxLen {
			return "", nil, nil
		}
		return token, nil, nil
	case "multipart/form-data":
		return a.readMultipartCSRF(w, r)
	default:
		return "", nil, nil
	}
}

func (a *app) readMultipartCSRF(w http.ResponseWriter, r *http.Request) (string, *multipartResume, error) {
	// The catalog handler also caps the body. This wrapper is what the
	// multipart reader actually reads, including a Close that drains a part.
	r.Body = http.MaxBytesReader(w, r.Body, csrfBodyLimit)
	mr, err := r.MultipartReader()
	if err != nil {
		return "", nil, nil
	}
	part, err := mr.NextPart()
	if err != nil {
		return "", nil, ignoreMaxBytes(err)
	}
	defer part.Close()
	if part.FormName() != csrfFormField {
		return "", nil, nil
	}
	buf, err := io.ReadAll(io.LimitReader(part, csrfTokenMaxLen+1))
	if err != nil {
		return "", nil, ignoreMaxBytes(err)
	}
	if len(buf) > csrfTokenMaxLen {
		return "", nil, nil
	}
	return string(buf), &multipartResume{reader: mr}, nil
}

// ignoreMaxBytes turns a capped body into a missing token. The handler must
// not run, and a too-large part is not a server failure.
func ignoreMaxBytes(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return nil
	}
	return err
}

func (a *app) rejectCSRF(w http.ResponseWriter, r *http.Request) {
	// The token itself is never logged.
	a.logger.Warn("csrf rejected", "method", r.Method, "path", logPath(r.URL.Path))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-CSRF-Rejected", "1")
	w.WriteHeader(http.StatusForbidden)
	io.WriteString(w, "<!doctype html><meta charset=\"utf-8\"><title>Forbidden</title><p role=\"alert\">"+template.HTMLEscapeString(csrfFailureMessage)+"</p><p><button type=\"button\" onclick=\"location.reload()\">Reload this page</button></p>\n")
}

// csrfProtect rejects state-changing requests that do not carry this browser's token.
// There is no route allowlist. Safe methods pass through.
func (a *app) csrfProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if safeMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		got, resume, err := a.readCSRFToken(w, r)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		expected, err := a.expectedCSRF(r)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		if !csrfTokensEqual(got, expected) {
			a.rejectCSRF(w, r)
			return
		}
		if resume != nil {
			r = r.WithContext(context.WithValue(r.Context(), multipartResumeKey, resume))
		}
		next.ServeHTTP(w, r)
	})
}
