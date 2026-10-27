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

// audienceShow starts a show for a new DJ and returns the DJ's browser and the show's public path.
func audienceShow(t *testing.T, a *app, form url.Values) (*browser, Show, string) {
	t.Helper()
	b, u := signInHost(t, a, "dj@example.com")
	s := createShow(t, a, b, form)
	var fresh User
	a.db.First(&fresh, u.ID)
	return b, s, showPath(fresh.Handle(), s.Slug)
}

func guest(t *testing.T, a *app, ip string) *browser {
	b := newBrowser(t, a)
	b.ip = ip
	return b
}

func request(b *browser, path, name, title string) *httptest.ResponseRecorder {
	b.t.Helper()
	return b.post(path+"/requests", url.Values{"participant_name": {name}, "song_title": {title}, "song_artist": {"Someone"}})
}

func hxGet(b *browser, target, hxTarget string) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", hxTarget)
	return b.do(req)
}

func mustContain(t *testing.T, what, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("%s lacks %q", what, want)
		}
	}
}

func TestAudienceRequestFlow(t *testing.T) {
	a := newTestApp(t)
	host, s, path := audienceShow(t, a, url.Values{"name": {"Friday"}, "approval_required": {"0"}})
	dj := strings.TrimSuffix(path, "/shows/"+s.Slug)

	alice := guest(t, a, "192.0.2.10")
	mustContain(t, "DJ profile", alice.get(dj).Body.String(), "Live Now", "Friday", "Make a Request", `href="`+path+`"`)
	mustContain(t, "show page", alice.get(path).Body.String(), "Request a Song", `name="song_title"`, "e.g. Mike S.")
	if alice.cookies[guestCookie] != nil {
		t.Fatal("looking at a show should not create a guest identity")
	}
	if alice.cookies[csrfCookieName] == nil {
		t.Fatal("looking at a show should set the anonymous csrf cookie")
	}
	var requests int64
	if err := a.db.Model(&SongRequest{}).Count(&requests).Error; err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("looking at a show created %d song requests", requests)
	}
	var guests int64
	if err := a.db.Model(&AudienceProfile{}).Where("user_id IS NULL").Count(&guests).Error; err != nil {
		t.Fatal(err)
	}
	if guests != 0 {
		t.Fatalf("looking at a show created %d guest profiles", guests)
	}

	rec := alice.post(path+"/requests", url.Values{"song_title": {"Hello"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Please enter your name.") {
		t.Fatalf("no name: %d", rec.Code)
	}
	mustContain(t, "failed request", rec.Body.String(), `value="Hello"`)
	if rec := alice.post(path+"/requests", url.Values{"participant_name": {"Alice"}}); !strings.Contains(rec.Body.String(), "Song title can&#39;t be blank") {
		t.Fatalf("no title: %d", rec.Code)
	}
	if alice.cookies[guestCookie] != nil {
		t.Fatal("a failed request should not create a guest identity")
	}

	rec = request(alice, path, "Alice", "Hello")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != path+"/queue?requested=queued" {
		t.Fatalf("request: %d %v\n%s", rec.Code, rec.Header(), rec.Body)
	}
	c := alice.cookies[guestCookie]
	if c == nil || !c.HttpOnly {
		t.Fatalf("guest cookie: %+v", c)
	}
	var profile AudienceProfile
	if err := a.db.Where("guest_token_hash = ?", hashToken(c.Value)).First(&profile).Error; err != nil {
		t.Fatal(err)
	}
	sr := requestByTitle(t, a, s, "Hello")
	if sr.Status != requestQueued || sr.RequestSource != sourceAudience || sr.QueuePosition == nil {
		t.Fatalf("request: %+v", sr)
	}

	body := alice.get(path + "/queue?requested=queued").Body.String()
	mustContain(t, "queue page", body, "You&#39;ve been added to the queue!", `class="queue-you"`, "You're up next!",
		"You are number 1 of 1.", `id="audience-live"`, `hx-get="`+path+`/queue"`, "/signup?role=audience")

	bob := guest(t, a, "192.0.2.11")
	request(bob, path, "Bob", "Wonderwall")
	body = bob.get(path + "/queue").Body.String()
	mustContain(t, "Bob's queue", body, "You're in the queue!", "You are number 2 of 2.", "Estimated wait: about 8 minutes.")
	if strings.Count(body, `class="queue-you"`) != 1 {
		t.Error("only Bob's row should be marked as his")
	}

	poll := hxGet(alice, path+"/queue", "audience-live")
	if strings.Contains(poll.Body.String(), "<html") || !strings.HasPrefix(strings.TrimSpace(poll.Body.String()), `<div id="audience-live"`) {
		t.Fatalf("poll should return the live block:\n%s", poll.Body)
	}

	// Her name is filled in, and the other pages show where she is in line.
	mustContain(t, "show page", alice.get(path).Body.String(), `value="Alice"`, "You're in the queue")
	mustContain(t, "DJ profile", alice.get(dj).Body.String(), "You're in the queue", path+"/queue")

	act(host, sr, "start", nil, false)
	body = alice.get(path + "/queue").Body.String()
	mustContain(t, "on stage", body, "(That's you!)", "You're up!", "now-playing-you")
	mustContain(t, "banner", alice.get(dj).Body.String(), "<strong>You're up!</strong>")

	display := bob.get(path + "/display").Body.String()
	mustContain(t, "display", display, `data-theme="dark"`, "Scan to request a song", "<svg", "Now Playing", "Hello", "Up Next", "Bob", "Wonderwall")
	if strings.Contains(display, "localStorage") {
		t.Error("the display is always dark and should not load the theme script")
	}
	if poll := hxGet(bob, path+"/display", "display-live"); strings.Contains(poll.Body.String(), "<html") || !strings.Contains(poll.Body.String(), `id="display-live"`) {
		t.Fatalf("display poll:\n%s", poll.Body)
	}

	// At the DJ's next show, Alice's last name is filled in.
	host.post(fmt.Sprintf("/shows/%d/end", s.ID), nil)
	next := createShow(t, a, host, url.Values{"name": {"Saturday"}})
	mustContain(t, "next show", alice.get(strings.Replace(path, s.Slug, next.Slug, 1)).Body.String(), `value="Alice"`)
	if body := alice.get(dj).Body.String(); strings.Contains(body, "queue-status-banner") {
		t.Error("an ended show should leave the banner")
	}
}

func TestAudienceRequestNotOpen(t *testing.T) {
	a := newTestApp(t)
	host, s, path := audienceShow(t, a, url.Values{"name": {"Later"}, "timing": {"later"}, "scheduled_at": {"2030-01-01T20:00:00Z"}})
	g := guest(t, a, "192.0.2.10")
	mustContain(t, "scheduled", g.get(path).Body.String(), "This show hasn't started yet.")
	if rec := request(g, path, "Alice", "Hello"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "This show hasn&#39;t started yet.") {
		t.Fatalf("scheduled: %d", rec.Code)
	}
	host.post(fmt.Sprintf("/shows/%d/start", s.ID), nil)
	host.post(fmt.Sprintf("/shows/%d/end", s.ID), nil)
	if rec := request(g, path, "Alice", "Hello"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "This show has ended and is no longer accepting requests.") {
		t.Fatalf("ended: %d", rec.Code)
	}
	if body := g.get(path + "/queue").Body.String(); strings.Contains(body, "hx-trigger") {
		t.Error("an ended show's queue should not poll")
	}
}

func TestAudiencePendingNoticeClearsWhenReviewed(t *testing.T) {
	a := newTestApp(t)
	host, s, path := audienceShow(t, a, url.Values{})
	g := guest(t, a, "192.0.2.10")
	request(g, path, "Alice", "Hello")

	body := g.get(path + "/queue?requested=pending").Body.String()
	mustContain(t, "pending queue", body, "The host will review it shortly.", `hx-get="`+path+`/queue?requested=pending"`)

	poll := hxGet(g, path+"/queue?requested=pending", "audience-live")
	mustContain(t, "still pending", poll.Body.String(), "The host will review it shortly.", `hx-get="`+path+`/queue?requested=pending"`)

	// A second request still waiting keeps the flash after the first is accepted.
	request(g, path, "Alice", "Encore")
	act(host, requestByTitle(t, a, s, "Hello"), "approve", nil, false)
	poll = hxGet(g, path+"/queue?requested=pending", "audience-live")
	mustContain(t, "one still pending", poll.Body.String(), "The host will review it shortly.", "You're up next!")

	act(host, requestByTitle(t, a, s, "Encore"), "approve", nil, false)
	poll = hxGet(g, path+"/queue?requested=pending", "audience-live")
	if strings.Contains(poll.Body.String(), "The host will review it shortly.") || strings.Contains(poll.Body.String(), "requested=pending") {
		t.Fatalf("accepted requests should drop the submitted notice:\n%s", poll.Body)
	}
	mustContain(t, "both accepted", poll.Body.String(), "You're up next!")

	body = g.get(path + "/queue?requested=pending").Body.String()
	if strings.Contains(body, "The host will review it shortly.") {
		t.Fatal("reloading the submitted URL should not bring the notice back")
	}

	// A rejection reviews the request too.
	request(g, path, "Alice", "Nope")
	act(host, requestByTitle(t, a, s, "Nope"), "reject", url.Values{"rejection_reason": {"Not tonight"}}, false)
	poll = hxGet(g, path+"/queue?requested=pending", "audience-live")
	if strings.Contains(poll.Body.String(), "The host will review it shortly.") {
		t.Fatal("a rejected request should drop the submitted notice")
	}
	mustContain(t, "rejected", poll.Body.String(), "Reason: Not tonight")
}

func TestAudienceNotFound(t *testing.T) {
	a := newTestApp(t)
	_, _, path := audienceShow(t, a, url.Values{})
	g := guest(t, a, "192.0.2.10")
	rec := g.get("/dj/nobody")
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "DJ Not Found") {
		t.Fatalf("unknown DJ: %d", rec.Code)
	}
	dj := path[:strings.Index(path, "/shows/")]
	for _, p := range []string{dj + "/shows/nope", dj + "/shows/nope/queue", dj + "/shows/nope/display"} {
		rec := g.get(p)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Show Not Found") || !strings.Contains(rec.Body.String(), `href="`+dj+`"`) {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}

func TestAudienceApprovalRejectAndCancel(t *testing.T) {
	a := newTestApp(t)
	host, s, path := audienceShow(t, a, url.Values{})
	g := guest(t, a, "192.0.2.10")

	if rec := request(g, path, "Alice", "Hello"); rec.Header().Get("Location") != path+"/queue?requested=pending" {
		t.Fatalf("pending request: %d %v", rec.Code, rec.Header())
	}
	body := g.get(path + "/queue?requested=pending").Body.String()
	mustContain(t, "queue page", body, "The host will review it shortly.", "Your Pending Requests", "Hello", "/cancel", `hx-confirm="Cancel your request for Hello?"`)
	mustContain(t, "host page", host.get(fmt.Sprintf("/shows/%d", s.ID)).Body.String(), "Pending Requests", "Alice")

	sr := requestByTitle(t, a, s, "Hello")
	cancel := fmt.Sprintf("%s/requests/%d/cancel", path, sr.ID)
	other := guest(t, a, "192.0.2.11")
	if rec := other.post(cancel, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("someone else's cancel: %d", rec.Code)
	}
	if rec := host.post(cancel, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("the DJ is a guest here, not the requester: %d", rec.Code)
	}
	if rec := g.post(cancel, nil); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != path+"/queue?canceled=1" {
		t.Fatalf("cancel: %d %v", rec.Code, rec.Header())
	}
	if sr := requestByTitle(t, a, s, "Hello"); sr.Status != requestCanceled {
		t.Fatalf("canceled: %+v", sr)
	}
	mustContain(t, "after cancel", g.get(path+"/queue?canceled=1").Body.String(), "Your request was canceled.")
	if rec := g.post(cancel, nil); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "This request can no longer be canceled.") {
		t.Fatalf("cancel twice: %d", rec.Code)
	}

	// A queued request can be canceled too, and the queue closes up.
	request(g, path, "Alice", "Queued One")
	request(other, path, "Bob", "Queued Two")
	act(host, requestByTitle(t, a, s, "Queued One"), "approve", nil, false)
	act(host, requestByTitle(t, a, s, "Queued Two"), "approve", nil, false)
	q := requestByTitle(t, a, s, "Queued One")
	g.post(fmt.Sprintf("%s/requests/%d/cancel", path, q.ID), nil)
	if got := queueTitles(t, a, s); len(got) != 1 || got[0] != "Queued Two" {
		t.Fatalf("queue after cancel: %v", got)
	}
	if two := requestByTitle(t, a, s, "Queued Two"); two.QueuePosition == nil || *two.QueuePosition != 1 {
		t.Fatalf("positions should close up: %+v", two)
	}

	request(g, path, "Alice", "Nope")
	act(host, requestByTitle(t, a, s, "Nope"), "reject", url.Values{"rejection_reason": {"Not tonight"}}, false)
	mustContain(t, "rejected", g.get(path+"/queue").Body.String(), "Rejected Requests", "Nope", "Reason: Not tonight")

	host.post(fmt.Sprintf("/shows/%d/end", s.ID), nil)
	if rec := g.post(fmt.Sprintf("%s/requests/%d/cancel", path, requestByTitle(t, a, s, "Nope").ID), nil); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "This show has ended.") {
		t.Fatalf("cancel after end: %d", rec.Code)
	}
}

