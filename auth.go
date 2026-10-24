package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	sessionCookie   = "session"
	sessionLifetime = 30 * 24 * time.Hour
	// impersonationLifetime caps an admin's "log in as" session; it isn't renewed.
	impersonationLifetime = time.Hour
	magicLinkLifetime     = 10 * time.Minute
	maxFailedLogins       = 10
	minPasswordLen        = 8
	maxPasswordLen        = 72 // bcrypt ignores bytes past 72
)

// randRead is the cryptographic random source for tokens. Tests replace it to
// force a failure. A failure must not become a token.
var randRead = rand.Read

// newToken returns 32 random bytes, base64url-encoded, for session IDs and emailed links.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := randRead(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// dummyHash is compared against when an email has no password, so a failed login
// takes about as long whether or not the account exists.
var dummyHash = sync.OnceValue(func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("not a real password"), bcrypt.DefaultCost)
	return h
})

func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// validEmail accepts one plain address. net/mail would also accept a display name
// ("Bob <bob@example.com>"); requiring the parsed address to equal the input rejects that.
func validEmail(email string) bool {
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Address == email
}

func validatePassword(pw string) string {
	switch {
	case len(pw) < minPasswordLen:
		return fmt.Sprintf("Password must be at least %d characters.", minPasswordLen)
	case len(pw) > maxPasswordLen:
		return fmt.Sprintf("Password must be at most %d characters.", maxPasswordLen)
	}
	return ""
}

// safeNext keeps post-login redirects on this site. Only a local path is allowed.
// url.Parse rejects malformed values and control characters; the extra checks
// reject scheme-relative URLs and backslash forms a browser can treat as a host.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	for i := 0; i < len(next); i++ {
		if next[i] < 0x20 || next[i] == 0x7f || next[i] == '\\' {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return next
}

// Request context: the logged-in user and their session ID.

type ctxKey int

const (
	userKey ctxKey = iota
	sessionKey
	impersonatorKey
)

// impersonatorID is the admin behind a "log in as" session, or 0.
func impersonatorID(r *http.Request) uint {
	id, _ := r.Context().Value(impersonatorKey).(uint)
	return id
}

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(userKey).(*User)
	return u
}

func currentSessionID(r *http.Request) string {
	id, _ := r.Context().Value(sessionKey).(string)
	return id
}

// secureCookies is true outside development. Production is reached over HTTPS
// at the browser even though this process sees plain HTTP behind the proxy.
func (a *app) secureCookies() bool { return a.cfg.Env != "dev" }

// cookieName is name in development and __Host-name when the cookie is Secure.
// Browsers reject the __Host- prefix without Secure, so HTTP development keeps
// the plain name. Production does not read the plain name.
func (a *app) cookieName(name string) string {
	if a.secureCookies() {
		return "__Host-" + name
	}
	return name
}

func (a *app) setSessionCookie(w http.ResponseWriter, raw string, expires time.Time) {
	http.SetCookie(w, a.sessionCookie(raw, expires))
}

func (a *app) sessionCookie(raw string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     a.cookieName(sessionCookie),
		Value:    raw,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	}
}

func (a *app) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName(sessionCookie),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

// startSession replaces the request's session, if any, with a new one for userID.
func (a *app) startSession(w http.ResponseWriter, r *http.Request, userID uint) error {
	return a.startSessionAs(w, r, userID, nil)
}

// startSessionAs is startSession, recording the admin behind a "log in as".
func (a *app) startSessionAs(w http.ResponseWriter, r *http.Request, userID uint, impersonator *uint) error {
	if id := currentSessionID(r); id != "" {
		if err := a.db.Delete(&Session{}, "id = ?", id).Error; err != nil {
			return err
		}
	}
	raw, err := newToken()
	if err != nil {
		return err
	}
	expires := a.now().Add(sessionLifetime)
	if impersonator != nil {
		expires = a.now().Add(impersonationLifetime)
	}
	s := Session{ID: hashToken(raw), UserID: userID, ImpersonatorID: impersonator, ExpiresAt: expires}
	if err := a.db.Create(&s).Error; err != nil {
		return err
	}
	a.setSessionCookie(w, raw, expires)
	return nil
}

