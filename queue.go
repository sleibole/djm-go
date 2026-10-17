package main

import (
	"database/sql"
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
	maxSongFieldLen        = 200
	maxRejectionReasonLen  = 500
	participantSuggestions = 10
)

// queueError is a request the queue refuses, with the message for the host.
type queueError string

func (e queueError) Error() string { return string(e) }

// errQueueChanged means a drag referred to a request that has left the queue.
var errQueueChanged = queueError("The queue changed. It has been reloaded.")

type queueRow struct {
	SongRequest
	Participant string
	Version     string `gorm:"-"` // what to show, per the catalog's variant setting
	ExternalID  string `gorm:"-"`
}

type liveQueue struct {
	Show       Show
	Error      string
	Pending    []queueRow
	NowPlaying *queueRow
	Queue      []queueRow
	Done       []queueRow // completed and skipped, newest first
}

func (a *app) loadQueue(s Show) (liveQueue, error) {
	q := liveQueue{Show: s}
	var rows []queueRow
	err := a.db.Table("song_requests").
		Select("song_requests.*, show_participants.display_name AS participant").
		Joins("JOIN show_participants ON show_participants.id = song_requests.show_participant_id").
		Where("song_requests.show_id = ? AND song_requests.status IN ?", s.ID,
			[]string{requestPending, requestQueued, requestNowPlaying, requestCompleted, requestSkipped}).
		Order("song_requests.queue_position, song_requests.requested_at, song_requests.id").
		Find(&rows).Error
	if err != nil {
		return q, err
	}
	vd := s.VariantDisplay()
	for _, row := range rows {
		if vd == "" || vd == "version" {
			row.Version = row.SongVersion
		}
		if vd == "" || vd == "id" {
			row.ExternalID = row.SongExternalID
		}
		switch row.Status {
		case requestPending:
			q.Pending = append(q.Pending, row)
		case requestQueued:
			q.Queue = append(q.Queue, row)
		case requestNowPlaying:
			q.NowPlaying = &row
		default:
			q.Done = append(q.Done, row)
		}
	}
	slices.SortStableFunc(q.Done, func(x, y queueRow) int {
		return finishedAt(y).Compare(finishedAt(x))
	})
	return q, nil
}

func finishedAt(r queueRow) time.Time {
	if r.CompletedAt != nil {
		return *r.CompletedAt
	}
	return r.RequestedAt
}

// queueIDs is the queued requests in order.
func queueIDs(tx *gorm.DB, showID uint) ([]uint, error) {
	var ids []uint
	err := tx.Model(&SongRequest{}).Where("show_id = ? AND status = ?", showID, requestQueued).
		Order("queue_position IS NULL, queue_position, id").Pluck("id", &ids).Error
	return ids, err
}

// setQueueOrder numbers the queued requests 1..n in the given order.
func setQueueOrder(tx *gorm.DB, ids []uint) error {
	for i, id := range ids {
		if err := tx.Model(&SongRequest{}).Where("id = ?", id).Update("queue_position", i+1).Error; err != nil {
			return err
		}
	}
	return nil
}

func renumberQueue(tx *gorm.DB, showID uint) error {
	ids, err := queueIDs(tx, showID)
	if err != nil {
		return err
	}
	return setQueueOrder(tx, ids)
}

// insertIndex is where a rotation style puts a request joining the queue.
// Standard rotation: the bottom.
func insertIndex(s Show, queue []uint) int {
	return len(queue)
}

// enqueue makes a request queued and places it by the show's rotation style.
func enqueue(tx *gorm.DB, s Show, id uint) error {
	if err := tx.Model(&SongRequest{}).Where("id = ?", id).
		Updates(map[string]any{"status": requestQueued, "queue_position": nil}).Error; err != nil {
		return err
	}
	ids, err := queueIDs(tx, s.ID)
	if err != nil {
		return err
	}
	ids = slices.DeleteFunc(ids, func(x uint) bool { return x == id })
	return setQueueOrder(tx, slices.Insert(ids, insertIndex(s, ids), id))
}

