package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// createShow posts the new-show form with sensible defaults and returns the show.
func createShow(t *testing.T, a *app, b *browser, form url.Values) Show {
	t.Helper()
	defaults := url.Values{"show_type": {showTypeKaraoke}, "approval_required": {"1"}, "timing": {"now"}}
	for k, v := range defaults {
		if !form.Has(k) {
			form[k] = v
		}
	}
	rec := b.post("/shows", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create show: %d\n%s", rec.Code, rec.Body)
	}
	var s Show
	if err := a.db.Order("id DESC").First(&s).Error; err != nil {
		t.Fatal(err)
	}
	return s
}

func reloadShow(t *testing.T, a *app, s Show) Show {
	t.Helper()
	var fresh Show
	if err := a.db.First(&fresh, s.ID).Error; err != nil {
		t.Fatal(err)
	}
	return fresh
}

func addManual(t *testing.T, b *browser, s Show, singer, title string) *httptest.ResponseRecorder {
	t.Helper()
	return b.post(fmt.Sprintf("/shows/%d/requests", s.ID), url.Values{"participant_name": {singer}, "song_title": {title}, "song_artist": {"Someone"}})
}

func requestByTitle(t *testing.T, a *app, s Show, title string) SongRequest {
	t.Helper()
	var sr SongRequest
	if err := a.db.Where("show_id = ? AND song_title = ?", s.ID, title).First(&sr).Error; err != nil {
		t.Fatalf("request %q: %v", title, err)
	}
	return sr
}

// queueTitles is the queued songs in order.
func queueTitles(t *testing.T, a *app, s Show) []string {
	t.Helper()
	var titles []string
	if err := a.db.Model(&SongRequest{}).Where("show_id = ? AND status = ?", s.ID, requestQueued).
		Order("queue_position").Pluck("song_title", &titles).Error; err != nil {
		t.Fatal(err)
	}
	return titles
}

// act posts a host action on a request, as HTMX does when hx is set.
func act(b *browser, sr SongRequest, action string, form url.Values, hx bool) *httptest.ResponseRecorder {
	b.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/requests/%d/%s", sr.ID, action), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if hx {
		req.Header.Set("HX-Request", "true")
	}
	return b.do(req)
}

func move(b *browser, sr SongRequest, after string) *httptest.ResponseRecorder {
	b.t.Helper()
	return act(b, sr, "move", url.Values{"after": {after}}, false)
}

func isPinned(a *app, id uint) bool {
	a.cache.mu.Lock()
	defer a.cache.mu.Unlock()
	return a.cache.pinned[id]
}

