package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func feedbackValues(message, token string) url.Values {
	v := url.Values{"category": {"bug"}, "message": {message}}
	if token != "" {
		v.Set(csrfFormField, token)
	}
	return v
}

func feedbackCount(t *testing.T, a *app) int {
	t.Helper()
	var n int64
	if err := a.db.Model(&Feedback{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func postFeedback(b *browser, body url.Values, header string) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if header != "" {
		req.Header.Set(csrfHeader, header)
	}
	return b.do(req)
}

func assertPOSTFormsHaveCSRF(t *testing.T, body string) {
	t.Helper()
	rest := body
	forms := 0
	for {
		i := strings.Index(rest, "<form")
		if i < 0 {
			break
		}
		rest = rest[i:]
		end := strings.Index(rest, "</form>")
		if end < 0 {
			t.Fatal("unclosed form")
		}
		form := rest[:end]
		rest = rest[end+len("</form>"):]
		if !strings.Contains(form, `method="post"`) {
			continue
		}
		forms++
		field := strings.Index(form, `name="csrf_token"`)
		if field < 0 {
			t.Fatalf("post form missing csrf token:\n%s", form)
		}
		if strings.Contains(form, `type="file"`) && strings.Index(form, `type="file"`) < field {
			t.Fatal("file input precedes the csrf field")
		}
	}
	if forms == 0 {
		t.Fatal("expected a POST form")
	}
}

func TestCSRFFieldEscapesToken(t *testing.T) {
	got := string(csrfFieldHTML(`"><script>alert(1)</script>`))
	if strings.Contains(got, "<script>") || strings.Contains(got, `value=""><`) {
		t.Fatalf("token was not escaped: %s", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") || !strings.Contains(got, "&#34;") {
		t.Fatalf("escaped field: %s", got)
	}
	if !strings.HasPrefix(got, `<input type="hidden" name="csrf_token" value="`) {
		t.Fatalf("field: %s", got)
	}
}

func TestCSRFAcceptsValidToken(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	page := b.get("/feedback")
	if page.Code != http.StatusOK {
		t.Fatalf("form: %d", page.Code)
	}
	assertPOSTFormsHaveCSRF(t, page.Body.String())
	if !strings.Contains(page.Body.String(), `<meta name="csrf-token" content="`+b.csrf+`">`) {
		t.Fatal("meta token does not match the form")
	}
	rec := b.post("/feedback", feedbackValues("hello", ""))
	if rec.Code != http.StatusOK || feedbackCount(t, a) != 1 {
		t.Fatalf("valid post: %d rows=%d\n%s", rec.Code, feedbackCount(t, a), rec.Body.String())
	}
	// The same token still works. It is not consumed to prevent a double submit.
	again := b.post("/feedback", feedbackValues("again", ""))
	if again.Code != http.StatusOK || feedbackCount(t, a) != 2 {
		t.Fatalf("second post: %d rows=%d", again.Code, feedbackCount(t, a))
	}
}

func TestCSRFRejectsMissingInvalidAndMalformedTokens(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	b.get("/feedback")
	token := b.csrf

	b.skipCSRF = true
	missing := postFeedback(b, feedbackValues("nope", ""), "")
	if missing.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
		t.Fatalf("missing: %d rows=%d", missing.Code, feedbackCount(t, a))
	}
	if !strings.Contains(missing.Body.String(), csrfFailureMessage) || !strings.Contains(missing.Body.String(), "Reload this page") {
		t.Fatalf("body: %s", missing.Body.String())
	}
	if missing.Header().Get("Cache-Control") != "private, no-store" || missing.Header().Get("X-CSRF-Rejected") != "1" {
		t.Fatalf("reject headers: %v", missing.Header())
	}

	bad := postFeedback(b, feedbackValues("nope", ""), "not-the-token")
	if bad.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
		t.Fatalf("invalid: %d rows=%d", bad.Code, feedbackCount(t, a))
	}

	for _, malformed := range []string{"", `"<script>`, "@@@", strings.Repeat("a", csrfTokenMaxLen+1)} {
		rec := postFeedback(b, feedbackValues("nope", malformed), malformed)
		if rec.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
			t.Fatalf("malformed %q: %d rows=%d", malformed, rec.Code, feedbackCount(t, a))
		}
	}

	// A real token in the query string is not a substitute for the body or header.
	req := httptest.NewRequest(http.MethodPost, "/feedback?"+csrfFormField+"="+url.QueryEscape(token),
		strings.NewReader(feedbackValues("query", "").Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if rec := b.do(req); rec.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
		t.Fatalf("query token: %d rows=%d", rec.Code, feedbackCount(t, a))
	}

	// A JSON body is not parsed for a token.
	jsonReq := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(
		fmt.Sprintf(`{"category":"bug","message":"json","%s":"%s"}`, csrfFormField, token)))
	jsonReq.Header.Set("Content-Type", "application/json")
	if rec := b.do(jsonReq); rec.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
		t.Fatalf("json: %d rows=%d", rec.Code, feedbackCount(t, a))
	}

	// The form field alone, with no header, is enough.
	field := postFeedback(b, feedbackValues("from-field", token), "")
	if field.Code != http.StatusOK || feedbackCount(t, a) != 1 {
		t.Fatalf("form field: %d rows=%d\n%s", field.Code, feedbackCount(t, a), field.Body.String())
	}
}