// loadRequest reads a request inside tx, so its status can't change before
// the update that depends on it.
func loadRequest(tx *gorm.DB, id uint) (SongRequest, error) {
	var sr SongRequest
	err := tx.First(&sr, id).Error
	return sr, err
}

func approveRequest(tx *gorm.DB, s Show, id uint, now time.Time) error {
	sr, err := loadRequest(tx, id)
	if err != nil {
		return err
	}
	if sr.Status != requestPending {
		return queueError("Request must be pending to approve.")
	}
	return enqueue(tx, s, id)
}

func rejectRequest(tx *gorm.DB, s Show, id uint, reason string, now time.Time) error {
	sr, err := loadRequest(tx, id)
	if err != nil {
		return err
	}
	if sr.Status != requestPending {
		return queueError("Request must be pending to reject.")
	}
	return tx.Model(&SongRequest{}).Where("id = ?", id).Updates(map[string]any{
		"status": requestRejected, "rejected_at": now, "rejection_reason": reason, "queue_position": nil,
	}).Error
}

// startRequest puts a queued request on stage. Whoever is on stage now is
// done, so the host never has to finish one song before starting the next.
func startRequest(tx *gorm.DB, s Show, id uint, now time.Time) error {
	sr, err := loadRequest(tx, id)
	if err != nil {
		return err
	}
	if sr.Status != requestQueued {
		return queueError("Only a queued request can be started.")
	}
	if err := tx.Model(&SongRequest{}).Where("show_id = ? AND status = ?", s.ID, requestNowPlaying).
		Updates(map[string]any{"status": requestCompleted, "completed_at": now}).Error; err != nil {
		return err
	}
	if err := tx.Model(&SongRequest{}).Where("id = ?", id).Updates(map[string]any{
		"status": requestNowPlaying, "started_at": now, "queue_position": nil,
	}).Error; err != nil {
		return err
	}
	return renumberQueue(tx, s.ID)
}

func completeRequest(tx *gorm.DB, s Show, id uint, now time.Time) error {
	sr, err := loadRequest(tx, id)
	if err != nil {
		return err
	}
	if sr.Status != requestNowPlaying {
		return queueError("Only the current song can be marked done.")
	}
	return tx.Model(&SongRequest{}).Where("id = ?", id).
		Updates(map[string]any{"status": requestCompleted, "completed_at": now}).Error
}

func skipRequest(tx *gorm.DB, s Show, id uint, now time.Time) error {
	sr, err := loadRequest(tx, id)
	if err != nil {
		return err
	}
	if sr.Status != requestQueued && sr.Status != requestNowPlaying {
		return queueError("Only a queued or now playing request can be skipped.")
	}
	if err := tx.Model(&SongRequest{}).Where("id = ?", id).Updates(map[string]any{
		"status": requestSkipped, "completed_at": now, "queue_position": nil,
	}).Error; err != nil {
		return err
	}
	return renumberQueue(tx, s.ID)
}

func cancelRequest(tx *gorm.DB, s Show, id uint, now time.Time) error {
	sr, err := loadRequest(tx, id)
	if err != nil {
		return err
	}
	if sr.Status != requestPending && sr.Status != requestQueued && sr.Status != requestNowPlaying {
		return queueError("This request cannot be canceled.")
	}
	if err := tx.Model(&SongRequest{}).Where("id = ?", id).Updates(map[string]any{
		"status": requestCanceled, "canceled_at": now, "queue_position": nil,
	}).Error; err != nil {
		return err
	}
	return renumberQueue(tx, s.ID)
}

// moveRequest places a queued request right after another one (after == 0
// means the top). Requests that joined during the drag keep their places.
func moveRequest(tx *gorm.DB, s Show, id, after uint) error {
	ids, err := queueIDs(tx, s.ID)
	if err != nil {
		return err
	}
	if !slices.Contains(ids, id) || id == after || (after != 0 && !slices.Contains(ids, after)) {
		return errQueueChanged
	}
	ids = slices.DeleteFunc(ids, func(x uint) bool { return x == id })
	at := 0
	if after != 0 {
		at = slices.Index(ids, after) + 1
	}
	return setQueueOrder(tx, slices.Insert(ids, at, id))
}