func TestCreateShowStartNow(t *testing.T) {
	a := newTestApp(t)
	b, u := signInHost(t, a, "dj@example.com")
	a.db.Model(&u).Update("slug", nil)

	s := createShow(t, a, b, url.Values{"name": {"Friday Night Karaoke!"}, "max_songs": {"2"}})
	if s.Status != showActive || s.StartedAt == nil || s.Slug != "friday-night-karaoke" || !s.ManualEntryEnabled ||
		!s.ApprovalRequired || s.MaxSongsPerSinger == nil || *s.MaxSongsPerSinger != 2 || s.RotationStyle != rotationStandard {
		t.Fatalf("show: %+v", s)
	}
	var fresh User
	a.db.First(&fresh, u.ID)
	if fresh.Slug == nil {
		t.Fatal("starting a show should give the DJ a handle")
	}
	body := b.get(fmt.Sprintf("/shows/%d?saved=started", s.ID)).Body.String()
	for _, want := range []string{"Show started!", "<mark>Live</mark>", `id="show-live"`, "The queue is empty.", "End Show", "/dj/" + *fresh.Slug + "/shows/friday-night-karaoke"} {
		if !strings.Contains(body, want) {
			t.Errorf("show page lacks %q", want)
		}
	}
	if body := b.get("/shows").Body.String(); !strings.Contains(body, "Friday Night Karaoke!") {
		t.Errorf("shows list:\n%s", body)
	}

	// One live show at a time.
	rec := b.post("/shows", url.Values{"name": {"Another"}, "show_type": {showTypeKaraoke}, "timing": {"now"}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "You already have a live show.") ||
		!strings.Contains(rec.Body.String(), fmt.Sprintf(`href="/shows/%d"`, s.ID)) {
		t.Fatalf("second live show: %d\n%s", rec.Code, rec.Body)
	}
	if body := b.get("/shows/new").Body.String(); !strings.Contains(body, "is live now") {
		t.Errorf("new show page should warn about the live show")
	}

	// The same name gets the next free slug.
	later := createShow(t, a, b, url.Values{"name": {"Friday Night Karaoke!"}, "timing": {"later"}, "scheduled_at": {"2026-12-31T20:00:00-05:00"}})
	if later.Status != showScheduled || later.Slug != "friday-night-karaoke-1" || later.ScheduledAt == nil ||
		!later.ScheduledAt.Equal(time.Date(2027, 1, 1, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("scheduled show: %+v", later)
	}
	rec = b.post(fmt.Sprintf("/shows/%d/start", later.ID), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "You already have a live show.") {
		t.Fatalf("start while live: %d", rec.Code)
	}

	if rec := b.post(fmt.Sprintf("/shows/%d/end", s.ID), nil); rec.Header().Get("Location") != "/shows?saved=ended" {
		t.Fatalf("end: %d %v", rec.Code, rec.Header())
	}
	if s = reloadShow(t, a, s); s.Status != showEnded || s.EndedAt == nil {
		t.Fatalf("ended: %+v", s)
	}
	if rec := b.post(fmt.Sprintf("/shows/%d/start", s.ID), nil); rec.Code != http.StatusSeeOther || reloadShow(t, a, s).Status != showEnded {
		t.Fatal("an ended show must stay ended")
	}
	if rec := b.post(fmt.Sprintf("/shows/%d/start", later.ID), nil); rec.Header().Get("Location") != fmt.Sprintf("/shows/%d?saved=started", later.ID) {
		t.Fatalf("start: %d %v", rec.Code, rec.Header())
	}
	if later = reloadShow(t, a, later); later.Status != showActive || later.StartedAt == nil {
		t.Fatalf("started: %+v", later)
	}
}

func TestCreateShowErrors(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	other, _ := signInHost(t, a, "other@example.com")
	theirs := createCatalog(t, a, other, "Theirs")

	for _, tc := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"timing": {"later"}}, "Pick a date and time for the show."},
		{url.Values{"timing": {"later"}, "scheduled_local": {"tomorrow"}}, "Pick a date and time for the show."},
		{url.Values{"slug": {"Bad Slug"}}, "URL slug must be lowercase letters, numbers, and hyphens"},
		{url.Values{"max_songs": {"0"}}, "Max songs must be a number from 1 to 99"},
		{url.Values{"show_type": {"party"}}, "Pick a show type."},
		{url.Values{"catalog_id": {fmt.Sprint(theirs.ID)}}, "Pick one of your catalogs."},
		{url.Values{"name": {strings.Repeat("x", 101)}}, "Name is too long"},
	} {
		form := url.Values{"show_type": {showTypeKaraoke}, "timing": {"now"}}
		for k, v := range tc.form {
			form[k] = v
		}
		rec := b.post("/shows", form)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%v: %d, want %q", tc.form, rec.Code, tc.want)
		}
	}
	var n int64
	a.db.Model(&Show{}).Count(&n)
	if n != 0 {
		t.Fatalf("%d shows created", n)
	}

	// Without JavaScript the datetime-local value is taken as UTC.
	s := createShow(t, a, b, url.Values{"name": {"Later"}, "timing": {"later"}, "scheduled_local": {"2026-11-01T19:30"}})
	if !s.ScheduledAt.Equal(time.Date(2026, 11, 1, 19, 30, 0, 0, time.UTC)) {
		t.Fatalf("scheduled at %v", s.ScheduledAt)
	}
	if rec := b.post("/shows", url.Values{"show_type": {showTypeKaraoke}, "timing": {"now"}, "slug": {"later"}}); !strings.Contains(rec.Body.String(), "URL slug has already been taken") {
		t.Errorf("duplicate slug: %d", rec.Code)
	}
	// An unnamed show gets a random slug.
	if s := createShow(t, a, b, url.Values{}); !strings.HasPrefix(s.Slug, "show-") || len(s.Slug) != 9 {
		t.Errorf("unnamed show slug %q", s.Slug)
	}
}

