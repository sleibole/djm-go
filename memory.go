package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Song memory: a signed-in visitor's saved songs and past performances, and
// linking a guest's history to the account they sign in to.

// linkGuest runs when someone signs in. If this browser has a guest profile,
// its history becomes the account's, as Rails' LinkOrMerge: the guest profile
// is attached to the account, or merged into the account's own profile. All in
// one transaction; the guest cookie is cleared after it commits.
func (a *app) linkGuest(w http.ResponseWriter, r *http.Request, userID uint) error {
	c, err := r.Cookie(guestCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	var guests []AudienceProfile
	if err := a.db.Where("guest_token_hash = ?", hashToken(c.Value)).Limit(1).Find(&guests).Error; err != nil {
		return err
	}
	if len(guests) == 0 {
		a.clearGuestCookie(w)
		return nil
	}
	guest := guests[0]
	merged := false
	err = a.db.Transaction(func(tx *gorm.DB) error {
		var own []AudienceProfile
		if err := tx.Where("user_id = ?", userID).Limit(1).Find(&own).Error; err != nil {
			return err
		}
		if len(own) == 0 {
			return tx.Model(&guest).Updates(map[string]any{"user_id": userID, "guest_token_hash": nil}).Error
		}
		merged = true
		return mergeProfiles(tx, guest.ID, own[0].ID)
	})
	if err != nil {
		return err
	}
	a.clearGuestCookie(w)
	a.logger.Info("guest linked", "user_id", userID, "guest_profile_id", guest.ID, "merged", merged)
	return nil
}

// linkGuestOrLog is linkGuest for the sign-in handlers: a failure is logged,
// and sign-in goes ahead with the guest cookie left in place to try next time.
func (a *app) linkGuestOrLog(w http.ResponseWriter, r *http.Request, userID uint) {
	if err := a.linkGuest(w, r, userID); err != nil {
		a.logger.Error("link guest", "user_id", userID, "err", err)
	}
}

// mergeProfiles moves everything from one profile to another and deletes it.
// In a show where both have a singer, the requests move to the surviving singer.
func mergeProfiles(tx *gorm.DB, from, into uint) error {
	var singers []ShowParticipant
	if err := tx.Where("audience_profile_id = ?", from).Find(&singers).Error; err != nil {
		return err
	}
	for _, guest := range singers {
		survivor, err := showParticipant(tx, guest.ShowID, &AudienceProfile{ID: into})
		if err != nil {
			return err
		}
		if survivor == nil {
			if err := tx.Model(&guest).Update("audience_profile_id", into).Error; err != nil {
				return err
			}
			continue
		}
		if err := tx.Model(&SongRequest{}).Where("show_participant_id = ?", guest.ID).
			Update("show_participant_id", survivor.ID).Error; err != nil {
			return err
		}
		if err := tx.Delete(&guest).Error; err != nil {
			return err
		}
	}
	if err := tx.Exec(`UPDATE OR IGNORE saved_songs SET audience_profile_id = ? WHERE audience_profile_id = ?`, into, from).Error; err != nil {
		return err
	}
	return tx.Delete(&AudienceProfile{}, from).Error
}

// heartView is a save/unsave toggle. It carries what's needed to save the
// song again after an unsave: a catalog pick in a show, or a past request.
type heartView struct {
	SavedID   uint // 0 when not saved
	ShowID    uint
	Pick      string // "{version}:{song id}"
	RequestID uint
	Next      string // where a plain form post returns to
	Title     string // for the button's label
}

// savedLookup indexes the profile's saved songs by fingerprint and by title and artist.
type savedLookup struct {
	byFingerprint map[string]uint
	bySong        map[string]uint
}

func (a *app) savedLookupFor(profile *AudienceProfile) (savedLookup, error) {
	l := savedLookup{map[string]uint{}, map[string]uint{}}
	if profile == nil {
		return l, nil
	}
	var saved []SavedSong
	if err := a.db.Where("audience_profile_id = ?", profile.ID).Find(&saved).Error; err != nil {
		return l, err
	}
	for _, s := range saved {
		l.byFingerprint[s.Fingerprint] = s.ID
		l.bySong[songKey(s.SongTitle, s.SongArtist)] = s.ID
	}
	return l, nil
}

// memoryShow is the live show the visitor came from (?show= or a form's show),
// so the lists can offer to request a song there.
type memoryShow struct {
	Show Show
	Path string // its public page
}

func (a *app) memoryShowFor(r *http.Request) (*memoryShow, error) {
	id, err := strconv.ParseUint(r.FormValue("show"), 10, 64)
	if err != nil {
		return nil, nil
	}
	var shows []Show
	if err := a.db.Where("id = ?", id).Limit(1).Find(&shows).Error; err != nil || len(shows) == 0 {
		return nil, err
	}
	s := shows[0]
	if err := a.attachCatalog(&s); err != nil {
		return nil, err
	}
	var dj User
	if err := a.db.Take(&dj, s.UserID).Error; err != nil {
		return nil, err
	}
	return &memoryShow{Show: s, Path: showPath(dj.Handle(), s.Slug)}, nil
}

// performance is one of the visitor's completed songs.
type performance struct {
	SongRequest
	ShowName    string
	CatalogName *string
}

func (p performance) ShowDisplayName() string {
	s := Show{Name: p.ShowName}
	if p.CatalogName != nil {
		s.Catalog = &Catalog{Name: *p.CatalogName}
	}
	return s.DisplayName()
}

type memoryPage struct {
	SignedOut    bool
	Show         *memoryShow // the live show to request on, if any
	Name         string      // filled into the request forms
	Saved        []SavedSong
	Performances []performance
	Lookup       savedLookup
	Notice       string
	Error        string
	ErrorID      uint // the row the error is about, whose request form stays open
}

// CanRequest is whether the rows get a request form.
func (p memoryPage) CanRequest() bool { return p.Show != nil && p.Show.Show.Active() }

func (p memoryPage) ShowQuery() string {
	if p.Show == nil {
		return ""
	}
	return "?show=" + strconv.FormatUint(uint64(p.Show.Show.ID), 10)
}

// memoryRequest is a row's form to request its song at the page's show.
type memoryRequest struct {
	Action string
	Title  string
	ShowID uint
	Name   string
	Error  string // open with this error when the last try failed
}

func memoryRequestView(p memoryPage, id uint, action, title string) memoryRequest {
	m := memoryRequest{Action: action, Title: title, Name: p.Name}
	if p.Show != nil {
		m.ShowID = p.Show.Show.ID
	}
	if id == p.ErrorID {
		m.Error = p.Error
	}
	return m
}

// Heart is the toggle for a performance row.
func (p memoryPage) Heart(perf performance) heartView {
	h := heartView{RequestID: perf.ID, Next: "/me/performances" + p.ShowQuery(), Title: perf.SongTitle}
	h.SavedID = p.Lookup.byFingerprint[fingerprint(perf.SongTitle, perf.SongArtist, perf.SongVersion, perf.SongExternalID)]
	return h
}

func (a *app) memoryPageFor(w http.ResponseWriter, r *http.Request) (memoryPage, *AudienceProfile, error) {
	var page memoryPage
	if currentUser(r) == nil {
		page.SignedOut = true
		return page, nil, nil
	}
	profile, err := a.audienceProfile(w, r)
	if err != nil {
		return page, nil, err
	}
	if page.Show, err = a.memoryShowFor(r); err != nil {
		return page, nil, err
	}
	if page.Show != nil {
		p, err := showParticipant(a.db, page.Show.Show.ID, profile)
		if err == nil {
			page.Name, err = a.prefillName(profile, p)
		}
		if err != nil {
			return page, nil, err
		}
	} else if page.Name, err = a.prefillName(profile, nil); err != nil {
		return page, nil, err
	}
	page.Lookup, err = a.savedLookupFor(profile)
	return page, profile, err
}

func (a *app) loadSaved(page *memoryPage, profile *AudienceProfile) error {
	if profile == nil {
		return nil
	}
	return a.db.Where("audience_profile_id = ?", profile.ID).Order("updated_at DESC, id DESC").Find(&page.Saved).Error
}

func (a *app) loadPerformances(page *memoryPage, profile *AudienceProfile) error {
	if profile == nil {
		return nil
	}
	return a.db.Raw(`
		SELECT song_requests.*, shows.name AS show_name, catalogs.name AS catalog_name
		FROM song_requests
		JOIN show_participants ON show_participants.id = song_requests.show_participant_id
		JOIN shows ON shows.id = song_requests.show_id
		LEFT JOIN catalogs ON catalogs.id = shows.catalog_id
		WHERE show_participants.audience_profile_id = ? AND song_requests.status = ?
		ORDER BY song_requests.completed_at DESC, song_requests.id DESC`,
		profile.ID, requestCompleted).Scan(&page.Performances).Error
}

func (a *app) renderSaved(w http.ResponseWriter, r *http.Request, status int, page memoryPage, profile *AudienceProfile) {
	if err := a.loadSaved(&page, profile); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, status, "saved_songs.html", page)
}