// requestAction is one host action on a request, run in a transaction.
type requestAction func(tx *gorm.DB, s Show, id uint, r *http.Request, now time.Time) error

func (a *app) requestActions() map[string]requestAction {
	simple := func(f func(*gorm.DB, Show, uint, time.Time) error) requestAction {
		return func(tx *gorm.DB, s Show, id uint, _ *http.Request, now time.Time) error { return f(tx, s, id, now) }
	}
	return map[string]requestAction{
		"approve": simple(approveRequest),
		"start":   simple(startRequest),
		"done":    simple(completeRequest),
		"skip":    simple(skipRequest),
		"delete":  simple(cancelRequest),
		"reject": func(tx *gorm.DB, s Show, id uint, r *http.Request, now time.Time) error {
			reason := strings.TrimSpace(r.PostFormValue("rejection_reason"))
			if utf8.RuneCountInString(reason) > maxRejectionReasonLen {
				return queueError(fmt.Sprintf("Reason is too long (maximum is %d characters)", maxRejectionReasonLen))
			}
			return rejectRequest(tx, s, id, reason, now)
		},
		"move": func(tx *gorm.DB, s Show, id uint, r *http.Request, _ time.Time) error {
			var after uint64
			if v := r.PostFormValue("after"); v != "" {
				var err error
				if after, err = strconv.ParseUint(v, 10, 64); err != nil {
					return errQueueChanged
				}
			}
			return moveRequest(tx, s, id, uint(after))
		},
	}
}

