package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

const (
	guestCookie         = "guest"
	guestCookieLifetime = 400 * 24 * time.Hour // the longest browsers keep a cookie
	lastSeenEvery       = time.Hour

	audiencePageSize      = 25
	audienceRequestLimit  = 10
	audienceRequestWindow = 10 * time.Minute

	// The wait estimate uses this show's median song length once a few songs
	// have been performed, and this until then.
	defaultSongLength   = 4 * time.Minute
	minSongsForEstimate = 3

	songsLimitMessage = "You can't request more songs until the ones you've already requested are sung."
)

func djPath(handle string) string { return "/dj/" + url.PathEscape(handle) }

func showPath(handle, slug string) string {
	return djPath(handle) + "/shows/" + url.PathEscape(slug)
}

func (a *app) setGuestCookie(w http.ResponseWriter, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     a.cookieName(guestCookie),
		Value:    raw,
		Path:     "/",
		Expires:  a.now().Add(guestCookieLifetime),
		HttpOnly: true,
		Secure:   a.secureCookies(),
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *app) clearGuestCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: a.cookieName(guestCookie), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: a.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

// audienceProfile is the visitor's audience identity, or nil; it never
// creates one. A signed-in account, of any role, has its own profile. A
// signed-out visitor is a guest identified by the guest cookie.
func (a *app) audienceProfile(w http.ResponseWriter, r *http.Request) (*AudienceProfile, error) {
	var found []AudienceProfile
	if u := currentUser(r); u != nil {
		if err := a.db.Where("user_id = ?", u.ID).Limit(1).Find(&found).Error; err != nil {
			return nil, err
		}
	} else if c, err := r.Cookie(a.cookieName(guestCookie)); err == nil && c.Value != "" {
		if err := a.db.Where("guest_token_hash = ?", hashToken(c.Value)).Limit(1).Find(&found).Error; err != nil {
			return nil, err
		}
		if len(found) == 0 {
			a.clearGuestCookie(w)
		}
	}
	if len(found) == 0 {
		return nil, nil
	}
	p := &found[0]
	if now := a.now(); p.LastSeenAt == nil || now.Sub(*p.LastSeenAt) >= lastSeenEvery {
		p.LastSeenAt = &now
		if err := a.db.Model(p).Update("last_seen_at", now).Error; err != nil {
			return nil, err
		}
	}
	return p, nil
}

// ensureAudienceProfile is audienceProfile, creating the profile (and for a
// guest, the cookie) on first use.
func (a *app) ensureAudienceProfile(w http.ResponseWriter, r *http.Request) (*AudienceProfile, error) {
	if p, err := a.audienceProfile(w, r); p != nil || err != nil {
		return p, err
	}
	now := a.now()
	p := AudienceProfile{LastSeenAt: &now, CreatedAt: now}
	if u := currentUser(r); u != nil {
		p.UserID = &u.ID
		return &p, a.db.Create(&p).Error
	}
	raw, err := newToken()
	if err != nil {
		return nil, err
	}
	hash := hashToken(raw)
	p.GuestTokenHash = &hash
	if err := a.db.Create(&p).Error; err != nil {
		return nil, err
	}
	a.setGuestCookie(w, raw)
	return &p, nil
}

func (a *app) publicDJ(w http.ResponseWriter, r *http.Request) (User, bool) {
	var djs []User
	if err := a.db.Where("slug = ?", r.PathValue("handle")).Limit(1).Find(&djs).Error; err != nil {
		a.serverError(w, r, err)
		return User{}, false
	}
	if len(djs) == 0 {
		a.message(w, r, http.StatusNotFound, "DJ Not Found",
			"We couldn't find a DJ with that handle. Double-check the URL or scan the QR code again.")
		return User{}, false
	}
	return djs[0], true
}