func (a *app) renderPerformances(w http.ResponseWriter, r *http.Request, status int, page memoryPage, profile *AudienceProfile) {
	if err := a.loadPerformances(&page, profile); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, status, "performances.html", page)
}

func (a *app) handleSavedSongs(w http.ResponseWriter, r *http.Request) {
	page, profile, err := a.memoryPageFor(w, r)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if r.URL.Query().Get("removed") != "" {
		page.Notice = "Removed from saved songs."
	}
	a.renderSaved(w, r, http.StatusOK, page, profile)
}

func (a *app) handlePerformances(w http.ResponseWriter, r *http.Request) {
	page, profile, err := a.memoryPageFor(w, r)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.renderPerformances(w, r, http.StatusOK, page, profile)
}

// ownRequest is one of the profile's requests, or nil.
func (a *app) ownRequest(profile *AudienceProfile, id string) (*SongRequest, error) {
	if profile == nil {
		return nil, nil
	}
	var found []SongRequest
	err := a.db.Raw(`
		SELECT song_requests.* FROM song_requests
		JOIN show_participants ON show_participants.id = song_requests.show_participant_id
		WHERE song_requests.id = ? AND show_participants.audience_profile_id = ?`, id, profile.ID).Scan(&found).Error
	if err != nil || len(found) == 0 {
		return nil, err
	}
	return &found[0], nil
}