// loadUser puts the logged-in user, if any, into the request context. Sessions renew
// once they're past half their lifetime, so active users stay logged in.
// /static and /healthz skip the lookup, so the health check answers from its own query.
func (a *app) loadUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(a.cookieName(sessionCookie))
		if err != nil || strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		var s Session
		err = a.db.Take(&s, "id = ?", hashToken(c.Value)).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			a.clearSessionCookie(w)
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			a.serverError(w, r, err)
			return
		}

		now := a.now()
		if !now.Before(s.ExpiresAt) {
			a.db.Delete(&s)
			a.clearSessionCookie(w)
			next.ServeHTTP(w, r)
			return
		}

		var u User
		if err := a.db.Take(&u, s.UserID).Error; err != nil {
			a.serverError(w, r, err)
			return
		}

		if s.ImpersonatorID == nil && s.ExpiresAt.Sub(now) < sessionLifetime/2 {
			s.ExpiresAt = now.Add(sessionLifetime)
			if err := a.db.Model(&s).Update("expires_at", s.ExpiresAt).Error; err != nil {
				a.serverError(w, r, err)
				return
			}
			a.setSessionCookie(w, c.Value, s.ExpiresAt)
		}

		ctx := context.WithValue(r.Context(), userKey, &u)
		ctx = context.WithValue(ctx, sessionKey, s.ID)
		if s.ImpersonatorID != nil {
			ctx = context.WithValue(ctx, impersonatorKey, *s.ImpersonatorID)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *app) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			http.Redirect(w, r, "/signin?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

// Sign in and sign up share one form. The email step signs in an existing account
// and creates a new one otherwise; the reply is the same either way, so the form
// can't be used to find out who has an account. Signup only adds the role choice.

type signinForm struct {
	Email        string
	Next         string
	Error        string
	Signup       bool
	Role         string // signup only: roleDJ, roleAudience, or empty until they choose
	ShowPassword bool
}

// signupRole has no default. Beta testers left a preselected "DJ" alone, so
// signup makes everyone pick.
func signupRole(s string) string {
	if s == roleDJ || s == roleAudience {
		return s
	}
	return ""
}

func (a *app) handleSigninForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, http.StatusOK, "signin.html", signinForm{Next: r.URL.Query().Get("next")})
}

func (a *app) handleSignupForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	q := r.URL.Query()
	a.render(w, r, http.StatusOK, "signin.html", signinForm{Next: q.Get("next"), Signup: true, Role: signupRole(q.Get("role"))})
}

type linkSentPage struct {
	Email string
	Link  string // set only for a local dev request that opted in; never in production
}

func (a *app) handleLinkRequest(w http.ResponseWriter, r *http.Request) {
	form := signinForm{
		Email: normalizeEmail(r.PostFormValue("email")),
		Next:  r.PostFormValue("next"),
	}
	if r.PostFormValue("signup") == "1" {
		form.Signup = true
		form.Role = signupRole(r.PostFormValue("role"))
	}
	fail := func(status int, msg string) {
		form.Error = msg
		a.render(w, r, status, "signin.html", form)
	}

	if !a.linkIPLimiter.allow(a.clientIP(r)) {
		fail(http.StatusTooManyRequests, "Too many sign-in emails from this network. Try again later.")
		return
	}
	if a.turnstileFailed(r) {
		fail(http.StatusForbidden, turnstileRejectedMessage)
		return
	}
	if !validEmail(form.Email) {
		fail(http.StatusUnprocessableEntity, "Enter a valid email address.")
		return
	}
	if form.Signup && form.Role == "" {
		fail(http.StatusUnprocessableEntity, "Choose whether you're a DJ/KJ or an audience member.")
		return
	}

	role := roleAudience
	if form.Signup {
		role = form.Role
	}
	u, err := a.findOrCreateUser(form.Email, role)
	if err != nil {
		a.serverError(w, r, err)
		return
	}

	// Past the per-address limit the page still says the email is on its way, as Rails did.
	var devLink string
	if a.linkEmailLimiter.allow(u.Email) {
		link, err := a.sendMagicLink(u, safeNext(form.Next))
		if err != nil {
			a.logger.Error("server error", "method", r.Method, "path", logPath(r.URL.Path), "err", err)
			a.message(w, r, http.StatusInternalServerError, "We couldn't send your sign-in link",
				"Something went wrong while sending the email. Please try again in a moment.")
			return
		}
		if a.showMagicLink() {
			devLink = link
		}
	}
	a.render(w, r, http.StatusOK, "signin_sent.html", linkSentPage{Email: u.Email, Link: devLink})
}