func (a *app) publicShow(w http.ResponseWriter, r *http.Request) (User, Show, bool) {
	dj, ok := a.publicDJ(w, r)
	if !ok {
		return dj, Show{}, false
	}
	var shows []Show
	if err := a.db.Where("user_id = ? AND slug = ?", dj.ID, r.PathValue("slug")).Limit(1).Find(&shows).Error; err != nil {
		a.serverError(w, r, err)
		return dj, Show{}, false
	}
	if len(shows) == 0 {
		a.render(w, r, http.StatusNotFound, "message.html", messagePage{
			Title: "Show Not Found", Text: "This show doesn't exist or may have been removed.",
			LinkURL: djPath(dj.Handle()), LinkText: "See " + dj.PublicName() + "'s active shows",
		})
		return dj, Show{}, false
	}
	s := shows[0]
	if err := a.attachCatalog(&s); err != nil {
		a.serverError(w, r, err)
		return dj, s, false
	}
	return dj, s, true
}

// bannerEntry is a live show where the visitor has a request waiting or on stage.
type bannerEntry struct {
	Show string
	URL  string // the show's queue page
	Up   bool   // on stage now
}

// queueBanner is shown on audience pages other than the queue page itself.
func (a *app) queueBanner(p *AudienceProfile) ([]bannerEntry, error) {
	if p == nil {
		return nil, nil
	}
	var rows []struct {
		Name        string
		Slug        string
		Handle      string
		CatalogName *string
		Up          bool
	}
	err := a.db.Raw(`
		SELECT shows.name, shows.slug, users.slug AS handle, catalogs.name AS catalog_name,
		       MAX(song_requests.status = 'now_playing') AS up
		FROM song_requests
		JOIN show_participants ON show_participants.id = song_requests.show_participant_id
		JOIN shows ON shows.id = song_requests.show_id
		JOIN users ON users.id = shows.user_id
		LEFT JOIN catalogs ON catalogs.id = shows.catalog_id
		WHERE show_participants.audience_profile_id = ? AND shows.status = ?
		  AND song_requests.status IN ?
		GROUP BY shows.id ORDER BY shows.started_at`,
		p.ID, showActive, []string{requestPending, requestQueued, requestNowPlaying}).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	entries := make([]bannerEntry, 0, len(rows))
	for _, row := range rows {
		s := Show{Name: row.Name}
		if row.CatalogName != nil {
			s.Catalog = &Catalog{Name: *row.CatalogName}
		}
		entries = append(entries, bannerEntry{Show: s.DisplayName(), URL: showPath(row.Handle, row.Slug) + "/queue", Up: row.Up})
	}
	return entries, nil
}

// showParticipant is the profile's participant in a show, if it has one.
func showParticipant(db *gorm.DB, showID uint, p *AudienceProfile) (*ShowParticipant, error) {
	if p == nil {
		return nil, nil
	}
	var found []ShowParticipant
	if err := db.Where("show_id = ? AND audience_profile_id = ?", showID, p.ID).Limit(1).Find(&found).Error; err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, nil
	}
	return &found[0], nil
}

// waitingCount is a participant's requests that count toward the song limit.
func waitingCount(db *gorm.DB, participantID uint) (int64, error) {
	var n int64
	err := db.Model(&SongRequest{}).Where("show_participant_id = ? AND status IN ?", participantID,
		[]string{requestPending, requestQueued}).Count(&n).Error
	return n, err
}

type djProfilePage struct {
	DJ        User
	Active    []Show
	Scheduled []Show
	Banner    []bannerEntry
}