func TestParameterize(t *testing.T) {
	for in, want := range map[string]string{
		"Friday Night Karaoke!": "friday-night-karaoke",
		"  Café Ölé  ":          "cafe-ole",
		"80's & 90's":           "80-s-90-s",
		"!!!":                   "",
	} {
		if got := parameterize(in); got != want {
			t.Errorf("parameterize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := truncateSlug("abc-def-ghi", 8); got != "abc-def" {
		t.Errorf("truncateSlug = %q", got)
	}
}

func TestShowSettings(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	s := createShow(t, a, b, url.Values{"name": {"Original"}})

	rec := b.post(fmt.Sprintf("/shows/%d", s.ID), url.Values{"name": {"Renamed"}, "slug": {"renamed"}, "show_type": {showTypeDJ}, "max_songs": {"3"}})
	if rec.Header().Get("Location") != fmt.Sprintf("/shows/%d?saved=settings", s.ID) {
		t.Fatalf("settings: %d\n%s", rec.Code, rec.Body)
	}
	s = reloadShow(t, a, s)
	if s.Name != "Renamed" || s.Slug != "renamed" || s.ShowType != showTypeDJ || s.ApprovalRequired || !s.ManualEntryEnabled || *s.MaxSongsPerSinger != 3 {
		t.Fatalf("after settings: %+v", s)
	}
	if body := b.get(fmt.Sprintf("/shows/%d", s.ID)).Body.String(); !strings.Contains(body, "Guest") {
		t.Error("a DJ show calls participants guests")
	}
	if rec := b.post(fmt.Sprintf("/shows/%d", s.ID), url.Values{"slug": {""}, "show_type": {showTypeDJ}}); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "URL slug can&#39;t be blank") {
		t.Fatalf("blank slug: %d", rec.Code)
	}

	b.post(fmt.Sprintf("/shows/%d/end", s.ID), nil)
	if rec := b.post(fmt.Sprintf("/shows/%d", s.ID), url.Values{"slug": {"again"}, "show_type": {showTypeDJ}}); rec.Code != http.StatusConflict {
		t.Fatalf("settings on an ended show: %d", rec.Code)
	}
}

func TestHostQueue(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	s := createShow(t, a, b, url.Values{"name": {"Queue"}})

	for _, add := range [][2]string{{"Alice", "Song A"}, {"Bob", "Song B"}, {"alice", "Song C"}, {"Cara", "Song D"}} {
		if rec := addManual(t, b, s, add[0], add[1]); rec.Code != http.StatusSeeOther {
			t.Fatalf("add %v: %d\n%s", add, rec.Code, rec.Body)
		}
	}
	if rec := addManual(t, b, s, "Bob", "Song B"); !strings.Contains(rec.Header().Get("Location"), "added=Bob") {
		t.Fatalf("added redirect: %v", rec.Header())
	}
	var participants []ShowParticipant
	a.db.Where("show_id = ?", s.ID).Order("id").Find(&participants)
	if len(participants) != 3 || participants[0].DisplayName != "Alice" || participants[0].AudienceProfileID == nil {
		t.Fatalf("participants: %+v", participants)
	}
	sr := requestByTitle(t, a, s, "Song C")
	if sr.ShowParticipantID != participants[0].ID || sr.RequestSource != sourceHostManual || sr.SongArtist != "Someone" {
		t.Fatalf("request: %+v", sr)
	}
	// The second "Song B" is not needed below.
	var extra SongRequest
	a.db.Where("show_id = ? AND song_title = ?", s.ID, "Song B").Order("id DESC").First(&extra)
	if rec := act(b, extra, "delete", nil, false); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rec.Code)
	}
	if got := queueTitles(t, a, s); !slices.Equal(got, []string{"Song A", "Song B", "Song C", "Song D"}) {
		t.Fatalf("queue: %v", got)
	}

	// Drag and drop.
	songA, songB, songC, songD := requestByTitle(t, a, s, "Song A"), requestByTitle(t, a, s, "Song B"), requestByTitle(t, a, s, "Song C"), requestByTitle(t, a, s, "Song D")
	if rec := move(b, songC, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("move to top: %d %s", rec.Code, rec.Body)
	}
	if got := queueTitles(t, a, s); !slices.Equal(got, []string{"Song C", "Song A", "Song B", "Song D"}) {
		t.Fatalf("after move to top: %v", got)
	}
	move(b, songA, fmt.Sprint(songD.ID))
	if got := queueTitles(t, a, s); !slices.Equal(got, []string{"Song C", "Song B", "Song D", "Song A"}) {
		t.Fatalf("after move to bottom: %v", got)
	}
	for _, after := range []string{fmt.Sprint(songA.ID), "999", "x"} {
		if rec := move(b, songA, after); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "The queue changed.") {
			t.Errorf("move after %s: %d %s", after, rec.Code, rec.Body)
		}
	}

	// Starting a song finishes the one on stage.
	rec := act(b, songC, "start", nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `id="show-live"`) || strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("start (htmx): %d\n%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "<strong>Alice</strong> — Song C") {
		t.Errorf("now playing:\n%s", rec.Body)
	}
	act(b, songB, "start", nil, false)
	if sr := requestByTitle(t, a, s, "Song C"); sr.Status != requestCompleted || sr.CompletedAt == nil {
		t.Fatalf("previous song: %+v", sr)
	}
	if sr := requestByTitle(t, a, s, "Song B"); sr.Status != requestNowPlaying || sr.StartedAt == nil || sr.QueuePosition != nil {
		t.Fatalf("started: %+v", sr)
	}
	if got := queueTitles(t, a, s); !slices.Equal(got, []string{"Song D", "Song A"}) {
		t.Fatalf("queue after start: %v", got)
	}

	// Refused actions explain themselves.
	rec = act(b, songD, "done", nil, true)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Only the current song can be marked done.") {
		t.Fatalf("done on queued: %d\n%s", rec.Code, rec.Body)
	}
	rec = act(b, songD, "approve", nil, false)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Request must be pending to approve.") || !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("approve queued: %d", rec.Code)
	}

	act(b, songB, "done", nil, true)
	act(b, songD, "skip", nil, true)
	if sr := requestByTitle(t, a, s, "Song B"); sr.Status != requestCompleted {
		t.Fatalf("done: %+v", sr)
	}
	if sr := requestByTitle(t, a, s, "Song D"); sr.Status != requestSkipped || sr.CompletedAt == nil {
		t.Fatalf("skip: %+v", sr)
	}
	if sr := requestByTitle(t, a, s, "Song A"); *sr.QueuePosition != 1 {
		t.Fatalf("renumbered: %+v", sr)
	}
	body := b.get(fmt.Sprintf("/shows/%d/live", s.ID)).Body.String()
	if !strings.Contains(body, "Completed (3)") || !strings.Contains(body, "Skipped") || !strings.Contains(body, "No one is performing right now.") {
		t.Fatalf("live:\n%s", body)
	}

	// Ending the show: the song on stage counts, the rest are canceled.
	act(b, songA, "start", nil, true)
	addManual(t, b, s, "Dan", "Song E")
	b.post(fmt.Sprintf("/shows/%d/end", s.ID), nil)
	if sr := requestByTitle(t, a, s, "Song A"); sr.Status != requestCompleted {
		t.Errorf("now playing at end: %+v", sr)
	}
	if sr := requestByTitle(t, a, s, "Song E"); sr.Status != requestCanceled || sr.CanceledAt == nil || sr.QueuePosition != nil {
		t.Errorf("queued at end: %+v", sr)
	}
	if rec := b.get(fmt.Sprintf("/shows/%d/live", s.ID)); rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "" || rec.Header().Get("HX-Retarget") != "#content" || rec.Header().Get("HX-Push-Url") != fmt.Sprintf("/shows/%d", s.ID) || !strings.Contains(rec.Body.String(), "Songs Performed") {
		t.Errorf("live after end: %d %v\n%s", rec.Code, rec.Header(), rec.Body)
	}
	if rec := act(b, songA, "skip", nil, true); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "This show has ended.") {
		t.Errorf("action after end: %d", rec.Code)
	}
	if rec := addManual(t, b, s, "Eve", "Song F"); rec.Code != http.StatusConflict {
		t.Errorf("add after end: %d", rec.Code)
	}
	if body := b.get(fmt.Sprintf("/shows/%d", s.ID)).Body.String(); !strings.Contains(body, "Songs Performed <small>(4)</small>") || strings.Contains(body, "Show Settings") {
		t.Errorf("ended page:\n%s", body)
	}
}

