package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// browser keeps cookies across requests to one app, like a single browser would.
type browser struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]*http.Cookie
	ip      string
}

func newBrowser(t *testing.T, a *app) *browser {
	return &browser{t: t, h: a.routes(), cookies: map[string]*http.Cookie{}, ip: "192.0.2.1"}
}

func (b *browser) do(req *http.Request) *httptest.ResponseRecorder {
	b.t.Helper()
	req.RemoteAddr = b.ip + ":1234"
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return rec
}

func (b *browser) get(target string) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.do(httptest.NewRequest(http.MethodGet, target, nil))
}

// post sends a form. A Turnstile token is added unless the test sets one.
func (b *browser) post(target string, form url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if !form.Has(turnstileField) {
		form.Set(turnstileField, "ok")
	}
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(req)
}

var linkPattern = regexp.MustCompile(`http://example\.test(/auth/[A-Za-z0-9_-]+(?:\?next=\S+)?)`)

// lastLink returns the path of the newest sign-in link emailed to `to`.
func lastLink(t *testing.T, a *app, to string) string {
	t.Helper()
	sent := a.mailer.(*testMailer).all()
	for i := len(sent) - 1; i >= 0; i-- {
		if sent[i].To == to {
			m := linkPattern.FindStringSubmatch(sent[i].Body)
			if m == nil {
				t.Fatalf("no link in email:\n%s", sent[i].Body)
			}
			return m[1]
		}
	}
	t.Fatalf("no email to %s", to)
	return ""
}

func mailCount(a *app, to string) int {
	n := 0
	for _, m := range a.mailer.(*testMailer).all() {
		if m.To == to {
			n++
		}
	}
	return n
}