func (a *app) handleDJProfile(w http.ResponseWriter, r *http.Request) {
	dj, ok := a.publicDJ(w, r)
	if !ok {
		return
	}
	page := djProfilePage{DJ: dj}
	lists := []struct {
		status, order string
		into          *[]Show
	}{
		{showActive, "started_at DESC", &page.Active},
		{showScheduled, "scheduled_at IS NULL, scheduled_at, id", &page.Scheduled},
	}
	for _, l := range lists {
		if err := a.db.Where("user_id = ? AND status = ?", dj.ID, l.status).Order(l.order).Find(l.into).Error; err != nil {
			a.serverError(w, r, err)
			return
		}
		for i := range *l.into {
			if err := a.attachCatalog(&(*l.into)[i]); err != nil {
				a.serverError(w, r, err)
				return
			}
		}
	}
	profile, err := a.audienceProfile(w, r)
	if err == nil {
		page.Banner, err = a.queueBanner(profile)
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, http.StatusOK, "dj_profile.html", page)
}

// pickedSong is a catalog song the visitor chose, with the version or ID
// that will go on the request.
type pickedSong struct {
	Song       catalogSong
	Pick       string // "{version}:{song id}", as posted
	Variant    string // the picker's choice, when the catalog shows one
	Version    string
	ExternalID string
}

func (a *app) audiencePick(r *http.Request, s Show, pick string) (*pickedSong, string, error) {
	song, msg, err := a.pickedSong(r, s, pick)
	if err != nil || msg != "" {
		return nil, msg, err
	}
	variant := r.FormValue(fmt.Sprintf("variant-%d", song.ID))
	vd := s.VariantDisplay()
	p := &pickedSong{
		Song: song, Pick: pick,
		Version:    pickVariant(song.Versions, variant, vd == "version"),
		ExternalID: pickVariant(song.ExternalIDs, variant, vd == "id"),
	}
	switch vd {
	case "version":
		p.Variant = p.Version
	case "id":
		p.Variant = p.ExternalID
	}
	return p, "", nil
}

type audienceShowPage struct {
	DJ           User
	Show         Show
	Path         string // the show's public path
	Name         string // the singer's name, filled in for them
	LimitReached bool
	Search       catalogSearchView
	Picked       *pickedSong
	Title        string // the typed-in request, kept after a failure
	Artist       string
	Error        string
	Banner       []bannerEntry
	Hearts       bool // signed in, so results can be saved
	Lookup       savedLookup
}

// Heart is the save toggle for a search result. It shows as saved when any
// version of the song is saved.
func (p audienceShowPage) Heart(song catalogSong) heartView {
	return heartView{
		SavedID: p.Lookup.bySong[songKey(song.Title, song.Artist)],
		ShowID:  p.Show.ID,
		Pick:    fmt.Sprintf("%d:%d", p.Search.Version, song.ID),
		Next:    p.Path + "?q=" + url.QueryEscape(p.Search.Query),
		Title:   song.Title,
	}
}

// withHearts turns on save toggles for a signed-in visitor.
func (a *app) withHearts(r *http.Request, page *audienceShowPage, profile *AudienceProfile) error {
	if currentUser(r) == nil {
		return nil
	}
	page.Hearts = true
	var err error
	page.Lookup, err = a.savedLookupFor(profile)
	return err
}

func (a *app) audienceShowPageFor(w http.ResponseWriter, r *http.Request, dj User, s Show) (audienceShowPage, error) {
	page := audienceShowPage{DJ: dj, Show: s, Path: showPath(dj.Handle(), s.Slug)}
	if s.Catalog != nil {
		page.Search.CatalogID = s.Catalog.ID
		if s.Catalog.ActiveVersion != nil {
			page.Search.Version = *s.Catalog.ActiveVersion
		}
	}
	profile, err := a.audienceProfile(w, r)
	if err == nil {
		err = a.withHearts(r, &page, profile)
	}
	if err != nil || profile == nil {
		return page, err
	}
	if page.Banner, err = a.queueBanner(profile); err != nil {
		return page, err
	}
	p, err := showParticipant(a.db, s.ID, profile)
	if err == nil {
		page.Name, err = a.prefillName(profile, p)
	}
	if err != nil || p == nil {
		return page, err
	}
	if s.MaxSongsPerSinger != nil {
		n, err := waitingCount(a.db, p.ID)
		if err != nil {
			return page, err
		}
		page.LimitReached = n >= int64(*s.MaxSongsPerSinger)
	}
	return page, nil
}