func TestApproveAndReject(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	s := createShow(t, a, b, url.Values{"name": {"Approvals"}})
	addManual(t, b, s, "Alice", "Queued")

	p := ShowParticipant{ShowID: s.ID, DisplayName: "Fan"}
	a.db.Create(&p)
	pending := func(title string) SongRequest {
		sr := SongRequest{ShowID: s.ID, ShowParticipantID: p.ID, SongTitle: title, RequestSource: sourceAudience, Status: requestPending, RequestedAt: a.now()}
		if err := a.db.Create(&sr).Error; err != nil {
			t.Fatal(err)
		}
		return sr
	}
	keep, drop := pending("Keep"), pending("Drop")

	body := b.get(fmt.Sprintf("/shows/%d", s.ID)).Body.String()
	if !strings.Contains(body, "Pending Requests <small>(2)</small>") || !strings.Contains(body, fmt.Sprintf(`id="reject-%d"`, drop.ID)) {
		t.Fatalf("pending section:\n%s", body)
	}
	if rec := act(b, keep, "approve", nil, true); rec.Code != http.StatusOK {
		t.Fatalf("approve: %d", rec.Code)
	}
	if got := queueTitles(t, a, s); !slices.Equal(got, []string{"Queued", "Keep"}) {
		t.Fatalf("approved goes to the bottom: %v", got)
	}
	if rec := act(b, drop, "reject", url.Values{"rejection_reason": {strings.Repeat("x", 501)}}, true); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Reason is too long") {
		t.Fatalf("long reason: %d", rec.Code)
	}
	act(b, drop, "reject", url.Values{"rejection_reason": {" Not tonight "}}, true)
	if sr := requestByTitle(t, a, s, "Drop"); sr.Status != requestRejected || sr.RejectionReason != "Not tonight" || sr.RejectedAt == nil {
		t.Fatalf("rejected: %+v", sr)
	}
	if rec := act(b, drop, "reject", nil, true); !strings.Contains(rec.Body.String(), "Request must be pending to reject.") {
		t.Fatalf("reject twice: %d", rec.Code)
	}
}