func TestCSRFRejectsCrossSessionToken(t *testing.T) {
	a := newTestApp(t)
	one := newBrowser(t, a)
	two := newBrowser(t, a)
	one.ip = "192.0.2.11"
	two.ip = "192.0.2.12"
	one.get("/feedback")
	two.get("/feedback")
	one.skipCSRF = true
	rec := postFeedback(one, feedbackValues("swap", ""), two.csrf)
	if rec.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
		t.Fatalf("cross-session: %d rows=%d", rec.Code, feedbackCount(t, a))
	}
}

func TestCSRFSafeMethodsStayOpen(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	b.skipCSRF = true
	if rec := b.get("/feedback"); rec.Code != http.StatusOK {
		t.Fatalf("GET: %d", rec.Code)
	}
	for _, method := range []string{http.MethodHead, http.MethodOptions} {
		req := httptest.NewRequest(method, "/feedback", nil)
		rec := b.do(req)
		if rec.Code == http.StatusForbidden {
			t.Fatalf("%s was rejected as CSRF", method)
		}
	}
}

func TestCSRFProtectsOtherUnsafeMethods(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	b.get("/feedback")
	for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		b.skipCSRF = true
		req := httptest.NewRequest(method, "/feedback", nil)
		if rec := b.do(req); rec.Code != http.StatusForbidden {
			t.Fatalf("%s without token: %d", method, rec.Code)
		}
		b.skipCSRF = false
		req = httptest.NewRequest(method, "/feedback", nil)
		rec := b.do(req)
		// The catch-all route answers 404. Reaching it means the token was accepted.
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s with token: %d, want 404 so the mux was reached", method, rec.Code)
		}
	}

	b.skipCSRF = true
	if rec := b.post("/healthz", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("POST /healthz without token: %d", rec.Code)
	}
	b.skipCSRF = false
	if rec := b.post("/healthz", nil); rec.Code == http.StatusForbidden || rec.Code == http.StatusOK {
		t.Fatalf("POST /healthz with token: %d", rec.Code)
	}
}

func TestCSRFAnonymousAndAuthenticatedSessions(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	if rec := b.post("/feedback", feedbackValues("guest", "")); rec.Code != http.StatusOK {
		t.Fatalf("anonymous: %d\n%s", rec.Code, rec.Body.String())
	}
	var guest Feedback
	if err := a.db.First(&guest).Error; err != nil || guest.UserID != nil {
		t.Fatalf("guest feedback: %+v %v", guest, err)
	}

	u := signIn(t, a, b, "fan@example.com", roleAudience)
	b.get("/account")
	anonWouldBe := b.csrf
	if rec := b.post("/feedback", feedbackValues("member", "")); rec.Code != http.StatusOK {
		t.Fatalf("authenticated: %d\n%s", rec.Code, rec.Body.String())
	}
	var member Feedback
	if err := a.db.Last(&member).Error; err != nil || member.UserID == nil || *member.UserID != u.ID {
		t.Fatalf("member feedback: %+v %v", member, err)
	}

	// A second tab with the same cookies keeps working.
	tab := newBrowser(t, a)
	tab.ip = b.ip
	for name, c := range b.cookies {
		tab.cookies[name] = c
	}
	tab.csrf = anonWouldBe
	if rec := tab.post("/feedback", feedbackValues("other tab", "")); rec.Code != http.StatusOK || feedbackCount(t, a) != 3 {
		t.Fatalf("second tab: %d rows=%d", rec.Code, feedbackCount(t, a))
	}
}