// prefillName is the name for the request form: the singer's name in this
// show (p, may be nil), else the name the profile used last at any show.
func (a *app) prefillName(profile *AudienceProfile, p *ShowParticipant) (string, error) {
	if p != nil {
		return p.DisplayName, nil
	}
	if profile == nil {
		return "", nil
	}
	var names []string
	err := a.db.Model(&ShowParticipant{}).Where("audience_profile_id = ?", profile.ID).
		Order("id DESC").Limit(1).Pluck("display_name", &names).Error
	if len(names) == 0 {
		return "", err
	}
	return names[0], err
}

// searchable is whether the audience can search the show's catalog now.
func searchable(s Show) bool {
	return s.Active() && s.Catalog != nil && s.Catalog.ActiveVersion != nil
}

// handleAudienceShow is the request page. ?q= searches and ?song= picks a
// result, so the whole flow works without JavaScript.
func (a *app) handleAudienceShow(w http.ResponseWriter, r *http.Request) {
	dj, s, ok := a.publicShow(w, r)
	if !ok {
		return
	}
	page, err := a.audienceShowPageFor(w, r, dj, s)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	q := r.URL.Query()
	if searchable(s) && !page.LimitReached {
		if pick := q.Get("song"); pick != "" {
			page.Picked, page.Error, err = a.audiencePick(r, s, pick)
			if err != nil {
				a.serverError(w, r, err)
				return
			}
		} else if query := q.Get("q"); strings.TrimSpace(query) != "" {
			offset, _ := strconv.Atoi(q.Get("offset"))
			page.Search = a.searchPage(r, *s.Catalog, query, max(offset, 0), audiencePageSize)
		}
	}
	a.render(w, r, http.StatusOK, "audience_show.html", page)
}

// handleAudienceSearch is the search-as-you-type fragment. Its URL names the
// catalog version, so a matching response can be cached.
func (a *app) handleAudienceSearch(w http.ResponseWriter, r *http.Request) {
	dj, s, ok := a.publicShow(w, r)
	if !ok {
		return
	}
	page := audienceShowPage{DJ: dj, Show: s, Path: showPath(dj.Handle(), s.Slug)}
	profile, err := a.audienceProfile(w, r)
	if err == nil {
		err = a.withHearts(r, &page, profile)
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	q := r.URL.Query()
	if searchable(s) {
		if query := q.Get("q"); strings.TrimSpace(query) != "" {
			offset, _ := strconv.Atoi(q.Get("offset"))
			page.Search = a.searchPage(r, *s.Catalog, query, max(offset, 0), audiencePageSize)
			// Hearts change as songs are saved, so only signed-out results are cached.
			if !page.Hearts && q.Get("v") == strconv.Itoa(page.Search.Version) && page.Search.Error == "" {
				w.Header().Set("Cache-Control", "private, max-age=300")
			}
		}
	}
	block := "audience_results"
	if page.Search.Offset > 0 {
		block = "audience_more_results"
	}
	a.renderBlock(w, r, http.StatusOK, "audience_show.html", block, page)
}

func (a *app) handleAudienceRequest(w http.ResponseWriter, r *http.Request) {
	dj, s, ok := a.publicShow(w, r)
	if !ok {
		return
	}
	page, err := a.audienceShowPageFor(w, r, dj, s)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("participant_name"))
	if name != "" {
		page.Name = name
	}
	fail := func(status int, msg string) {
		page.Error = msg
		a.render(w, r, status, "audience_show.html", page)
	}
	if msg := notOpenMessage(s); msg != "" {
		fail(http.StatusConflict, msg)
		return
	}

	sr := SongRequest{RequestSource: sourceAudience}
	if pick := r.PostFormValue("song"); pick != "" {
		picked, msg, err := a.audiencePick(r, s, pick)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		if msg != "" {
			fail(http.StatusUnprocessableEntity, msg)
			return
		}
		page.Picked = picked
		sr.SongTitle, sr.SongArtist = picked.Song.Title, picked.Song.Artist
		sr.SongVersion, sr.SongExternalID = picked.Version, picked.ExternalID
	} else {
		page.Title = strings.TrimSpace(r.PostFormValue("song_title"))
		page.Artist = strings.TrimSpace(r.PostFormValue("song_artist"))
		switch {
		case s.Catalog != nil && !s.ManualEntryEnabled:
			fail(http.StatusUnprocessableEntity, "Pick a song from the catalog.")
			return
		case page.Title == "":
			fail(http.StatusUnprocessableEntity, "Song title can't be blank")
			return
		case utf8.RuneCountInString(page.Title) > maxSongFieldLen || utf8.RuneCountInString(page.Artist) > maxSongFieldLen:
			fail(http.StatusUnprocessableEntity, fmt.Sprintf("Song title and artist can be at most %d characters", maxSongFieldLen))
			return
		}
		sr.SongTitle, sr.SongArtist = page.Title, page.Artist
	}
	status, msg, err := a.placeAudienceRequest(w, r, s, name, &sr)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if msg != "" {
		fail(status, msg)
		return
	}
	http.Redirect(w, r, page.Path+"/queue?requested="+sr.Status, http.StatusSeeOther)
}

