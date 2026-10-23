package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAccountPageForDJAndAudience(t *testing.T) {
	a := newTestApp(t)
	dj := newBrowser(t, a)
	u := signIn(t, a, dj, "dj@example.com", "dj")
	body := dj.get("/account").Body.String()
	for _, want := range []string{"Public profile", "http://example.test/dj/" + u.Handle(), "Show defaults", "Set password", `data-theme-choice="dark"`, `id="handle"`} {
		if !strings.Contains(body, want) {
			t.Errorf("DJ account page is missing %q", want)
		}
	}
	home := dj.get("/").Body.String()
	publicURL := "http://example.test/dj/" + u.Handle()
	for _, want := range []string{
		`<a class="public-url" href="` + publicURL + `">` + publicURL + `</a>`,
		`<a href="/account#handle">Change</a>`,
	} {
		if !strings.Contains(home, want) {
			t.Errorf("dashboard is missing %q", want)
		}
	}

	fan := newBrowser(t, a)
	signIn(t, a, fan, "fan@example.com", "")
	body = fan.get("/account").Body.String()
	if strings.Contains(body, "Public profile") || !strings.Contains(body, "Start hosting") {
		t.Fatalf("audience account page:\n%s", body)
	}
	for _, path := range []string{"/account/profile", "/account/defaults"} {
		if rec := fan.post(path, url.Values{"handle": {"fan"}, "show_type": {"dj"}}); rec.Code != http.StatusNotFound {
			t.Errorf("audience %s: %d", path, rec.Code)
		}
	}
}

func TestStartHosting(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	u := signIn(t, a, b, "fan@example.com", "")
	if rec := b.post("/account/host", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("host: %d", rec.Code)
	}
	a.db.Take(&u, u.ID)
	if u.Role != roleDJ || !strings.HasPrefix(u.Handle(), "dj-") {
		t.Fatalf("user = %+v", u)
	}
	if body := b.get("/account?saved=hosting").Body.String(); !strings.Contains(body, "You can host shows now") {
		t.Fatal("no hosting message")
	}
}