// signIn requests a link for email (as a signup with role when role is set) and redeems it.
func signIn(t *testing.T, a *app, b *browser, email, role string) User {
	t.Helper()
	form := url.Values{"email": {email}}
	if role != "" {
		form.Set("signup", "1")
		form.Set("role", role)
	}
	if rec := b.post("/signin/link", form); rec.Code != http.StatusOK {
		t.Fatalf("link request: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := b.post(lastLink(t, a, normalizeEmail(email)), nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("redeem: %d\n%s", rec.Code, rec.Body.String())
	}
	var u User
	if err := a.db.Take(&u, "email = ?", normalizeEmail(email)).Error; err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSignupAsDJ(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)

	rec := b.post("/signin/link", url.Values{"email": {" New@Example.com "}, "signup": {"1"}, "role": {"dj"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "We sent a sign-in link to <strong>new@example.com</strong>") {
		t.Fatalf("got %d:\n%s", rec.Code, rec.Body.String())
	}
	mail := a.mailer.(*testMailer).all()
	if len(mail) != 1 || mail[0].Subject != "Your DJMagic.io login link" || !strings.Contains(mail[0].Body, "Welcome to DJMagic.io!") ||
		!strings.Contains(mail[0].Body, "expires in 10 minutes") {
		t.Fatalf("email = %+v", mail)
	}

	var u User
	if err := a.db.Take(&u, "email = ?", "new@example.com").Error; err != nil {
		t.Fatal(err)
	}
	if u.Role != roleDJ || !regexp.MustCompile(`^dj-[a-z0-9]{6}$`).MatchString(u.Handle()) || u.EmailConfirmedAt != nil {
		t.Fatalf("user = %+v handle %q", u, u.Handle())
	}

	link := lastLink(t, a, "new@example.com")
	// Opening the link only shows the confirm page; a mail scanner can't use it up.
	for range 2 {
		rec = b.get(link)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Continue as new@example.com") {
			t.Fatalf("confirm page: %d\n%s", rec.Code, rec.Body.String())
		}
	}
	rec = b.post(link, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("redeem: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(b.get("/").Body.String(), "Welcome back!") {
		t.Fatal("not signed in after redeeming")
	}
	a.db.Take(&u, u.ID)
	if u.EmailConfirmedAt == nil {
		t.Fatal("email not confirmed")
	}

	// Single use.
	other := newBrowser(t, a)
	if rec := other.post(link, nil); rec.Code != http.StatusGone {
		t.Fatalf("second redeem: %d", rec.Code)
	}
	if rec := other.get(link); rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "expired or already been used") {
		t.Fatalf("used link page: %d", rec.Code)
	}

	// An existing account gets the plain sign-in email, and keeps its role.
	b2 := newBrowser(t, a)
	b2.post("/signin/link", url.Values{"email": {"new@example.com"}, "signup": {"1"}, "role": {"audience"}})
	if body := a.mailer.(*testMailer).all()[1].Body; !strings.HasPrefix(body, "Visit the link below to log in to DJMagic.io:") {
		t.Fatalf("returning email:\n%s", body)
	}
	a.db.Take(&u, u.ID)
	if u.Role != roleDJ {
		t.Fatalf("role changed to %q", u.Role)
	}
}

func TestSigninWithUnknownEmailCreatesAudienceAccount(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	u := signIn(t, a, b, "fan@example.com", "")
	if u.Role != roleAudience || u.Slug != nil {
		t.Fatalf("user = %+v", u)
	}
	if body := b.get("/").Body.String(); !strings.Contains(body, "Start hosting") {
		t.Fatalf("audience dashboard:\n%s", body)
	}
}

func TestSignupMakesYouChooseARole(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	if body := b.get("/signup").Body.String(); !strings.Contains(body, `name="role" value="dj" required>`) || strings.Contains(body, "checked") {
		t.Fatalf("signup should preselect nothing:\n%s", body)
	}
	if body := b.get("/signup?role=audience").Body.String(); !strings.Contains(body, `value="audience" required checked>`) {
		t.Fatalf("signup audience:\n%s", body)
	}

	for _, role := range []string{"", "admin"} {
		rec := b.post("/signin/link", url.Values{"email": {"a@example.com"}, "signup": {"1"}, "role": {role}})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Choose whether") ||
			!strings.Contains(rec.Body.String(), `value="a@example.com"`) {
			t.Fatalf("role %q: %d\n%s", role, rec.Code, rec.Body.String())
		}
	}
	var n int64
	a.db.Model(&User{}).Count(&n)
	if n != 0 || len(a.mailer.(*testMailer).all()) != 0 {
		t.Fatal("signup without a role created a user or sent mail")
	}

	if rec := b.post("/signin/link", url.Values{"email": {"fan@example.com"}, "signup": {"1"}, "role": {"audience"}}); rec.Code != http.StatusOK {
		t.Fatalf("audience signup: %d", rec.Code)
	}
	var u User
	a.db.Take(&u, "email = ?", "fan@example.com")
	if u.Role != roleAudience || u.Slug != nil {
		t.Fatalf("user = %+v", u)
	}
	if body := b.get("/signin").Body.String(); strings.Contains(body, `name="role"`) || !strings.Contains(body, "Sign in with a password instead") {
		t.Fatalf("signin:\n%s", body)
	}
}

func TestLinkRequestRejectsBadEmail(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	for _, email := range []string{"", "nope", "Bob <bob@example.com>"} {
		if rec := b.post("/signin/link", url.Values{"email": {email}}); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%q: %d", email, rec.Code)
		}
	}
	var n int64
	a.db.Model(&User{}).Count(&n)
	if n != 0 {
		t.Fatalf("%d users created", n)
	}
}

func TestTurnstileFailureCreatesNothing(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	rec := b.post("/signin/link", url.Values{"email": {"a@example.com"}, turnstileField: {"bad"}})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), turnstileRejectedMessage) {
		t.Fatalf("got %d", rec.Code)
	}
	rec = b.post("/signin", url.Values{"email": {"a@example.com"}, "password": {"whatever1"}, turnstileField: {"bad"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("password form: %d", rec.Code)
	}
	var n int64
	a.db.Model(&User{}).Count(&n)
	if n != 0 || len(a.mailer.(*testMailer).all()) != 0 {
		t.Fatal("a rejected request created a user or sent mail")
	}
}

func TestMagicLinkExpiresAfterTenMinutes(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	start := time.Now()
	a.now = func() time.Time { return start }
	b.post("/signin/link", url.Values{"email": {"a@example.com"}})
	link := lastLink(t, a, "a@example.com")

	a.now = func() time.Time { return start.Add(magicLinkLifetime + time.Second) }
	if rec := b.get(link); rec.Code != http.StatusGone {
		t.Fatalf("confirm page: %d", rec.Code)
	}
	if rec := b.post(link, nil); rec.Code != http.StatusGone {
		t.Fatalf("redeem: %d", rec.Code)
	}
}

func TestNewLinkReplacesOlderOne(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	b.post("/signin/link", url.Values{"email": {"a@example.com"}})
	first := lastLink(t, a, "a@example.com")
	b.post("/signin/link", url.Values{"email": {"a@example.com"}})
	second := lastLink(t, a, "a@example.com")
	if rec := b.post(first, nil); rec.Code != http.StatusGone {
		t.Fatalf("old link: %d", rec.Code)
	}
	if rec := b.post(second, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("new link: %d", rec.Code)
	}
}

func TestMagicLinkKeepsSafeNext(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	b.post("/signin/link", url.Values{"email": {"a@example.com"}, "next": {"/account"}})
	link := lastLink(t, a, "a@example.com")
	if !strings.HasSuffix(link, "?next=%2Faccount") {
		t.Fatalf("link = %s", link)
	}
	// The confirm page carries next into the form it posts.
	u, _ := url.Parse(link)
	if body := b.get(link).Body.String(); !strings.Contains(body, `name="next" value="/account"`) {
		t.Fatalf("confirm page:\n%s", body)
	}
	if rec := b.post(u.Path, url.Values{"next": {"/account"}}); rec.Header().Get("Location") != "/account" {
		t.Fatalf("redirect = %q", rec.Header().Get("Location"))
	}

	b.post("/signin/link", url.Values{"email": {"b@example.com"}, "next": {"//evil.example"}})
	if link := lastLink(t, a, "b@example.com"); strings.Contains(link, "next") {
		t.Fatalf("unsafe next kept: %s", link)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "/",
		"/account":             "/account",
		"/a?b=c":               "/a?b=c",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"https://evil.example": "/",
		"account":              "/",
		"/a\nb":                "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPasswordLoginLockoutAndUnlock(t *testing.T) {
	a := newTestApp(t)
	a.loginLimiter = newRateLimiter(1000, time.Minute, time.Now)
	b := newBrowser(t, a)
	signIn(t, a, b, "dj@example.com", "dj")

	if rec := b.post("/account/password", url.Values{"password": {"correct horse"}, "confirm": {"correct horse"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("set password: %d\n%s", rec.Code, rec.Body.String())
	}
	b.post("/logout", nil)

	login := func(pw string) *httptest.ResponseRecorder {
		return newBrowser(t, a).post("/signin", url.Values{"email": {"DJ@example.com"}, "password": {pw}})
	}
	if rec := login("correct horse"); rec.Code != http.StatusSeeOther {
		t.Fatalf("login: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := login("nope nope"); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Email or password is incorrect") {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	// The failure count from above plus nine more reaches ten.
	for range maxFailedLogins - 1 {
		login("nope nope")
	}
	rec := login("correct horse")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "Account locked") {
		t.Fatalf("locked account with right password: %d\n%s", rec.Code, rec.Body.String())
	}

	// An email link unlocks it.
	signIn(t, a, newBrowser(t, a), "dj@example.com", "")
	if rec := login("correct horse"); rec.Code != http.StatusSeeOther {
		t.Fatalf("after unlock: %d", rec.Code)
	}
	var u User
	a.db.Take(&u, "email = ?", "dj@example.com")
	if u.LockedAt != nil || u.FailedLoginAttempts != 0 {
		t.Fatalf("user = %+v", u)
	}
}

func TestPasswordLoginWithoutPasswordOrAccount(t *testing.T) {
	a := newTestApp(t)
	signIn(t, a, newBrowser(t, a), "linkonly@example.com", "")
	for _, email := range []string{"linkonly@example.com", "nobody@example.com"} {
		rec := newBrowser(t, a).post("/signin", url.Values{"email": {email}, "password": {"whatever1"}})
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), badPasswordMessage) {
			t.Errorf("%s: %d", email, rec.Code)
		}
	}
}

func TestLinkRequestRateLimits(t *testing.T) {
	a := newTestApp(t)

	// Per address: past 5 an hour the page looks the same but nothing is sent.
	for i := range 7 {
		b := newBrowser(t, a)
		b.ip = "192.0.2." + string(rune('1'+i))
		if rec := b.post("/signin/link", url.Values{"email": {"a@example.com"}}); rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	if n := mailCount(a, "a@example.com"); n != 5 {
		t.Fatalf("sent %d emails, want 5", n)
	}

	// Per IP: 10 an hour.
	b := newBrowser(t, a)
	b.ip = "198.51.100.7"
	for i := range 10 {
		b.post("/signin/link", url.Values{"email": {"u" + string(rune('a'+i)) + "@example.com"}})
	}
	rec := b.post("/signin/link", url.Values{"email": {"late@example.com"}})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("11th from one IP: %d", rec.Code)
	}
}

func TestRequireUserRedirectsToSignin(t *testing.T) {
	a := newTestApp(t)
	rec := newBrowser(t, a).get("/account")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/signin?next=%2Faccount" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSignedInUserSkipsSigninPages(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	signIn(t, a, b, "a@example.com", "")
	for _, path := range []string{"/signin", "/signup"} {
		if rec := b.get(path); rec.Code != http.StatusSeeOther {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
}

func TestLogout(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	signIn(t, a, b, "a@example.com", "")
	if rec := b.post("/logout", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", rec.Code)
	}
	if body := b.get("/").Body.String(); !strings.Contains(body, "Make the queue visible") {
		t.Fatal("still signed in after logout")
	}
	var n int64
	a.db.Model(&Session{}).Count(&n)
	if n != 0 {
		t.Fatalf("%d sessions left", n)
	}
}

func TestExpiredSessionIsSignedOut(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	signIn(t, a, b, "a@example.com", "")
	a.now = func() time.Time { return time.Now().Add(sessionLifetime + time.Minute) }
	if rec := b.get("/account"); rec.Code != http.StatusSeeOther {
		t.Fatalf("expired session: %d", rec.Code)
	}
}

func TestLogPathHidesMagicToken(t *testing.T) {
	if got := logPath("/auth/abc123"); got != "/auth/{token}" {
		t.Fatalf("logPath = %q", got)
	}
	if got := logPath("/account"); got != "/account" {
		t.Fatalf("logPath = %q", got)
	}
}

func TestCleanupRemovesUnconfirmedAccounts(t *testing.T) {
	a := newTestApp(t)
	signIn(t, a, newBrowser(t, a), "real@example.com", "")
	newBrowser(t, a).post("/signin/link", url.Values{"email": {"typo@example.con"}})

	a.cleanupOnce()
	var n int64
	a.db.Model(&User{}).Count(&n)
	if n != 2 {
		t.Fatalf("cleanup too early: %d users", n)
	}

	a.now = func() time.Time { return time.Now().Add(unconfirmedLifetime + time.Hour) }
	a.cleanupOnce()
	var emails []string
	a.db.Model(&User{}).Pluck("email", &emails)
	if len(emails) != 1 || emails[0] != "real@example.com" {
		t.Fatalf("users left: %v", emails)
	}
	a.db.Model(&MagicToken{}).Count(&n)
	if n != 0 {
		t.Fatalf("%d tokens left", n)
	}
}