func TestAudienceSongLimitAndNames(t *testing.T) {
	a := newTestApp(t)
	host, s, path := audienceShow(t, a, url.Values{"approval_required": {"0"}, "max_songs": {"1"}})
	g := guest(t, a, "192.0.2.10")
	request(g, path, "Alice", "One")
	rec := request(g, path, "Alice", "Two")
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "You can&#39;t request more songs until the ones you&#39;ve already requested are sung.") {
		t.Fatalf("limit: %d", rec.Code)
	}
	body := g.get(path).Body.String()
	mustContain(t, "show page at the limit", body, "You can't request more songs", path+"/queue")
	if strings.Contains(body, `name="song_title"`) {
		t.Error("the form should be hidden at the limit")
	}

	// A guest's request always uses that phone's own singer, so a guest "Carol"
	// is a different singer from a host-added Carol, as in Rails.
	addManual(t, host, s, "Carol", "Host Song")
	request(guest(t, a, "192.0.2.12"), path, "Carol", "Guest Song")
	var n int64
	a.db.Model(&ShowParticipant{}).Where("show_id = ? AND display_name = ?", s.ID, "Carol").Count(&n)
	if n != 2 {
		t.Fatalf("Carols = %d, want 2", n)
	}

	// Renaming herself renames her participant.
	act(host, requestByTitle(t, a, s, "One"), "start", nil, false)
	act(host, requestByTitle(t, a, s, "One"), "done", nil, false)
	request(g, path, "Alice B.", "Two")
	var p ShowParticipant
	a.db.Where("show_id = ? AND audience_profile_id IS NOT NULL", s.ID).First(&p)
	if p.DisplayName != "Alice B." {
		t.Fatalf("name: %q", p.DisplayName)
	}
}