// handleSaveSong saves a song from a show's catalog search or from a past request.
func (a *app) handleSaveSong(w http.ResponseWriter, r *http.Request) {
	profile, err := a.ensureAudienceProfile(w, r)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	heart := heartView{Next: safeNext(r.PostFormValue("next"))}
	saved := SavedSong{AudienceProfileID: profile.ID}
	if rid := r.PostFormValue("request"); rid != "" {
		sr, err := a.ownRequest(profile, rid)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		if sr == nil {
			a.notFound(w, r)
			return
		}
		var s Show
		if err := a.db.Take(&s, sr.ShowID).Error; err != nil {
			a.serverError(w, r, err)
			return
		}
		heart.RequestID = sr.ID
		saved.SongTitle, saved.SongArtist, saved.SongVersion, saved.SongExternalID = sr.SongTitle, sr.SongArtist, sr.SongVersion, sr.SongExternalID
		saved.SourceCatalogID = s.CatalogID
	} else {
		ms, err := a.memoryShowFor(r)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		if ms == nil || ms.Show.Catalog == nil {
			a.notFound(w, r)
			return
		}
		picked, msg, err := a.audiencePick(r, ms.Show, r.PostFormValue("song"))
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		if msg != "" {
			a.message(w, r, http.StatusUnprocessableEntity, "Couldn't save that song", msg)
			return
		}
		heart.ShowID, heart.Pick = ms.Show.ID, picked.Pick
		saved.SongTitle, saved.SongArtist = picked.Song.Title, picked.Song.Artist
		saved.SongVersion, saved.SongExternalID = picked.Version, picked.ExternalID
		saved.SourceCatalogID = &ms.Show.Catalog.ID
	}
	saved.Fingerprint = fingerprint(saved.SongTitle, saved.SongArtist, saved.SongVersion, saved.SongExternalID)
	heart.Title = saved.SongTitle

	// A double tap can race; the unique index keeps one row and both get it.
	err = a.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&saved).Error
	if err == nil {
		err = a.db.Where("audience_profile_id = ? AND fingerprint = ?", profile.ID, saved.Fingerprint).Take(&saved).Error
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	heart.SavedID = saved.ID
	a.respondHeart(w, r, heart)
}

func (a *app) respondHeart(w http.ResponseWriter, r *http.Request, heart heartView) {
	if r.Header.Get("HX-Request") == "true" {
		a.renderBlock(w, r, http.StatusOK, "saved_songs.html", "heart_block", heart)
		return
	}
	http.Redirect(w, r, heart.Next, http.StatusSeeOther)
}

// ownSaved is the {id} saved song if it's the visitor's.
func (a *app) ownSaved(w http.ResponseWriter, r *http.Request) (*AudienceProfile, *SavedSong, bool) {
	profile, err := a.audienceProfile(w, r)
	if err != nil {
		a.serverError(w, r, err)
		return nil, nil, false
	}
	var found []SavedSong
	if profile != nil {
		err = a.db.Where("id = ? AND audience_profile_id = ?", r.PathValue("id"), profile.ID).Limit(1).Find(&found).Error
	}
	if err != nil {
		a.serverError(w, r, err)
		return nil, nil, false
	}
	if len(found) == 0 {
		a.notFound(w, r)
		return nil, nil, false
	}
	return profile, &found[0], true
}