// notOpenMessage is why a show isn't taking requests, or "".
func notOpenMessage(s Show) string {
	switch {
	case s.Active():
		return ""
	case s.Scheduled():
		return "This show hasn't started yet."
	default:
		return "This show has ended and is no longer accepting requests."
	}
}

// placeAudienceRequest adds sr (song fields and source set) for the visitor's
// own singer in s, named name. A refusal is a status and message.
func (a *app) placeAudienceRequest(w http.ResponseWriter, r *http.Request, s Show, name string, sr *SongRequest) (int, string, error) {
	if msg := notOpenMessage(s); msg != "" {
		return http.StatusConflict, msg, nil
	}
	switch {
	case name == "":
		return http.StatusUnprocessableEntity, "Please enter your name.", nil
	case utf8.RuneCountInString(name) > maxDisplayNameLen:
		return http.StatusUnprocessableEntity, fmt.Sprintf("Name is too long (maximum is %d characters)", maxDisplayNameLen), nil
	}
	if !a.audienceLimiter.allow(a.clientIP(r) + "|" + strconv.FormatUint(uint64(s.ID), 10)) {
		return http.StatusTooManyRequests, "You're sending requests too quickly. Please wait a few minutes and try again.", nil
	}

	sr.ShowID, sr.Status, sr.RequestedAt = s.ID, requestQueued, a.now()
	if s.ApprovalRequired {
		sr.Status = requestPending
	}
	profile, err := a.ensureAudienceProfile(w, r)
	if err != nil {
		return 0, "", err
	}
	err = a.db.Transaction(func(tx *gorm.DB) error {
		p, err := showParticipant(tx, s.ID, profile)
		if err != nil {
			return err
		}
		if p == nil {
			p = &ShowParticipant{ShowID: s.ID, AudienceProfileID: &profile.ID, DisplayName: name}
			if err := tx.Create(p).Error; err != nil {
				return err
			}
		} else if p.DisplayName != name {
			if err := tx.Model(p).Update("display_name", name).Error; err != nil {
				return err
			}
		}
		if s.MaxSongsPerSinger != nil {
			n, err := waitingCount(tx, p.ID)
			if err != nil {
				return err
			}
			if n >= int64(*s.MaxSongsPerSinger) {
				return queueError(songsLimitMessage)
			}
		}
		sr.ShowParticipantID = p.ID
		if err := tx.Create(sr).Error; err != nil {
			return err
		}
		if sr.Status == requestQueued {
			return enqueue(tx, s, sr.ID)
		}
		return nil
	})
	var qerr queueError
	if errors.As(err, &qerr) {
		return http.StatusUnprocessableEntity, qerr.Error(), nil
	}
	return 0, "", err
}