func TestAudienceRateLimit(t *testing.T) {
	a := newTestApp(t)
	now := time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	_, _, path := audienceShow(t, a, url.Values{"approval_required": {"0"}})
	g := guest(t, a, "192.0.2.10")
	for i := range audienceRequestLimit {
		if rec := request(g, path, "Alice", fmt.Sprint("Song ", i)); rec.Code != http.StatusSeeOther {
			t.Fatalf("request %d: %d", i, rec.Code)
		}
	}
	rec := request(g, path, "Alice", "One more")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "You&#39;re sending requests too quickly.") {
		t.Fatalf("over the limit: %d", rec.Code)
	}
	if rec := request(guest(t, a, "192.0.2.11"), path, "Bob", "Other phone"); rec.Code != http.StatusSeeOther {
		t.Fatalf("another IP: %d", rec.Code)
	}
	now = now.Add(audienceRequestWindow)
	if rec := request(g, path, "Alice", "Later"); rec.Code != http.StatusSeeOther {
		t.Fatalf("after the window: %d", rec.Code)
	}
}

func TestAudienceAccountIdentity(t *testing.T) {
	a := newTestApp(t)
	_, s, path := audienceShow(t, a, url.Values{"approval_required": {"0"}})
	b := newBrowser(t, a)
	b.ip = "192.0.2.10"
	u := signIn(t, a, b, "fan@example.com", roleAudience)
	request(b, path, "Fan", "Hello")
	if b.cookies[guestCookie] != nil {
		t.Fatal("an audience account should not get a guest cookie")
	}
	var p AudienceProfile
	if err := a.db.Where("user_id = ?", u.ID).First(&p).Error; err != nil {
		t.Fatal(err)
	}
	var sp ShowParticipant
	a.db.Where("show_id = ?", s.ID).First(&sp)
	if sp.AudienceProfileID == nil || *sp.AudienceProfileID != p.ID {
		t.Fatalf("participant: %+v", sp)
	}
	body := b.get(path + "/queue").Body.String()
	mustContain(t, "queue page", body, `class="queue-you"`)
	if strings.Contains(body, "/signup?role=audience") {
		t.Error("a signed-in visitor should not be asked to sign up")
	}

	// A stale guest cookie is cleared.
	g := guest(t, a, "192.0.2.11")
	g.cookies[guestCookie] = &http.Cookie{Name: guestCookie, Value: "stale"}
	g.get(path)
	if g.cookies[guestCookie] != nil {
		t.Fatal("an unknown guest cookie should be cleared")
	}
}

