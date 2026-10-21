package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func hxPost(b *browser, target string, form url.Values) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	return b.do(req)
}

func profileOf(t *testing.T, a *app, u User) AudienceProfile {
	t.Helper()
	var p AudienceProfile
	if err := a.db.Where("user_id = ?", u.ID).First(&p).Error; err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGuestHistoryLinksOnSignIn(t *testing.T) {
	a := newTestApp(t)
	host, s, path := audienceShow(t, a, url.Values{"approval_required": {"0"}})

	g := guest(t, a, "192.0.2.10")
	request(g, path, "Alice", "Hello")
	act(host, requestByTitle(t, a, s, "Hello"), "start", nil, false)
	act(host, requestByTitle(t, a, s, "Hello"), "done", nil, false)
	guestHash := hashToken(g.cookies[guestCookie].Value)

	u := signIn(t, a, g, "alice@example.com", roleAudience)
	if g.cookies[guestCookie] != nil {
		t.Fatal("signing in should clear the guest cookie")
	}
	p := profileOf(t, a, u)
	if p.GuestTokenHash != nil {
		t.Fatal("the linked profile should drop its guest token")
	}
	var n int64
	a.db.Model(&AudienceProfile{}).Where("guest_token_hash = ?", guestHash).Count(&n)
	if n != 0 {
		t.Fatal("the guest profile should now be the account's")
	}
	mustContain(t, "performances", g.get("/me/performances").Body.String(), "Hello", "Every song you've sung.")

	// Later, as a guest again in the same show: signing in merges the two singers.
	g.post("/logout", nil)
	request(g, path, "Alice G", "Second")
	signIn(t, a, g, "alice@example.com", "")
	var singers []ShowParticipant
	a.db.Where("show_id = ?", s.ID).Find(&singers)
	if len(singers) != 1 || singers[0].AudienceProfileID == nil || *singers[0].AudienceProfileID != p.ID {
		t.Fatalf("singers after merge: %+v", singers)
	}
	if sr := requestByTitle(t, a, s, "Second"); sr.ShowParticipantID != singers[0].ID {
		t.Fatalf("the guest's request should move to the account's singer: %+v", sr)
	}
	a.db.Model(&AudienceProfile{}).Count(&n)
	if n != 1 {
		t.Fatalf("profiles after merge: %d", n)
	}
}

// catalogShowWithFan starts a catalog-only show and signs a fan in.
func catalogShowWithFan(t *testing.T) (a *app, host *browser, s Show, path string, fan *browser, u User, pick, variant string) {
	a = newTestApp(t)
	host, dj := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, host, "Songs")
	uploadCSV(t, host, c, fixture(t, "valid_kj_versions.csv"))
	build(t, a)
	host.post(fmt.Sprintf("/catalogs/%d", c.ID), url.Values{"variant_display": {"version"}})
	s = createShow(t, a, host, url.Values{"catalog_id": {fmt.Sprint(c.ID)}, "approval_required": {"0"}})
	a.db.First(&dj, dj.ID)
	path = showPath(dj.Handle(), s.Slug)

	db, err := a.cache.open(t.Context(), c.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	songs, _ := searchCatalog(db, "bohemian", 10, 0, "")
	pick = fmt.Sprintf("1:%d", songs.Songs[0].ID)
	variant = fmt.Sprintf("variant-%d", songs.Songs[0].ID)

	fan = guest(t, a, "192.0.2.20")
	u = signIn(t, a, fan, "fan@example.com", roleAudience)
	return
}

func TestSavedSongsAndSingAgain(t *testing.T) {
	a, host, s, path, fan, u, pick, variant := catalogShowWithFan(t)
	show := fmt.Sprint(s.ID)

	rec := fan.get(path + "/search?q=bohemian&v=1")
	if rec.Header().Get("Cache-Control") != "" {
		t.Error("search results with hearts must not be cached")
	}
	mustContain(t, "search", rec.Body.String(), `action="/me/saved"`, `aria-label="Save Bohemian Rhapsody"`, `hx-include="closest tr"`)
	mustContain(t, "show page", fan.get(path).Body.String(), "/me/saved?show="+show, "/me/performances?show="+show)

	save := url.Values{"show": {show}, "song": {pick}, variant: {"Key +2"}, "next": {path}}
	rec = hxPost(fan, "/me/saved", save)
	mustContain(t, "heart", rec.Body.String(), `aria-pressed="true"`, "Remove Bohemian Rhapsody from saved songs")
	hxPost(fan, "/me/saved", save)
	var saved []SavedSong
	a.db.Find(&saved)
	if len(saved) != 1 || saved[0].SongVersion != "Key +2" || saved[0].SongArtist != "Queen" || saved[0].SourceCatalogID == nil {
		t.Fatalf("saved: %+v", saved)
	}
	if rec := fan.post("/me/saved", save); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != path {
		t.Fatalf("plain save: %d %v", rec.Code, rec.Header())
	}
	mustContain(t, "search after save", fan.get(path+"/search?q=bohemian&v=1").Body.String(), `aria-pressed="true"`)

	body := fan.get("/me/saved?show=" + show).Body.String()
	mustContain(t, "saved page", body, "Bohemian Rhapsody", "Key &#43;2", "Requesting at", fmt.Sprintf("/me/saved/%d/request", saved[0].ID))
	if body := fan.get("/me/saved").Body.String(); strings.Contains(body, "/request") {
		t.Error("without a show there's nowhere to request")
	}

	rec = fan.post(fmt.Sprintf("/me/saved/%d/request", saved[0].ID), url.Values{"show": {show}, "participant_name": {"Fan"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != path+"/queue?requested=queued" {
		t.Fatalf("request saved: %d %v\n%s", rec.Code, rec.Header(), rec.Body)
	}
	sr := requestByTitle(t, a, s, "Bohemian Rhapsody")
	if sr.RequestSource != sourceSaved || sr.SongVersion != "Key +2" {
		t.Fatalf("request: %+v", sr)
	}
	a.db.First(&saved[0], saved[0].ID)
	if saved[0].RequestCount != 1 || saved[0].LastRequestedAt == nil {
		t.Fatalf("request count: %+v", saved[0])
	}

	act(host, sr, "start", nil, false)
	act(host, sr, "done", nil, false)
	body = fan.get("/me/performances?show=" + show).Body.String()
	mustContain(t, "performances", body, "Bohemian Rhapsody", `aria-pressed="true"`, fmt.Sprintf("/me/performances/%d/request", sr.ID))
	rec = fan.post(fmt.Sprintf("/me/performances/%d/request", sr.ID), url.Values{"show": {show}, "participant_name": {"Fan"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sing again: %d\n%s", rec.Code, rec.Body)
	}
	var again int64
	a.db.Model(&SongRequest{}).Where("show_id = ? AND status = ? AND request_source = ?", s.ID, requestQueued, sourceSaved).Count(&again)
	if again != 1 {
		t.Fatal("sing again should queue the song")
	}
	if rec := fan.post(fmt.Sprintf("/me/performances/%d/request", sr.ID+1), url.Values{"show": {show}}); rec.Code != http.StatusNotFound {
		t.Fatalf("an unfinished request can't be sung again: %d", rec.Code)
	}

	// A catalog-only show only takes songs in its catalog.
	p := profileOf(t, a, u)
	other := SavedSong{AudienceProfileID: p.ID, SongTitle: "Not There", SongArtist: "Nobody", Fingerprint: fingerprint("Not There", "Nobody", "", "")}
	a.db.Create(&other)
	rec = fan.post(fmt.Sprintf("/me/saved/%d/request", other.ID), url.Values{"show": {show}, "participant_name": {"Fan"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("not in catalog: %d", rec.Code)
	}
	mustContain(t, "not in catalog", rec.Body.String(), "That song isn&#39;t in this show&#39;s catalog.", "<details class=\"memory-request\" open>", `value="Fan"`)

	// Someone else can't use or remove my saved songs.
	stranger := guest(t, a, "192.0.2.21")
	signIn(t, a, stranger, "stranger@example.com", roleAudience)
	if rec := stranger.post(fmt.Sprintf("/me/saved/%d/request", saved[0].ID), url.Values{"show": {show}, "participant_name": {"X"}}); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger request: %d", rec.Code)
	}
	if rec := stranger.post(fmt.Sprintf("/me/saved/%d/delete", saved[0].ID), nil); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger delete: %d", rec.Code)
	}
	if rec := stranger.post("/me/saved", url.Values{"request": {fmt.Sprint(sr.ID)}}); rec.Code != http.StatusNotFound {
		t.Fatalf("stranger saving my request: %d", rec.Code)
	}

	// Unsave from a heart, then from the list.
	rec = hxPost(fan, fmt.Sprintf("/me/saved/%d/delete", saved[0].ID), url.Values{"heart": {"1"}, "request": {fmt.Sprint(sr.ID)}, "next": {"/me/performances"}})
	mustContain(t, "unsaved heart", rec.Body.String(), `aria-pressed="false"`, `name="request" value="`+fmt.Sprint(sr.ID)+`"`)
	rec = fan.post("/me/saved", url.Values{"request": {fmt.Sprint(sr.ID)}, "next": {"/me/performances"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save from performance: %d", rec.Code)
	}
	var fromPerf SavedSong
	a.db.Where("audience_profile_id = ? AND song_title = ?", p.ID, "Bohemian Rhapsody").First(&fromPerf)
	rec = fan.post(fmt.Sprintf("/me/saved/%d/delete", fromPerf.ID), url.Values{"show": {show}})
	if rec.Header().Get("Location") != "/me/saved?removed=1&show="+show {
		t.Fatalf("list delete: %v", rec.Header())
	}
	mustContain(t, "after delete", fan.get("/me/saved?removed=1").Body.String(), "Removed from saved songs.")

	// Once the show ends, requests are refused.
	host.post(fmt.Sprintf("/shows/%d/end", s.ID), nil)
	rec = fan.post(fmt.Sprintf("/me/saved/%d/request", other.ID), url.Values{"show": {show}, "participant_name": {"Fan"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "That show is not accepting requests right now.") {
		t.Fatalf("ended show: %d", rec.Code)
	}
}

func TestMemoryPagesSignedOut(t *testing.T) {
	a := newTestApp(t)
	g := guest(t, a, "192.0.2.10")
	mustContain(t, "saved", g.get("/me/saved").Body.String(), "Save your song history", "/signup?next=%2Fme%2Fsaved")
	mustContain(t, "performances", g.get("/me/performances").Body.String(), "Save your song history")
	if rec := g.post("/me/saved", url.Values{"request": {"1"}}); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin") {
		t.Fatalf("saving signed out: %d %v", rec.Code, rec.Header())
	}
}

func TestDJsHaveSongMemoryToo(t *testing.T) {
	a := newTestApp(t)
	_, s, path := audienceShow(t, a, url.Values{"approval_required": {"0"}})
	other, u := signInHost(t, a, "other@example.com")
	request(other, path, "Other DJ", "Hello")
	sr := requestByTitle(t, a, s, "Hello")
	if rec := other.post("/me/saved", url.Values{"request": {fmt.Sprint(sr.ID)}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d", rec.Code)
	}
	var n int64
	a.db.Model(&SavedSong{}).Where("audience_profile_id = ?", profileOf(t, a, u).ID).Count(&n)
	if n != 1 {
		t.Fatal("a DJ's saved song should be on their profile")
	}
	mustContain(t, "dashboard", other.get("/").Body.String(), `href="/me/saved"`)
}

func TestCleanupStaleGuestsAndDeletedAccounts(t *testing.T) {
	a := newTestApp(t)
	_, s, path := audienceShow(t, a, url.Values{"approval_required": {"0"}})
	request(guest(t, a, "192.0.2.10"), path, "Old Guest", "Hello")
	request(guest(t, a, "192.0.2.11"), path, "New Guest", "World")
	fan := guest(t, a, "192.0.2.12")
	u := signIn(t, a, fan, "fan@example.com", roleAudience)
	request(fan, path, "Fan", "Song")
	fanProfile := profileOf(t, a, u)
	a.db.Create(&SavedSong{AudienceProfileID: fanProfile.ID, SongTitle: "Kept", Fingerprint: "norm:kept||"})

	old := time.Now().Add(-staleGuestLifetime - time.Hour)
	oldSinger := participantByName(t, a, s, "Old Guest")
	a.db.Model(&AudienceProfile{}).Where("id = ?", *oldSinger.AudienceProfileID).Update("last_seen_at", old)
	a.db.Model(&AudienceProfile{}).Where("id = ?", fanProfile.ID).Update("last_seen_at", old)
	a.cleanupOnce()

	if p := participantByName(t, a, s, "Old Guest"); p.AudienceProfileID != nil {
		t.Fatal("the stale guest's profile should be gone, leaving the singer's name")
	}
	if p := participantByName(t, a, s, "New Guest"); p.AudienceProfileID == nil {
		t.Fatal("a recent guest should be kept")
	}
	profileOf(t, a, u) // an account's profile is never stale

	if err := a.deleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	var n int64
	a.db.Model(&SavedSong{}).Count(&n)
	if n != 0 {
		t.Fatal("deleting an account should delete its saved songs")
	}
	if p := participantByName(t, a, s, "Fan"); p.AudienceProfileID != nil {
		t.Fatal("deleting an account should delete its profile")
	}
}

func participantByName(t *testing.T, a *app, s Show, name string) ShowParticipant {
	t.Helper()
	var p ShowParticipant
	if err := a.db.Where("show_id = ? AND display_name = ?", s.ID, name).First(&p).Error; err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFingerprint(t *testing.T) {
	if got := fingerprint("  Hello   World ", "ADELE", "", ""); got != "norm:hello world|adele|" {
		t.Errorf("normalized: %q", got)
	}
	if got := fingerprint("Hello", "Adele", "Key +2", " SC123 "); got != "ext:SC123" {
		t.Errorf("external: %q", got)
	}
}