// showMagicLink is the explicit local-dev opt-in for putting a sign-in URL on
// the check-your-email page. Production never qualifies, even if the flag is set.
func (a *app) showMagicLink() bool {
	return a.cfg.Env == "dev" && a.cfg.SMTPHost == "" && a.cfg.DevShowMagicLink
}

// findOrCreateUser returns the account for email. A new account gets role, and a
// new DJ gets a generated handle they can change on the Account page.
func (a *app) findOrCreateUser(email, role string) (User, error) {
	var u User
	err := a.db.Take(&u, "email = ?", email).Error
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return u, err
	}
	u = User{Email: email, Role: role}
	if role == roleDJ {
		handle, err := a.newHandle()
		if err != nil {
			return u, err
		}
		u.Slug = &handle
	}
	if err := a.db.Create(&u).Error; err != nil {
		// Another request may have created it first.
		if err2 := a.db.Take(&u, "email = ?", email).Error; err2 == nil {
			return u, nil
		}
		return u, err
	}
	return u, nil
}

const handleChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// newHandle returns an unused "dj-xxxxxx" handle, as Rails' ensure_slug! did.
func (a *app) newHandle() (string, error) {
	for range 10 {
		b := make([]byte, 6)
		if _, err := randRead(b); err != nil {
			return "", err
		}
		for i := range b {
			b[i] = handleChars[int(b[i])%len(handleChars)]
		}
		handle := "dj-" + string(b)
		var n int64
		if err := a.db.Model(&User{}).Where("slug = ?", handle).Count(&n).Error; err != nil {
			return "", err
		}
		if n == 0 {
			return handle, nil
		}
	}
	return "", errors.New("could not find a free handle")
}

// sendMagicLink replaces any earlier link for the user, so only the newest one works.
func (a *app) sendMagicLink(u User, next string) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	now := a.now()
	err = a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", u.ID).Delete(&MagicToken{}).Error; err != nil {
			return err
		}
		return tx.Create(&MagicToken{UserID: u.ID, TokenHash: hashToken(token), ExpiresAt: now.Add(magicLinkLifetime)}).Error
	})
	if err != nil {
		return "", err
	}

	link := a.cfg.BaseURL + "/auth/" + token
	if next != "/" {
		link += "?next=" + url.QueryEscape(next)
	}
	intro := "Visit the link below to log in to DJMagic.io:"
	if u.EmailConfirmedAt == nil {
		intro = "Welcome to DJMagic.io! Visit the link below to confirm your email and log in:"
	}
	body := intro + "\n\n" + link + "\n\n" +
		"This link expires in 10 minutes and can only be used once.\n\n" +
		"This link was requested on " + now.UTC().Format("January 2, 2006 at 3:04 PM") + " UTC. " +
		"If you didn't ask for it, you can ignore this email.\n"
	if err := a.mailer.Send(u.Email, "Your DJMagic.io login link", body); err != nil {
		return "", err
	}
	return link, nil
}

type linkConfirmPage struct {
	Valid bool
	Email string
	Next  string
}

// handleLinkConfirm does not sign in on GET. Email security scanners open links
// before the person does and would use up the token. The page posts the form itself
// when JavaScript runs; the button still works without it.
func (a *app) handleLinkConfirm(w http.ResponseWriter, r *http.Request) {
	page := linkConfirmPage{Next: r.URL.Query().Get("next")}
	var mt MagicToken
	err := a.db.Take(&mt, "token_hash = ? AND consumed_at IS NULL AND expires_at > ?", hashToken(r.PathValue("token")), a.now()).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		a.serverError(w, r, err)
		return
	}
	if err == nil {
		var u User
		if err := a.db.Take(&u, mt.UserID).Error; err != nil {
			a.serverError(w, r, err)
			return
		}
		page.Valid = true
		page.Email = u.Email
	}
	status := http.StatusOK
	if !page.Valid {
		status = http.StatusGone
	}
	a.render(w, r, status, "magic_link.html", page)
}