func TestCSRFLoginLogoutAndExpiry(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	b.get("/signin")
	beforeLogin := b.csrf
	if beforeLogin == "" {
		t.Fatal("signin page did not issue a token")
	}
	signIn(t, a, b, "a@example.com", "")
	b.get("/account")
	afterLogin := b.csrf
	if afterLogin == "" || afterLogin == beforeLogin {
		t.Fatalf("login did not rotate the token (%q -> %q)", beforeLogin, afterLogin)
	}

	b.skipCSRF = true
	stale := postFeedback(b, feedbackValues("stale", ""), beforeLogin)
	if stale.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
		t.Fatalf("pre-login token: %d rows=%d", stale.Code, feedbackCount(t, a))
	}
	b.skipCSRF = false

	sessionToken := b.csrf
	if rec := b.post("/logout", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", rec.Code)
	}
	b.skipCSRF = true
	if rec := postFeedback(b, feedbackValues("after logout", ""), sessionToken); rec.Code != http.StatusForbidden {
		t.Fatalf("logged-out session token: %d", rec.Code)
	}
	if rec := b.get("/account"); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout did not end the session: %d", rec.Code)
	}

	// A new anonymous token works, and an expired anonymous row does not.
	b.skipCSRF = false
	b.csrf = ""
	if rec := b.post("/feedback", feedbackValues("fresh", "")); rec.Code != http.StatusOK {
		t.Fatalf("after logout: %d\n%s", rec.Code, rec.Body.String())
	}
	var row CSRFSession
	if err := a.db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.db.Model(&row).Update("expires_at", a.now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if rec := b.post("/feedback", feedbackValues("expired", "")); rec.Code != http.StatusForbidden {
		t.Fatalf("expired anonymous token: %d", rec.Code)
	}

	b.csrf = ""
	signIn(t, a, b, "a@example.com", "")
	var s Session
	if err := a.db.Where("user_id = ?", currentUserID(t, a, "a@example.com")).First(&s).Error; err != nil {
		t.Fatal(err)
	}
	b.get("/account")
	if err := a.db.Model(&s).Update("expires_at", a.now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	if rec := b.post("/account/password", url.Values{"password": {"long enough"}, "confirm": {"long enough"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("expired session: %d\n%s", rec.Code, rec.Body.String())
	}
	var u User
	a.db.Where("email = ?", "a@example.com").First(&u)
	if u.HasPassword() {
		t.Fatal("expired session still changed the password")
	}
}

func currentUserID(t *testing.T, a *app, email string) uint {
	t.Helper()
	var u User
	if err := a.db.Where("email = ?", email).First(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestCSRFSessionRegenerationDropsOldToken(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	signIn(t, a, b, "a@example.com", "")
	b.get("/account")
	first := b.csrf
	signIn(t, a, b, "a@example.com", "")
	b.skipCSRF = true
	rec := postFeedback(b, feedbackValues("old session", ""), first)
	if rec.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
		t.Fatalf("regenerated session accepted the old token: %d rows=%d", rec.Code, feedbackCount(t, a))
	}
}

func TestCSRFTemplatesAndHTMX(t *testing.T) {
	a := newTestApp(t)
	host, _, path := audienceShow(t, a, url.Values{"name": {"Friday"}, "manual_entry_enabled": {"1"}})
	anon := guest(t, a, "192.0.2.70")
	showPage := anon.get(path)
	if showPage.Code != http.StatusOK {
		t.Fatalf("show: %d", showPage.Code)
	}
	assertPOSTFormsHaveCSRF(t, showPage.Body.String())

	fan := guest(t, a, "192.0.2.71")
	signIn(t, a, fan, "fan@example.com", roleAudience)
	saved := fan.get("/me/saved")
	assertPOSTFormsHaveCSRF(t, saved.Body.String())

	account := host.get("/account")
	assertPOSTFormsHaveCSRF(t, account.Body.String())

	h := a.routes()
	history := httptest.NewRequest(http.MethodGet, "/feedback", nil)
	history.Header.Set("HX-Request", "true")
	history.Header.Set("HX-History-Restore-Request", "true")
	historyRec := httptest.NewRecorder()
	h.ServeHTTP(historyRec, history)
	if historyRec.Code != http.StatusOK || !strings.Contains(historyRec.Body.String(), "<html") || !strings.Contains(historyRec.Body.String(), `name="csrf-token"`) {
		t.Fatalf("history restore: %d html=%v", historyRec.Code, strings.Contains(historyRec.Body.String(), "<html"))
	}

	fragment := httptest.NewRequest(http.MethodGet, "/feedback", nil)
	fragment.Header.Set("HX-Request", "true")
	fragment.Header.Set("HX-Target", "content")
	fragmentRec := httptest.NewRecorder()
	h.ServeHTTP(fragmentRec, fragment)
	if strings.Contains(fragmentRec.Body.String(), "<html") || !strings.Contains(fragmentRec.Body.String(), `name="csrf_token"`) {
		t.Fatal("fragment lost the csrf field or included a document")
	}

	b := newBrowser(t, a)
	b.get("/feedback")
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(feedbackValues("htmx", "").Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "content")
	if rec := b.do(req); rec.Code != http.StatusOK {
		t.Fatalf("htmx post: %d\n%s", rec.Code, rec.Body.String())
	}

	b.skipCSRF = true
	bad := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(feedbackValues("htmx-bad", "").Encode()))
	bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bad.Header.Set("HX-Request", "true")
	badRec := b.do(bad)
	if badRec.Code != http.StatusForbidden || !strings.Contains(badRec.Body.String(), csrfFailureMessage) {
		t.Fatalf("htmx reject: %d %s", badRec.Code, badRec.Body.String())
	}

	boosted := httptest.NewRequest(http.MethodGet, "/feedback", nil)
	boosted.Header.Set("HX-Request", "true")
	boosted.Header.Set("HX-Boosted", "true")
	boosted.Header.Set("HX-Target", "body")
	boostedRec := httptest.NewRecorder()
	h.ServeHTTP(boostedRec, boosted)
	if !strings.Contains(boostedRec.Body.String(), `name="csrf-token"`) || !strings.Contains(boostedRec.Body.String(), "<html") {
		t.Fatal("boosted navigation lost the csrf meta tag")
	}
}

func TestCSRFMultipartFieldAndHeader(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Songs")
	page := b.get(fmt.Sprintf("/catalogs/%d", c.ID))
	assertPOSTFormsHaveCSRF(t, page.Body.String())

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField(csrfFormField, b.csrf); err != nil {
		t.Fatal(err)
	}
	part, err := w.CreateFormFile("file", "songs.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "title,artist\nHello,Adele\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b.skipCSRF = true
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/catalogs/%d/upload", c.ID), &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := b.do(req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("multipart field: %d\n%s", rec.Code, rec.Body.String())
	}
}

func TestCSRFOriginCheckStillRejectsCrossSite(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	b.get("/feedback")
	req := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(feedbackValues("cross", "").Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(csrfHeader, b.csrf)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := b.do(req); rec.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
		t.Fatalf("cross-site with token: %d rows=%d", rec.Code, feedbackCount(t, a))
	}

	same := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(feedbackValues("same", "").Encode()))
	same.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	same.Header.Set("Sec-Fetch-Site", "same-origin")
	if rec := b.do(same); rec.Code != http.StatusOK || feedbackCount(t, a) != 1 {
		t.Fatalf("same-origin: %d rows=%d\n%s", rec.Code, feedbackCount(t, a), rec.Body.String())
	}

	sibling := httptest.NewRequest(http.MethodPost, "/feedback", strings.NewReader(feedbackValues("sibling", "").Encode()))
	sibling.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sibling.Header.Set(csrfHeader, b.csrf)
	sibling.Header.Set("Sec-Fetch-Site", "same-site")
	if rec := b.do(sibling); rec.Code != http.StatusForbidden || feedbackCount(t, a) != 1 {
		t.Fatalf("same-site: %d rows=%d", rec.Code, feedbackCount(t, a))
	}
}

func TestCSRFRejectionDoesNotLogTheToken(t *testing.T) {
	a := newTestApp(t)
	var buf bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&buf, nil))
	b := newBrowser(t, a)
	b.get("/feedback")
	secret := "this-token-must-not-appear-in-logs"
	b.skipCSRF = true
	postFeedback(b, feedbackValues("x", ""), secret)
	if strings.Contains(buf.String(), secret) || strings.Contains(buf.String(), b.csrf) {
		t.Fatalf("log leaked a token: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "csrf rejected") {
		t.Fatalf("log: %s", buf.String())
	}
}

func TestCSRFCookieFlags(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	b.get("/signin")
	c := b.cookies[csrfCookieName]
	if c == nil || c.Name != "csrf" || c.Secure || c.Path != "/" || c.Domain != "" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("dev csrf cookie: %+v", c)
	}

	prod := newTestApp(t)
	prod.cfg.Env = "prod"
	pb := newBrowser(t, prod)
	pb.get("/signin")
	pc := pb.cookies["__Host-"+csrfCookieName]
	if pc == nil || !pc.Secure || pc.Path != "/" || pc.Domain != "" || !pc.HttpOnly || pc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("prod csrf cookie: %+v", pc)
	}
	if pb.cookies[csrfCookieName] != nil {
		t.Fatal("prod set the unprefixed csrf cookie")
	}
}