// handleRequestAction runs POST /requests/{rid}/{action}. HTMX gets the live
// part of the show page back; a plain form post is redirected to the page.
func (a *app) handleRequestAction(w http.ResponseWriter, r *http.Request) {
	action, ok := a.requestActions()[r.PathValue("action")]
	if !ok {
		a.notFound(w, r)
		return
	}
	rid, err := strconv.ParseUint(r.PathValue("rid"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return
	}
	var sr SongRequest
	if err := a.db.First(&sr, rid).Error; err != nil {
		a.notFound(w, r)
		return
	}
	s, err := a.loadShow(sr.ShowID, currentUser(r).ID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		a.notFound(w, r)
		return
	} else if err != nil {
		a.serverError(w, r, err)
		return
	}

	if s.Active() {
		err = a.db.Transaction(func(tx *gorm.DB) error { return action(tx, s, sr.ID, r, a.now()) })
	} else {
		err = queueError("This show has ended.")
		if s.Scheduled() {
			err = queueError("This show hasn't started yet.")
		}
	}
	var qerr queueError
	if err != nil && !errors.As(err, &qerr) {
		a.serverError(w, r, err)
		return
	}

	if r.PathValue("action") == "move" {
		// The page already shows the new order; reorder.js only needs to know it stuck.
		if err != nil {
			http.Error(w, qerr.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	status := http.StatusOK
	if err != nil {
		status = http.StatusConflict
	}
	if r.Header.Get("HX-Request") != "true" {
		if err == nil {
			http.Redirect(w, r, fmt.Sprintf("/shows/%d", s.ID), http.StatusSeeOther)
			return
		}
		a.renderShow(w, r, status, s, func(p *showPage) { p.Error = qerr.Error() })
		return
	}
	a.renderLive(w, r, status, s, string(qerr))
}

func (a *app) renderLive(w http.ResponseWriter, r *http.Request, status int, s Show, msg string) {
	q, err := a.loadQueue(s)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	q.Error = msg
	a.renderBlock(w, r, status, "show.html", "show_live_block", q)
}

// handleShowLive is polled by the host page.
func (a *app) handleShowLive(w http.ResponseWriter, r *http.Request) {
	s, ok := a.ownShow(w, r)
	if !ok {
		return
	}
	if !s.Active() {
		w.Header().Set("HX-Redirect", fmt.Sprintf("/shows/%d", s.ID))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.renderLive(w, r, http.StatusOK, s, "")
}

// addForm is the host's Add to Queue form, kept as typed after a failure.
type addForm struct {
	Participant string
	Title       string
	Artist      string
	Manual      bool // the manual entry fields were used
	Error       string
}

// handleAddRequest adds a request for a singer, picked from the catalog
// search (the "song" button, "{version}:{song id}") or typed in.
func (a *app) handleAddRequest(w http.ResponseWriter, r *http.Request) {
	s, ok := a.ownShow(w, r)
	if !ok {
		return
	}
	f := addForm{
		Participant: strings.TrimSpace(r.PostFormValue("participant_name")),
		Title:       strings.TrimSpace(r.PostFormValue("song_title")),
		Artist:      strings.TrimSpace(r.PostFormValue("song_artist")),
	}
	pick := r.PostFormValue("song")
	f.Manual = pick == ""
	fail := func(status int, msg string) {
		f.Error = msg
		a.renderShow(w, r, status, s, func(p *showPage) { p.Add = f })
	}
	if !s.Active() {
		// The Add to Queue form is only on a live show's page.
		msg := "This show has ended."
		if s.Scheduled() {
			msg = "Start the show before adding requests."
		}
		a.renderShow(w, r, http.StatusConflict, s, func(p *showPage) { p.Error = msg })
		return
	}
	switch {
	case f.Participant == "":
		fail(http.StatusUnprocessableEntity, s.ParticipantLabel()+"'s name is required.")
		return
	case utf8.RuneCountInString(f.Participant) > maxDisplayNameLen:
		fail(http.StatusUnprocessableEntity, fmt.Sprintf("Name is too long (maximum is %d characters)", maxDisplayNameLen))
		return
	}

	sr := SongRequest{ShowID: s.ID, SongTitle: f.Title, SongArtist: f.Artist, RequestSource: sourceHostManual, Status: requestQueued, RequestedAt: a.now()}
	if pick != "" {
		song, msg, err := a.pickedSong(r, s, pick)
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		if msg != "" {
			fail(http.StatusUnprocessableEntity, msg)
			return
		}
		variant := r.PostFormValue(fmt.Sprintf("variant-%d", song.ID))
		sr.SongTitle, sr.SongArtist, sr.RequestSource = song.Title, song.Artist, sourceHostCatalog
		sr.SongVersion = pickVariant(song.Versions, variant, s.VariantDisplay() == "version")
		sr.SongExternalID = pickVariant(song.ExternalIDs, variant, s.VariantDisplay() == "id")
	} else {
		switch {
		case f.Title == "":
			fail(http.StatusUnprocessableEntity, "Song title can't be blank")
			return
		case utf8.RuneCountInString(f.Title) > maxSongFieldLen || utf8.RuneCountInString(f.Artist) > maxSongFieldLen:
			fail(http.StatusUnprocessableEntity, fmt.Sprintf("Song title and artist can be at most %d characters", maxSongFieldLen))
			return
		}
	}

	var name string
	err := a.db.Transaction(func(tx *gorm.DB) error {
		p, err := hostParticipant(tx, s, f.Participant, a.now())
		if err != nil {
			return err
		}
		name = p.DisplayName
		if err := checkSongLimit(tx, s, p); err != nil {
			return err
		}
		sr.ShowParticipantID = p.ID
		if err := tx.Create(&sr).Error; err != nil {
			return err
		}
		return enqueue(tx, s, sr.ID)
	})
	var qerr queueError
	if errors.As(err, &qerr) {
		fail(http.StatusUnprocessableEntity, qerr.Error())
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/shows/%d?added=%s", s.ID, url.QueryEscape(name)), http.StatusSeeOther)
}

// pickedSong loads the song a host picked from search results. The value
// names the catalog version searched, so a rebuild in between can't swap in
// a different song with the same id.
func (a *app) pickedSong(r *http.Request, s Show, pick string) (catalogSong, string, error) {
	const gone = "That song is no longer in the catalog. Search again."
	if s.Catalog == nil {
		return catalogSong{}, gone, nil
	}
	v, id, ok := strings.Cut(pick, ":")
	version, err1 := strconv.Atoi(v)
	songID, err2 := strconv.ParseInt(id, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		return catalogSong{}, gone, nil
	}
	db, err := a.cache.open(r.Context(), s.Catalog.ID, version)
	if errors.Is(err, errBlobNotFound) {
		return catalogSong{}, gone, nil
	}
	if err != nil {
		return catalogSong{}, "", err
	}
	song, err := catalogSongByID(db, songID)
	if errors.Is(err, sql.ErrNoRows) {
		return catalogSong{}, gone, nil
	}
	return song, "", err
}

// variantPicker is one search result's version or ID choice in Add to Queue.
type variantPicker struct {
	SongID  int64
	Options []string
}

func pickVariantView(songID int64, options []string) variantPicker {
	return variantPicker{songID, options}
}

// pickVariant is the chosen version or ID when the picker showed that kind,
// else the song's first, as Rails stored it.
func pickVariant(options []string, chosen string, picker bool) string {
	if picker && slices.Contains(options, chosen) {
		return chosen
	}
	if len(options) > 0 {
		return options[0]
	}
	return ""
}

// hostParticipant finds the singer the host typed in this show, ignoring
// case, or adds them.
func hostParticipant(tx *gorm.DB, s Show, name string, now time.Time) (ShowParticipant, error) {
	var found []ShowParticipant
	if err := tx.Where("show_id = ? AND display_name = ? COLLATE NOCASE", s.ID, name).Order("id").Limit(1).Find(&found).Error; err != nil {
		return ShowParticipant{}, err
	}
	if len(found) > 0 {
		return found[0], nil
	}
	profile := AudienceProfile{CreatedAt: now}
	if err := tx.Create(&profile).Error; err != nil {
		return ShowParticipant{}, err
	}
	p := ShowParticipant{ShowID: s.ID, AudienceProfileID: &profile.ID, DisplayName: name}
	return p, tx.Create(&p).Error
}

func checkSongLimit(tx *gorm.DB, s Show, p ShowParticipant) error {
	if s.MaxSongsPerSinger == nil {
		return nil
	}
	n, err := waitingCount(tx, p.ID)
	if err != nil {
		return err
	}
	if n >= int64(*s.MaxSongsPerSinger) {
		return queueError(p.DisplayName + " already has the maximum number of songs queued.")
	}
	return nil
}

// handleParticipantSuggestions fills the singer name's datalist: this show's
// singers whose names start with what was typed.
func (a *app) handleParticipantSuggestions(w http.ResponseWriter, r *http.Request) {
	s, ok := a.ownShow(w, r)
	if !ok {
		return
	}
	var names []string
	if q := strings.TrimSpace(r.URL.Query().Get("participant_name")); q != "" {
		like := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		if err := a.db.Model(&ShowParticipant{}).Where(`show_id = ? AND display_name LIKE ? ESCAPE '\'`, s.ID, like).
			Order("display_name COLLATE NOCASE").Limit(participantSuggestions).Distinct().Pluck("display_name", &names).Error; err != nil {
			a.serverError(w, r, err)
			return
		}
	}
	a.renderBlock(w, r, http.StatusOK, "show.html", "participant_options", names)
}

// handleShowSearch is the host's catalog search in Add to Queue.
func (a *app) handleShowSearch(w http.ResponseWriter, r *http.Request) {
	s, ok := a.ownShow(w, r)
	if !ok {
		return
	}
	page := showPage{Show: s}
	if s.Catalog != nil {
		page.Search.CatalogID = s.Catalog.ID
		if q := r.URL.Query().Get("q"); strings.TrimSpace(q) != "" && s.Catalog.ActiveVersion != nil {
			page.Search = a.searchView(r, *s.Catalog, q)
		}
	}
	a.renderBlock(w, r, http.StatusOK, "show.html", "add_results", page)
}