func (a *app) handleLinkRedeem(w http.ResponseWriter, r *http.Request) {
	hash := hashToken(r.PathValue("token"))
	now := a.now()

	// A single conditional update consumes the token, so two clicks can't both succeed.
	res := a.db.Model(&MagicToken{}).
		Where("token_hash = ? AND consumed_at IS NULL AND expires_at > ?", hash, now).
		Update("consumed_at", now)
	if res.Error != nil {
		a.serverError(w, r, res.Error)
		return
	}
	if res.RowsAffected == 0 {
		a.render(w, r, http.StatusGone, "magic_link.html", linkConfirmPage{})
		return
	}

	var mt MagicToken
	if err := a.db.Take(&mt, "token_hash = ?", hash).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	// The link reached the mailbox: that confirms the address and unlocks password login.
	err := a.db.Exec(`UPDATE users SET email_confirmed_at = COALESCE(email_confirmed_at, ?),
		failed_login_attempts = 0, locked_at = NULL, updated_at = ? WHERE id = ?`, now, now, mt.UserID).Error
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if err := a.startSession(w, r, mt.UserID); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.linkGuestOrLog(w, r, mt.UserID)
	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

// Password login

const (
	badPasswordMessage = "Email or password is incorrect. No password yet? Use an email link instead."
	lockedMessage      = "Account locked. Use an email link to sign in and unlock your account."
)

func (a *app) handlePasswordLogin(w http.ResponseWriter, r *http.Request) {
	form := signinForm{
		Email:        normalizeEmail(r.PostFormValue("email")),
		Next:         r.PostFormValue("next"),
		ShowPassword: true,
	}
	fail := func(status int, msg string) {
		form.Error = msg
		a.render(w, r, status, "signin.html", form)
	}

	if !a.loginLimiter.allow(a.clientIP(r)) {
		fail(http.StatusTooManyRequests, "Too many login attempts. Wait a minute and try again.")
		return
	}
	if a.turnstileFailed(r) {
		fail(http.StatusForbidden, turnstileRejectedMessage)
		return
	}

	var u User
	err := a.db.Take(&u, "email = ?", form.Email).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		a.serverError(w, r, err)
		return
	}
	found := err == nil

	hash := dummyHash()
	if found && u.HasPassword() {
		hash = []byte(u.PasswordHash)
	}
	passwordOK := bcrypt.CompareHashAndPassword(hash, []byte(r.PostFormValue("password"))) == nil

	if found && u.LockedAt != nil {
		fail(http.StatusForbidden, lockedMessage)
		return
	}
	if !found || !u.HasPassword() {
		fail(http.StatusUnauthorized, badPasswordMessage)
		return
	}
	if !passwordOK {
		now := a.now()
		err := a.db.Exec(`UPDATE users SET failed_login_attempts = failed_login_attempts + 1,
			locked_at = CASE WHEN failed_login_attempts + 1 >= ? THEN ? ELSE locked_at END
			WHERE id = ?`, maxFailedLogins, now, u.ID).Error
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		fail(http.StatusUnauthorized, badPasswordMessage)
		return
	}

	if u.FailedLoginAttempts > 0 {
		if err := a.db.Model(&u).Update("failed_login_attempts", 0).Error; err != nil {
			a.serverError(w, r, err)
			return
		}
	}
	if err := a.startSession(w, r, u.ID); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.linkGuestOrLog(w, r, u.ID)
	http.Redirect(w, r, safeNext(form.Next), http.StatusSeeOther)
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if id := currentSessionID(r); id != "" {
		if err := a.db.Delete(&Session{}, "id = ?", id).Error; err != nil {
			a.serverError(w, r, err)
			return
		}
	}
	a.clearSessionCookie(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