// audienceLive is the polled part of the queue page.
type audienceLive struct {
	Show          Show
	Path          string
	NowPlaying    *queueRow
	Queue         []queueRow
	MeID          uint // the visitor's participant in this show, 0 if none
	Up            bool // the visitor is on stage
	Positions     []int
	Wait          time.Duration // until the visitor's first spot
	Pending       []queueRow    // the visitor's requests waiting for approval
	Rejected      []queueRow
	PendingNotice string // "host will review" flash; empty once nothing is awaiting approval
}

// PositionText is Rails' wording for where the visitor is.
func (l audienceLive) PositionText() string {
	if len(l.Positions) == 1 {
		return fmt.Sprintf("You are number %d of %d.", l.Positions[0], len(l.Queue))
	}
	spots := make([]string, len(l.Positions))
	for i, p := range l.Positions {
		spots[i] = fmt.Sprintf("#%d", p)
	}
	return fmt.Sprintf("Your spots: %s of %d.", strings.Join(spots, ", "), len(l.Queue))
}

// WaitMinutes rounds the estimate to whole minutes, at least one.
func (l audienceLive) WaitMinutes() int {
	return max(1, int((l.Wait+time.Minute/2)/time.Minute))
}

type cancelButton struct {
	Path string
	Row  queueRow
}

func cancelView(l audienceLive, r queueRow) cancelButton { return cancelButton{l.Path, r} }

type audienceQueuePage struct {
	DJ       User
	Show     Show
	Path     string
	Live     audienceLive
	Notice   string
	Error    string
	SignedIn bool
}

func (a *app) loadAudienceLive(w http.ResponseWriter, r *http.Request, dj User, s Show) (audienceLive, error) {
	live := audienceLive{Show: s, Path: showPath(dj.Handle(), s.Slug)}
	q, err := a.loadQueue(s)
	if err != nil {
		return live, err
	}
	live.NowPlaying, live.Queue = q.NowPlaying, q.Queue
	profile, err := a.audienceProfile(w, r)
	if err != nil {
		return live, err
	}
	p, err := showParticipant(a.db, s.ID, profile)
	if err != nil || p == nil {
		return live, err
	}
	live.MeID = p.ID
	live.Up = q.NowPlaying != nil && q.NowPlaying.ShowParticipantID == p.ID
	for i, row := range q.Queue {
		if row.ShowParticipantID == p.ID {
			live.Positions = append(live.Positions, i+1)
		}
	}
	for _, row := range q.Pending {
		if row.ShowParticipantID == p.ID {
			live.Pending = append(live.Pending, row)
		}
	}
	err = a.db.Table("song_requests").Where("show_id = ? AND show_participant_id = ? AND status = ?", s.ID, p.ID, requestRejected).
		Order("rejected_at DESC").Find(&live.Rejected).Error
	if err != nil || len(live.Positions) == 0 {
		return live, err
	}
	songLength, err := a.songLength(s.ID)
	live.Wait = time.Duration(live.Positions[0]) * songLength
	return live, err
}

// songLength is the median time from start to done in this show so far.
func (a *app) songLength(showID uint) (time.Duration, error) {
	var done []SongRequest
	err := a.db.Select("started_at", "completed_at").
		Where("show_id = ? AND status = ? AND started_at IS NOT NULL AND completed_at IS NOT NULL", showID, requestCompleted).
		Find(&done).Error
	if err != nil || len(done) < minSongsForEstimate {
		return defaultSongLength, err
	}
	lengths := make([]time.Duration, len(done))
	for i, sr := range done {
		lengths[i] = sr.CompletedAt.Sub(*sr.StartedAt)
	}
	slices.Sort(lengths)
	return lengths[len(lengths)/2], nil
}