func (a *app) handleUnsaveSong(w http.ResponseWriter, r *http.Request) {
	_, saved, ok := a.ownSaved(w, r)
	if !ok {
		return
	}
	if err := a.db.Delete(saved).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	if r.PostFormValue("heart") == "" {
		if r.Header.Get("HX-Request") != "true" {
			http.Redirect(w, r, "/me/saved?removed=1"+strings.Replace(memoryShowQuery(r), "?", "&", 1), http.StatusSeeOther)
			return
		}
		page, profile, err := a.memoryPageFor(w, r)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		page.Notice = "Removed from saved songs."
		a.renderSaved(w, r, http.StatusOK, page, profile)
		return
	}
	heart := heartView{Next: safeNext(r.PostFormValue("next")), Pick: r.PostFormValue("song"), Title: saved.SongTitle}
	if id, err := strconv.ParseUint(r.PostFormValue("show"), 10, 64); err == nil {
		heart.ShowID = uint(id)
	}
	if id, err := strconv.ParseUint(r.PostFormValue("request"), 10, 64); err == nil {
		heart.RequestID = uint(id)
	}
	a.respondHeart(w, r, heart)
}

func memoryShowQuery(r *http.Request) string {
	if s := r.FormValue("show"); s != "" {
		return "?show=" + url.QueryEscape(s)
	}
	return ""
}

// inShowCatalog is whether a remembered song may be requested on s. A show
// that only takes catalog songs only takes ones in its catalog (matching title
// and artist ignoring case). Changed from Rails, which took any saved song.
func (a *app) inShowCatalog(ctx context.Context, s Show, title, artist string) (bool, error) {
	if s.Catalog == nil || s.ManualEntryEnabled {
		return true, nil
	}
	if s.Catalog.ActiveVersion == nil {
		return false, nil
	}
	db, err := a.cache.open(ctx, s.Catalog.ID, *s.Catalog.ActiveVersion)
	if err != nil {
		return false, err
	}
	var one int
	err = db.QueryRowContext(ctx, `SELECT 1 FROM songs WHERE lower(title) = lower(?) AND lower(artist) = lower(?) LIMIT 1`,
		title, artist).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// requestRemembered requests a saved song or a past performance on the show
// in the form. It returns the queue page to go to, or the refusal.
func (a *app) requestRemembered(w http.ResponseWriter, r *http.Request, ms *memoryShow, sr SongRequest) (string, int, string, error) {
	if ms == nil {
		return "", http.StatusUnprocessableEntity, "Choose a show to request from.", nil
	}
	if notOpenMessage(ms.Show) != "" {
		return "", http.StatusConflict, "That show is not accepting requests right now.", nil
	}
	ok, err := a.inShowCatalog(r.Context(), ms.Show, sr.SongTitle, sr.SongArtist)
	if err != nil || !ok {
		return "", http.StatusUnprocessableEntity, "That song isn't in this show's catalog.", err
	}
	sr.RequestSource = sourceSaved
	status, msg, err := a.placeAudienceRequest(w, r, ms.Show, strings.TrimSpace(r.PostFormValue("participant_name")), &sr)
	if err != nil || msg != "" {
		return "", status, msg, err
	}
	return ms.Path + "/queue?requested=" + sr.Status, 0, "", nil
}

func (a *app) handleRequestSaved(w http.ResponseWriter, r *http.Request) {
	_, saved, ok := a.ownSaved(w, r)
	if !ok {
		return
	}
	page, profile, err := a.memoryPageFor(w, r)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	to, status, msg, err := a.requestRemembered(w, r, page.Show, SongRequest{
		SongTitle: saved.SongTitle, SongArtist: saved.SongArtist, SongVersion: saved.SongVersion, SongExternalID: saved.SongExternalID,
	})
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if msg != "" {
		page.Error, page.ErrorID = msg, saved.ID
		page.Name = strings.TrimSpace(r.PostFormValue("participant_name"))
		a.renderSaved(w, r, status, page, profile)
		return
	}
	err = a.db.Model(saved).Updates(map[string]any{
		"request_count": gorm.Expr("request_count + 1"), "last_requested_at": a.now(),
	}).Error
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// handleSingAgain requests one of the visitor's past songs.
func (a *app) handleSingAgain(w http.ResponseWriter, r *http.Request) {
	page, profile, err := a.memoryPageFor(w, r)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	past, err := a.ownRequest(profile, r.PathValue("rid"))
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if past == nil || past.Status != requestCompleted {
		a.notFound(w, r)
		return
	}
	to, status, msg, err := a.requestRemembered(w, r, page.Show, SongRequest{
		SongTitle: past.SongTitle, SongArtist: past.SongArtist, SongVersion: past.SongVersion, SongExternalID: past.SongExternalID,
	})
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if msg != "" {
		page.Error, page.ErrorID = msg, past.ID
		page.Name = strings.TrimSpace(r.PostFormValue("participant_name"))
		a.renderPerformances(w, r, status, page, profile)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}