func TestLogoutRequiresCSRF(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	signIn(t, a, b, "a@example.com", "")
	b.skipCSRF = true
	if rec := b.post("/logout", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("logout without token: %d", rec.Code)
	}
	if rec := b.get("/account"); rec.Code != http.StatusOK {
		t.Fatalf("session ended without a token: %d", rec.Code)
	}
}

func TestAnonymousCSRFSessionIsOneRecord(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	b.get("/feedback")
	b.get("/feedback")
	b.get("/signin")

	var rows []CSRFSession
	if err := a.db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("refresh created %d csrf sessions", len(rows))
	}
	c := b.cookies[csrfCookieName]
	if c == nil || !c.HttpOnly || c.Secure || c.Path != "/" || c.Domain != "" || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("dev csrf cookie: %+v", c)
	}
	if c.Value == rows[0].Token || c.Value == b.csrf || hashToken(c.Value) != rows[0].ID || b.csrf != rows[0].Token {
		t.Fatal("cookie must be an opaque id, not the synchronizer token")
	}

	b.cookies[csrfCookieName] = &http.Cookie{Name: csrfCookieName, Value: "not-a-real-session"}
	b.skipCSRF = true
	if rec := postFeedback(b, feedbackValues("stolen", ""), rows[0].Token); rec.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
		t.Fatalf("invalid csrf cookie: %d rows=%d", rec.Code, feedbackCount(t, a))
	}

	if err := a.db.Model(&rows[0]).Update("expires_at", a.now().Add(-time.Hour)).Error; err != nil {
		t.Fatal(err)
	}
	a.cleanupOnce()
	var left int64
	if err := a.db.Model(&CSRFSession{}).Count(&left).Error; err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("cleanup left %d csrf sessions", left)
	}
}

