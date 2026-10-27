package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestConsoleMailerOmitsBody(t *testing.T) {
	var buf bytes.Buffer
	m := consoleMailer{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	body := "Visit http://localhost:8080/auth/super-secret-token"
	if err := m.Send("a@example.com", "Your DJMagic.io login link", body); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, "super-secret-token") || strings.Contains(got, "/auth/") || strings.Contains(got, body) {
		t.Fatalf("log leaked the message: %s", got)
	}
	if !strings.Contains(got, "a@example.com") || !strings.Contains(got, "Your DJMagic.io login link") {
		t.Fatalf("log dropped the recipient or subject: %s", got)
	}
}

func TestMagicLinkShownOnlyWhenAsked(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)

	rec := b.post("/signin/link", url.Values{"email": {"plain@example.com"}})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "/auth/") {
		t.Fatalf("link on the page without the flag: %d\n%s", rec.Code, rec.Body.String())
	}

	a.cfg.DevShowMagicLink = true
	rec = b.post("/signin/link", url.Values{"email": {"shown@example.com"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/auth/") {
		t.Fatalf("expected the link on the page: %d\n%s", rec.Code, rec.Body.String())
	}

	a.cfg.SMTPHost = "localhost"
	rec = b.post("/signin/link", url.Values{"email": {"mailed@example.com"}})
	if strings.Contains(rec.Body.String(), "/auth/") {
		t.Fatal("Mailhog path put the link on the page")
	}
	a.cfg.SMTPHost = ""

	a.cfg.Env = "prod"
	rec = b.post("/signin/link", url.Values{"email": {"prod@example.com"}})
	if strings.Contains(rec.Body.String(), "/auth/") {
		t.Fatal("production page included the link")
	}
}

func TestLoadConfigDevShowMagicLink(t *testing.T) {
	t.Setenv("ENV", "dev")
	t.Setenv("DEV_SHOW_MAGIC_LINK", "true")
	if !loadConfig().DevShowMagicLink {
		t.Fatal("expected the flag")
	}
	t.Setenv("DEV_SHOW_MAGIC_LINK", "1")
	if loadConfig().DevShowMagicLink {
		t.Fatal("only the exact string true enables the flag")
	}
	t.Setenv("DEV_SHOW_MAGIC_LINK", "")
	if loadConfig().DevShowMagicLink {
		t.Fatal("unset should be off")
	}
}

func TestPasswordRemovalSignsOutOtherSessions(t *testing.T) {
	a := newTestApp(t)
	laptop := newBrowser(t, a)
	phone := newBrowser(t, a)
	signIn(t, a, laptop, "a@example.com", "")
	if rec := laptop.post("/account/password", url.Values{"password": {"long enough"}, "confirm": {"long enough"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("set password: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := phone.post("/signin", url.Values{"email": {"a@example.com"}, "password": {"long enough"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("phone login: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := laptop.post("/account/password/delete", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d\n%s", rec.Code, rec.Body.String())
	}
	if rec := laptop.get("/account"); rec.Code != http.StatusOK {
		t.Fatalf("current session ended: %d", rec.Code)
	}
	if rec := phone.get("/account"); rec.Code != http.StatusSeeOther {
		t.Fatalf("other session still signed in: %d", rec.Code)
	}
}

func TestSensitivePagesAreNotStored(t *testing.T) {
	a := newTestApp(t)
	anon := newBrowser(t, a)
	if cc := anon.get("/").Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("public home: %q", cc)
	}

	signedIn := newBrowser(t, a)
	signIn(t, a, signedIn, "dj@example.com", "dj")
	if cc := signedIn.get("/").Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("signed-in home: %q", cc)
	}
	if cc := signedIn.get("/account").Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("account: %q", cc)
	}

	if rec := anon.post("/signin/link", url.Values{"email": {"link@example.com"}}); rec.Code != http.StatusOK {
		t.Fatalf("link request: %d", rec.Code)
	}
	if cc := anon.get(lastLink(t, a, "link@example.com")).Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("magic-link page: %q", cc)
	}

	_, _, path := audienceShow(t, a, url.Values{"name": {"Friday"}, "approval_required": {"0"}})
	g := guest(t, a, "192.0.2.40")
	if rec := request(g, path, "Alice", "Hello"); rec.Code != http.StatusSeeOther {
		t.Fatalf("request: %d\n%s", rec.Code, rec.Body.String())
	}
	queue := g.get(path + "/queue")
	if cc := queue.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("queue: %q", cc)
	}
	if cc := anon.get("/docs/csv-format").Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("public docs: %q", cc)
	}
	if cc := anon.get("/feedback").Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Fatalf("public feedback: %q", cc)
	}
}