func TestAddRequestErrorsAndSongLimit(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	s := createShow(t, a, b, url.Values{"name": {"Limits"}, "max_songs": {"1"}})

	for _, tc := range []struct {
		singer, title, want string
	}{
		{"", "Song", "Singer&#39;s name is required."},
		{strings.Repeat("x", 51), "Song", "Name is too long"},
		{"Alice", "", "Song title can&#39;t be blank"},
		{"Alice", strings.Repeat("x", 201), "at most 200 characters"},
	} {
		rec := addManual(t, b, s, tc.singer, tc.title)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("add %q %q: %d, want %q", tc.singer, tc.title, rec.Code, tc.want)
		}
	}
	if rec := addManual(t, b, s, "Alice", "First"); rec.Code != http.StatusSeeOther {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := addManual(t, b, s, "ALICE", "Second")
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Alice already has the maximum number of songs queued.") ||
		!strings.Contains(rec.Body.String(), `value="ALICE"`) {
		t.Fatalf("over the limit: %d\n%s", rec.Code, rec.Body)
	}
	// Once on stage, the song no longer counts.
	act(b, requestByTitle(t, a, s, "First"), "start", nil, true)
	if rec := addManual(t, b, s, "Alice", "Second"); rec.Code != http.StatusSeeOther {
		t.Fatalf("after starting: %d", rec.Code)
	}

	scheduled := createShow(t, a, b, url.Values{"name": {"Later"}, "timing": {"later"}, "scheduled_local": {"2027-01-01T20:00"}})
	if rec := addManual(t, b, scheduled, "Alice", "Song"); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Start the show before adding requests.") {
		t.Errorf("add to scheduled: %d", rec.Code)
	}
}