var requestedNotices = map[string]string{
	requestPending: "Your request has been submitted! The host will review it shortly.",
	requestQueued:  "You've been added to the queue!",
}

func (a *app) handleAudienceQueue(w http.ResponseWriter, r *http.Request) {
	dj, s, ok := a.publicShow(w, r)
	if !ok {
		return
	}
	a.renderAudienceQueue(w, r, http.StatusOK, dj, s, "")
}

func (a *app) renderAudienceQueue(w http.ResponseWriter, r *http.Request, status int, dj User, s Show, errMsg string) {
	live, err := a.loadAudienceLive(w, r, dj, s)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	// The submitted flash stays only while a request is still awaiting approval.
	// Polls keep ?requested=pending so the flash leaves with the next update
	// once the host accepts, rejects, or the singer cancels.
	if r.URL.Query().Get("canceled") == "" && r.URL.Query().Get("requested") == requestPending && len(live.Pending) > 0 {
		live.PendingNotice = requestedNotices[requestPending]
	}
	if r.Header.Get("HX-Target") == "audience-live" {
		a.renderBlock(w, r, status, "audience_queue.html", "audience_live_block", live)
		return
	}
	page := audienceQueuePage{
		DJ: dj, Show: s, Path: live.Path, Live: live, Error: errMsg,
		Notice: requestedNotices[r.URL.Query().Get("requested")], SignedIn: currentUser(r) != nil,
	}
	if page.Notice == requestedNotices[requestPending] {
		page.Notice = ""
	}
	if r.URL.Query().Get("canceled") != "" {
		page.Notice = "Your request was canceled."
	}
	a.render(w, r, status, "audience_queue.html", page)
}

// handleAudienceCancel takes back one of the visitor's own requests.
func (a *app) handleAudienceCancel(w http.ResponseWriter, r *http.Request) {
	dj, s, ok := a.publicShow(w, r)
	if !ok {
		return
	}
	rid, err := strconv.ParseUint(r.PathValue("rid"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	profile, err := a.audienceProfile(w, r)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	p, err := showParticipant(a.db, s.ID, profile)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	var sr []SongRequest
	if p != nil {
		err = a.db.Where("id = ? AND show_id = ? AND show_participant_id = ?", rid, s.ID, p.ID).Limit(1).Find(&sr).Error
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if len(sr) == 0 {
		a.notFound(w, r)
		return
	}
	if !s.Active() {
		a.renderAudienceQueue(w, r, http.StatusConflict, dj, s, "This show has ended.")
		return
	}
	err = a.db.Transaction(func(tx *gorm.DB) error {
		cur, err := loadRequest(tx, sr[0].ID)
		if err != nil {
			return err
		}
		if cur.Status != requestPending && cur.Status != requestQueued {
			return queueError("This request can no longer be canceled.")
		}
		return cancelRequest(tx, s, cur.ID, a.now())
	})
	var qerr queueError
	if errors.As(err, &qerr) {
		a.renderAudienceQueue(w, r, http.StatusConflict, dj, s, qerr.Error())
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, showPath(dj.Handle(), s.Slug)+"/queue?canceled=1", http.StatusSeeOther)
}

type displayPage struct {
	DJ   User
	Show Show
	URL  string // the show's public page, for the QR code
	Live liveQueue
}

// handleDisplay is the TV display for the venue.
func (a *app) handleDisplay(w http.ResponseWriter, r *http.Request) {
	dj, s, ok := a.publicShow(w, r)
	if !ok {
		return
	}
	q, err := a.loadQueue(s)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	page := displayPage{DJ: dj, Show: s, URL: a.cfg.BaseURL + showPath(dj.Handle(), s.Slug), Live: q}
	if r.Header.Get("HX-Target") == "display-live" {
		a.renderBlock(w, r, http.StatusOK, "display.html", "display_live_block", page)
		return
	}
	a.render(w, r, http.StatusOK, "display.html", page)
}