func TestDevCookiesKeepPlainNames(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	signIn(t, a, b, "dj@example.com", "dj")
	c := b.cookies[sessionCookie]
	if c == nil || c.Name != "session" || c.Secure || c.Path != "/" || c.Domain != "" || !c.HttpOnly {
		t.Fatalf("dev session cookie: %+v", c)
	}
	_, _, path := audienceShow(t, a, url.Values{"name": {"Friday"}, "approval_required": {"0"}})
	g := guest(t, a, "192.0.2.41")
	request(g, path, "Alice", "Hello")
	gc := g.cookies[guestCookie]
	if gc == nil || gc.Name != "guest" || gc.Secure || gc.Path != "/" || gc.Domain != "" {
		t.Fatalf("dev guest cookie: %+v", gc)
	}
}

func TestProductionCookiesUseHostPrefix(t *testing.T) {
	a := newTestApp(t)
	a.cfg.Env = "prod"

	b := newBrowser(t, a)
	signIn(t, a, b, "dj@example.com", "dj")
	c := b.cookies["__Host-session"]
	if c == nil || c.Name != "__Host-session" || !c.Secure || c.Path != "/" || c.Domain != "" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie: %+v", c)
	}
	if b.cookies[sessionCookie] != nil {
		t.Fatal("plain session cookie was also set")
	}
	if rec := b.get("/account"); rec.Code != http.StatusOK {
		t.Fatalf("session did not persist: %d", rec.Code)
	}

	legacy := newBrowser(t, a)
	legacy.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: c.Value}
	if rec := legacy.get("/account"); rec.Code != http.StatusSeeOther {
		t.Fatalf("legacy session cookie was accepted: %d", rec.Code)
	}

	if rec := b.post("/logout", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("logout: %d", rec.Code)
	}
	if b.cookies["__Host-session"] != nil {
		t.Fatal("session cookie survived logout")
	}
	if rec := b.get("/account"); rec.Code != http.StatusSeeOther {
		t.Fatalf("still signed in after logout: %d", rec.Code)
	}

	_, _, path := audienceShow(t, a, url.Values{"name": {"Friday"}, "approval_required": {"0"}})
	alice := guest(t, a, "192.0.2.42")
	if rec := request(alice, path, "Alice", "Hello"); rec.Code != http.StatusSeeOther {
		t.Fatalf("request: %d\n%s", rec.Code, rec.Body.String())
	}
	gc := alice.cookies["__Host-guest"]
	if gc == nil || !gc.Secure || gc.Path != "/" || gc.Domain != "" || !gc.HttpOnly {
		t.Fatalf("guest cookie: %+v", gc)
	}
	if alice.cookies[guestCookie] != nil {
		t.Fatal("plain guest cookie was also set")
	}
	first := alice.get(path + "/queue")
	if !strings.Contains(first.Body.String(), `class="queue-you"`) {
		t.Fatal("queue did not recognize the guest")
	}
	again := alice.get(path + "/queue")
	if again.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(again.Body.String(), `class="queue-you"`) {
		t.Fatalf("refreshed queue: cache=%q", again.Header().Get("Cache-Control"))
	}

	old := guest(t, a, "192.0.2.43")
	old.cookies[guestCookie] = &http.Cookie{Name: guestCookie, Value: gc.Value}
	if strings.Contains(old.get(path+"/queue").Body.String(), `class="queue-you"`) {
		t.Fatal("legacy guest cookie was accepted")
	}
}