func TestParticipantSuggestions(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	old := createShow(t, a, b, url.Values{"name": {"Old"}})
	addManual(t, b, old, "Alfred", "Song")
	b.post(fmt.Sprintf("/shows/%d/end", old.ID), nil)

	s := createShow(t, a, b, url.Values{"name": {"Now"}})
	for _, name := range []string{"Alice", "albert", "Bob", "Al_x"} {
		addManual(t, b, s, name, "Song")
	}
	suggest := func(q string) string {
		return b.get(fmt.Sprintf("/shows/%d/participants?participant_name=%s", s.ID, url.QueryEscape(q))).Body.String()
	}
	if got := suggest("al"); got != `<option value="Al_x"></option><option value="albert"></option><option value="Alice"></option>` {
		t.Errorf("suggestions for al: %s", got)
	}
	if got := suggest("al_"); got != `<option value="Al_x"></option>` {
		t.Errorf("underscore is literal: %s", got)
	}
	if got := suggest(" "); got != "" {
		t.Errorf("blank: %s", got)
	}
}

func TestShowWithCatalog(t *testing.T) {
	a := newTestApp(t)
	b, u := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Songs")
	uploadCSV(t, b, c, fixture(t, "valid_kj_versions.csv"))
	build(t, a)
	b.post(fmt.Sprintf("/catalogs/%d", c.ID), url.Values{"variant_display": {"version"}})

	s := createShow(t, a, b, url.Values{"name": {"Catalog Show"}, "catalog_id": {fmt.Sprint(c.ID)}})
	if s.CatalogID == nil || *s.CatalogID != c.ID || s.ManualEntryEnabled {
		t.Fatalf("show: %+v", s)
	}
	if !isPinned(a, c.ID) {
		t.Fatal("a live show's catalog should be pinned")
	}

	body := b.get(fmt.Sprintf("/shows/%d/search?q=bohemian", s.ID)).Body.String()
	if !strings.Contains(body, "Bohemian Rhapsody") || !strings.Contains(body, `value="1:`) || !strings.Contains(body, "<option>Key &#43;2</option>") {
		t.Fatalf("search:\n%s", body)
	}
	if body := b.get(fmt.Sprintf("/shows/%d?q=journey", s.ID)).Body.String(); !strings.Contains(body, "Don&#39;t Stop Believin&#39;") {
		t.Fatal("page search without JavaScript")
	}

	db, err := a.cache.open(t.Context(), c.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	songs, err := searchCatalog(db, "bohemian", 10, 0, "")
	if err != nil || len(songs.Songs) != 1 {
		t.Fatalf("search: %v %+v", err, songs)
	}
	song := songs.Songs[0]
	rec := b.post(fmt.Sprintf("/shows/%d/requests", s.ID), url.Values{
		"participant_name": {"Alice"}, "song": {fmt.Sprintf("1:%d", song.ID)}, fmt.Sprintf("variant-%d", song.ID): {"Key +2"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add from catalog: %d\n%s", rec.Code, rec.Body)
	}
	sr := requestByTitle(t, a, s, "Bohemian Rhapsody")
	if sr.SongArtist != "Queen" || sr.SongVersion != "Key +2" || sr.RequestSource != sourceHostCatalog {
		t.Fatalf("request: %+v", sr)
	}
	for _, pick := range []string{"9:1", fmt.Sprintf("1:%d", 99999), "junk"} {
		rec := b.post(fmt.Sprintf("/shows/%d/requests", s.ID), url.Values{"participant_name": {"Bob"}, "song": {pick}})
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "That song is no longer in the catalog.") {
			t.Errorf("pick %q: %d", pick, rec.Code)
		}
	}
	if body := b.get(fmt.Sprintf("/shows/%d/live", s.ID)).Body.String(); !strings.Contains(body, "<small>Key &#43;2</small>") {
		t.Errorf("live queue shows the version:\n%s", body)
	}

	// The catalog and the account can't go while the show is live.
	rec = b.post(fmt.Sprintf("/catalogs/%d/delete", c.ID), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "End all active shows before deleting this catalog.") {
		t.Fatalf("catalog delete: %d", rec.Code)
	}
	rec = b.post("/account/delete", url.Values{"confirm": {u.Email}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "End all active shows before deleting your account.") {
		t.Fatalf("account delete: %d", rec.Code)
	}

	b.post(fmt.Sprintf("/shows/%d/end", s.ID), nil)
	if isPinned(a, c.ID) {
		t.Error("an ended show's catalog should be unpinned")
	}
	if rec := b.post(fmt.Sprintf("/catalogs/%d/delete", c.ID), nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("catalog delete after end: %d", rec.Code)
	}
	// As in Rails, the catalog's shows go with it; the delete dialog says so.
	var n int64
	a.db.Model(&Show{}).Where("id = ?", s.ID).Count(&n)
	if n != 0 {
		t.Fatal("the catalog's show should be deleted with it")
	}
}

func TestPinsSurviveRestart(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	c := createCatalog(t, a, b, "Songs")
	createShow(t, a, b, url.Values{"catalog_id": {fmt.Sprint(c.ID)}})
	a.cache.setPinned(nil)
	a.refreshPins()
	if !isPinned(a, c.ID) {
		t.Fatal("refreshPins should pin live shows' catalogs")
	}
}

func TestShowAccess(t *testing.T) {
	a := newTestApp(t)
	b, _ := signInHost(t, a, "dj@example.com")
	s := createShow(t, a, b, url.Values{"name": {"Mine"}})
	addManual(t, b, s, "Alice", "Song")
	sr := requestByTitle(t, a, s, "Song")

	other, _ := signInHost(t, a, "other@example.com")
	for _, path := range []string{"/shows/%d", "/shows/%d/live", "/shows/%d/search?q=x", "/shows/%d/participants?participant_name=a"} {
		if rec := other.get(fmt.Sprintf(path, s.ID)); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s by another DJ: %d", path, rec.Code)
		}
	}
	for _, path := range []string{"/shows/%d", "/shows/%d/start", "/shows/%d/end", "/shows/%d/requests"} {
		if rec := other.post(fmt.Sprintf(path, s.ID), url.Values{"participant_name": {"x"}, "song_title": {"x"}}); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s by another DJ: %d", path, rec.Code)
		}
	}
	for _, action := range []string{"start", "delete", "move", "nonsense"} {
		if rec := act(other, sr, action, nil, false); rec.Code != http.StatusNotFound {
			t.Errorf("%s by another DJ: %d", action, rec.Code)
		}
	}
	if reloadShow(t, a, s).Status != showActive || requestByTitle(t, a, s, "Song").Status != requestQueued {
		t.Fatal("another DJ changed the show")
	}

	fan := newBrowser(t, a)
	signIn(t, a, fan, "fan@example.com", "")
	if rec := fan.get("/shows"); rec.Code != http.StatusForbidden {
		t.Errorf("audience member on /shows: %d", rec.Code)
	}
}