func TestImpersonationReplacesCSRFToken(t *testing.T) {
	a := newTestApp(t)
	admin, _ := signInAdmin(t, a, "admin@example.com")
	fan := signIn(t, a, newBrowser(t, a), "fan@example.com", roleAudience)
	admin.get("/account")
	before := admin.csrf

	rec := admin.post(fmt.Sprintf("/admin/users/%d/impersonate", fan.ID), nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("impersonate: %d\n%s", rec.Code, rec.Body.String())
	}
	admin.skipCSRF = true
	if rec := postFeedback(admin, feedbackValues("stale admin", ""), before); rec.Code != http.StatusForbidden || feedbackCount(t, a) != 0 {
		t.Fatalf("pre-impersonation token: %d rows=%d", rec.Code, feedbackCount(t, a))
	}

	admin.skipCSRF = false
	admin.csrf = ""
	if rec := admin.post("/feedback", feedbackValues("as fan", "")); rec.Code != http.StatusOK {
		t.Fatalf("impersonated token: %d\n%s", rec.Code, rec.Body.String())
	}
	admin.get("/")
	asFan := admin.csrf
	if rec := admin.post("/admin/return", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("return: %d\n%s", rec.Code, rec.Body.String())
	}
	admin.skipCSRF = true
	if rec := postFeedback(admin, feedbackValues("stale fan", ""), asFan); rec.Code != http.StatusForbidden {
		t.Fatalf("impersonation token after return: %d", rec.Code)
	}
}

func TestCSRFMultipartLeadingPartIsCapped(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Songs")
	old := csrfBodyLimit
	csrfBodyLimit = 64
	t.Cleanup(func() { csrfBodyLimit = old })

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	note, err := w.CreateFormField("note")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(note, strings.Repeat("x", 4096)); err != nil {
		t.Fatal(err)
	}
	file, err := w.CreateFormFile("file", "songs.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "title,artist\nHello,Adele\n"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	b.skipCSRF = true
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/catalogs/%d/upload", c.ID), &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := b.do(req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("oversized leading part: %d\n%s", rec.Code, rec.Body.String())
	}
	var versions int64
	if err := a.db.Model(&CatalogVersion{}).Where("catalog_id = ?", c.ID).Count(&versions).Error; err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Fatalf("rejected upload stored %d versions", versions)
	}
}

func TestTokenFreeResponsesDoNotStartACSRFSession(t *testing.T) {
	a := newTestApp(t)
	h := a.routes()
	for _, path := range []string{"/healthz", "/robots.txt"} {
		rec := get(t, h, path)
		if rec.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s set a cookie: %q", path, rec.Header().Get("Set-Cookie"))
		}
	}
	var n int64
	if err := a.db.Model(&CSRFSession{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("token-free requests created %d csrf sessions", n)
	}
	home := get(t, h, "/")
	if !strings.Contains(home.Body.String(), `name="csrf-token"`) || home.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("home cache %q", home.Header().Get("Cache-Control"))
	}
}