func TestAccountProfile(t *testing.T) {
	a := newTestApp(t)
	other := newBrowser(t, a)
	signIn(t, a, other, "other@example.com", "dj")
	other.post("/account/profile", url.Values{"handle": {"taken"}})

	b := newBrowser(t, a)
	u := signIn(t, a, b, "dj@example.com", "dj")

	for handle, want := range map[string]string{
		"ab":                    "3 to 30 characters",
		strings.Repeat("a", 31): "3 to 30 characters",
		"-mike":                 "start or end with a hyphen",
		"mike-":                 "start or end with a hyphen",
		"mi ke":                 "lowercase letters",
		"mïke":                  "lowercase letters",
		"taken":                 "That handle is taken.",
	} {
		rec := b.post("/account/profile", url.Values{"handle": {handle}})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("handle %q: %d, want %q", handle, rec.Code, want)
		}
	}
	rec := b.post("/account/profile", url.Values{"handle": {"dj-x"}, "display_name": {strings.Repeat("x", 51)}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "at most 50") {
		t.Errorf("long display name: %d", rec.Code)
	}

	rec = b.post("/account/profile", url.Values{"handle": {"  DJ-Mike "}, "display_name": {" DJ Mike "}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d\n%s", rec.Code, rec.Body.String())
	}
	a.db.Take(&u, u.ID)
	if u.Handle() != "dj-mike" || u.DisplayName != "DJ Mike" || u.PublicName() != "DJ Mike" {
		t.Fatalf("user = %+v", u)
	}
	// Saving your own handle again is not "taken".
	if rec := b.post("/account/profile", url.Values{"handle": {"dj-mike"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("resave: %d", rec.Code)
	}
	a.db.Take(&u, u.ID)
	if u.PublicName() != "dj-mike" {
		t.Fatalf("empty display name should fall back to the handle, got %q", u.PublicName())
	}
}

func TestAccountDefaults(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	u := signIn(t, a, b, "dj@example.com", "dj")
	if u.DefaultShowType != showTypeKaraoke || u.DefaultRotationStyle != rotationStandard || u.DefaultMaxSongsPerSinger != nil {
		t.Fatalf("new user defaults = %+v", u)
	}

	for _, form := range []url.Values{
		{"show_type": {"rave"}},
		{"show_type": {"dj"}, "max_songs": {"0"}},
		{"show_type": {"dj"}, "max_songs": {"100"}},
		{"show_type": {"dj"}, "max_songs": {"two"}},
	} {
		if rec := b.post("/account/defaults", form); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d", form, rec.Code)
		}
	}

	if rec := b.post("/account/defaults", url.Values{"show_type": {"dj"}, "max_songs": {"3"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d", rec.Code)
	}
	a.db.Take(&u, u.ID)
	if u.DefaultShowType != showTypeDJ || u.DefaultMaxSongsPerSinger == nil || *u.DefaultMaxSongsPerSinger != 3 {
		t.Fatalf("user = %+v", u)
	}
	b.post("/account/defaults", url.Values{"show_type": {"karaoke"}, "max_songs": {""}})
	a.db.Take(&u, u.ID)
	if u.DefaultMaxSongsPerSinger != nil {
		t.Fatal("blank should clear the limit")
	}
}

func TestPasswordChangeSignsOutOtherSessions(t *testing.T) {
	a := newTestApp(t)
	laptop := newBrowser(t, a)
	phone := newBrowser(t, a)
	signIn(t, a, laptop, "a@example.com", "")
	signIn(t, a, phone, "a@example.com", "")

	for _, form := range []url.Values{
		{"password": {"short"}, "confirm": {"short"}},
		{"password": {"long enough"}, "confirm": {"different!"}},
		{"password": {strings.Repeat("x", 73)}, "confirm": {strings.Repeat("x", 73)}},
	} {
		if rec := laptop.post("/account/password", form); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("bad password accepted: %d", rec.Code)
		}
	}

	laptop.post("/account/password", url.Values{"password": {"long enough"}, "confirm": {"long enough"}})
	if rec := laptop.get("/account"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Change password") {
		t.Fatalf("laptop: %d", rec.Code)
	}
	if rec := phone.get("/account"); rec.Code != http.StatusSeeOther {
		t.Fatalf("phone still signed in: %d", rec.Code)
	}

	laptop.post("/account/password/delete", nil)
	var u User
	a.db.Take(&u, "email = ?", "a@example.com")
	if u.HasPassword() {
		t.Fatal("password not removed")
	}
}

func TestDeleteAccount(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	u := signIn(t, a, b, "dj@example.com", "dj")

	if rec := b.post("/account/delete", url.Values{"confirm": {"someone@example.com"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("wrong confirmation: %d", rec.Code)
	}
	rec := b.post("/account/delete", url.Values{"confirm": {"DJ@example.com"}})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Account deleted") || strings.Contains(rec.Body.String(), `aria-label="Log out"`) {
		t.Fatalf("delete: %d\n%s", rec.Code, rec.Body.String())
	}
	var n int64
	a.db.Model(&User{}).Where("id = ?", u.ID).Count(&n)
	if n != 0 {
		t.Fatal("user still exists")
	}
	a.db.Model(&Session{}).Count(&n)
	if n != 0 {
		t.Fatal("sessions left")
	}
	if rec := b.get("/account"); rec.Code != http.StatusSeeOther {
		t.Fatalf("still signed in: %d", rec.Code)
	}
}

func TestOnlyAdminCannotDeleteAccount(t *testing.T) {
	a := newTestApp(t)
	b := newBrowser(t, a)
	u := signIn(t, a, b, "admin@example.com", "")
	a.db.Model(&u).Update("role", roleAdmin)
	rec := b.post("/account/delete", url.Values{"confirm": {"admin@example.com"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "only admin") {
		t.Fatalf("got %d", rec.Code)
	}
}