func TestAudienceCatalogRequest(t *testing.T) {
	a := newTestApp(t)
	b, u := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Songs")
	uploadCSV(t, b, c, fixture(t, "valid_kj_versions.csv"))
	build(t, a)
	b.post(fmt.Sprintf("/catalogs/%d", c.ID), url.Values{"variant_display": {"version"}})
	s := createShow(t, a, b, url.Values{"catalog_id": {fmt.Sprint(c.ID)}, "approval_required": {"0"}})
	a.db.First(&u, u.ID)
	path := showPath(u.Handle(), s.Slug)
	g := guest(t, a, "192.0.2.10")

	body := g.get(path).Body.String()
	mustContain(t, "catalog show page", body, "Step 1: Find your song", `hx-get="`+path+`/search"`, `name="v" value="1"`, `id="pick-form"`)
	if strings.Contains(body, "Can't find your song?") {
		t.Error("freeform is off")
	}

	rec := g.get(path + "/search?q=bohemian&v=1")
	if rec.Header().Get("Cache-Control") != "private, max-age=300" {
		t.Errorf("search cache header: %q", rec.Header().Get("Cache-Control"))
	}
	mustContain(t, "search", rec.Body.String(), "Bohemian Rhapsody", `form="pick-form"`, "<option>Key &#43;2</option>", "Select")
	if rec := g.get(path + "/search?q=bohemian&v=0"); rec.Header().Get("Cache-Control") != "" {
		t.Error("a stale version must not be cached")
	}
	mustContain(t, "page search", g.get(path+"?q=journey").Body.String(), "Don&#39;t Stop Believin&#39;")

	db, err := a.cache.open(t.Context(), c.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	songs, _ := searchCatalog(db, "bohemian", 10, 0, "")
	song := songs.Songs[0]
	pick := fmt.Sprintf("1:%d", song.ID)
	variant := fmt.Sprintf("variant-%d", song.ID)
	body = g.get(path + "?song=" + url.QueryEscape(pick) + "&" + variant + "=" + url.QueryEscape("Key +2")).Body.String()
	mustContain(t, "step 2", body, "Step 2: Enter your name", "Bohemian Rhapsody", `name="song" value="`+pick+`"`, `value="Key &#43;2"`, "Change song")

	if rec := g.post(path+"/requests", url.Values{"participant_name": {"Alice"}, "song_title": {"Typed"}}); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "Pick a song from the catalog.") {
		t.Fatalf("freeform off: %d", rec.Code)
	}
	if rec := g.post(path+"/requests", url.Values{"participant_name": {"Alice"}, "song": {"1:99999"}}); !strings.Contains(rec.Body.String(), "That song is no longer in the catalog.") {
		t.Fatalf("missing song: %d", rec.Code)
	}
	rec = g.post(path+"/requests", url.Values{"participant_name": {"Alice"}, "song": {pick}, variant: {"Key +2"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("request: %d\n%s", rec.Code, rec.Body)
	}
	sr := requestByTitle(t, a, s, "Bohemian Rhapsody")
	if sr.SongArtist != "Queen" || sr.SongVersion != "Key +2" || sr.RequestSource != sourceAudience {
		t.Fatalf("request: %+v", sr)
	}

	// With freeform on, typed requests are allowed too.
	b.post(fmt.Sprintf("/shows/%d", s.ID), url.Values{"name": {""}, "show_type": {showTypeKaraoke}, "slug": {s.Slug}, "manual_entry_enabled": {"1"}})
	mustContain(t, "freeform", g.get(path).Body.String(), "Can't find your song?")
	if rec := g.post(path+"/requests", url.Values{"participant_name": {"Alice"}, "song_title": {"Typed"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("freeform on: %d\n%s", rec.Code, rec.Body)
	}

	// The host page offers the share dialog and display.
	mustContain(t, "host page", b.get(fmt.Sprintf("/shows/%d", s.ID)).Body.String(), `id="share-dialog"`, "http://example.test"+path, path+"/display", "<svg")
}

func TestQRSVG(t *testing.T) {
	svg, err := qrSVG("http://example.test/dj/a/shows/b", "qr-code")
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, `<svg `) || !strings.Contains(s, `class="qr-code"`) || !strings.Contains(s, `fill="#fff"`) || !strings.Contains(s, "<path") {
		t.Fatalf("svg: %s", s)
	}
}
