package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
	"gorm.io/gorm"
)

const (
	maxShowNameLen  = 100
	maxShowSlugLen  = 50
	recentShowLimit = 20
)

// ownShow loads the {id} show if it belongs to the current user, with its
// catalog; anyone else gets a 404.
func (a *app) ownShow(w http.ResponseWriter, r *http.Request) (Show, bool) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		a.notFound(w, r)
		return Show{}, false
	}
	s, err := a.loadShow(uint(id), currentUser(r).ID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		a.notFound(w, r)
		return s, false
	}
	if err != nil {
		a.serverError(w, r, err)
		return s, false
	}
	return s, true
}

func (a *app) loadShow(id, userID uint) (Show, error) {
	var s Show
	if err := a.db.Where("id = ? AND user_id = ?", id, userID).First(&s).Error; err != nil {
		return s, err
	}
	return s, a.attachCatalog(&s)
}

func (a *app) attachCatalog(s *Show) error {
	if s.CatalogID == nil {
		return nil
	}
	var c Catalog
	if err := a.db.First(&c, *s.CatalogID).Error; err != nil {
		return err
	}
	s.Catalog = &c
	return nil
}

// activeShow is the user's live show, if any.
func (a *app) activeShow(userID uint) (*Show, error) {
	var shows []Show
	if err := a.db.Where("user_id = ? AND status = ?", userID, showActive).Limit(1).Find(&shows).Error; err != nil {
		return nil, err
	}
	if len(shows) == 0 {
		return nil, nil
	}
	return &shows[0], a.attachCatalog(&shows[0])
}

// refreshPins keeps the catalogs of live shows in the local cache.
func (a *app) refreshPins() {
	var ids []uint
	if err := a.db.Model(&Show{}).Where("status = ? AND catalog_id IS NOT NULL", showActive).Pluck("catalog_id", &ids).Error; err != nil {
		a.logger.Error("refresh catalog pins", "err", err)
		return
	}
	a.cache.setPinned(ids)
}

type showListItem struct {
	Show
	Queued int
	Played int
}

type showsPage struct {
	Scheduled []showListItem
	Active    []showListItem
	Ended     []showListItem
	Saved     string
}

func (a *app) handleShows(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	page := showsPage{Saved: r.URL.Query().Get("saved")}
	lists := []struct {
		status, order string
		limit         int
		into          *[]showListItem
	}{
		{showScheduled, "scheduled_at IS NULL, scheduled_at, id", -1, &page.Scheduled},
		{showActive, "started_at DESC", -1, &page.Active},
		{showEnded, "ended_at DESC, id DESC", recentShowLimit, &page.Ended},
	}
	for _, l := range lists {
		var shows []Show
		if err := a.db.Where("user_id = ? AND status = ?", u.ID, l.status).Order(l.order).Limit(l.limit).Find(&shows).Error; err != nil {
			a.serverError(w, r, err)
			return
		}
		for _, s := range shows {
			item := showListItem{Show: s}
			if err := a.attachCatalog(&item.Show); err != nil {
				a.serverError(w, r, err)
				return
			}
			var queued, played int64
			a.db.Model(&SongRequest{}).Where("show_id = ? AND status = ?", s.ID, requestQueued).Count(&queued)
			a.db.Model(&SongRequest{}).Where("show_id = ? AND status IN ?", s.ID, []string{requestCompleted, requestSkipped}).Count(&played)
			item.Queued, item.Played = int(queued), int(played)
			*l.into = append(*l.into, item)
		}
	}
	a.render(w, r, http.StatusOK, "shows.html", page)
}

// showForm is what the new-show and settings forms post, kept as typed so a
// failed save can show it again.
type showForm struct {
	Name           string
	Slug           string
	CatalogID      string
	ShowType       string
	Approval       bool
	ManualEntry    bool
	Timing         string // "now" or "later"; new shows only
	ScheduledLocal string // the datetime-local field, in the browser's time zone
	ScheduledAt    string // the same moment in UTC, filled in by app.js
	MaxSongs       string
}

func readShowForm(r *http.Request) showForm {
	return showForm{
		Name:           strings.TrimSpace(r.PostFormValue("name")),
		Slug:           strings.ToLower(strings.TrimSpace(r.PostFormValue("slug"))),
		CatalogID:      r.PostFormValue("catalog_id"),
		ShowType:       r.PostFormValue("show_type"),
		Approval:       r.PostFormValue("approval_required") == "1",
		ManualEntry:    r.PostFormValue("manual_entry_enabled") == "1",
		Timing:         r.PostFormValue("timing"),
		ScheduledLocal: r.PostFormValue("scheduled_local"),
		ScheduledAt:    r.PostFormValue("scheduled_at"),
		MaxSongs:       strings.TrimSpace(r.PostFormValue("max_songs")),
	}
}

// scheduledTime prefers the UTC value app.js adds. Without JavaScript the
// datetime-local value is taken as UTC.
func (f showForm) scheduledTime() (*time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, f.ScheduledAt); err == nil {
		t = t.UTC()
		return &t, true
	}
	if t, err := time.Parse("2006-01-02T15:04", f.ScheduledLocal); err == nil {
		return &t, true
	}
	return nil, f.ScheduledLocal == "" && f.ScheduledAt == ""
}

func (f showForm) maxSongs() (*int, string) {
	if f.MaxSongs == "" {
		return nil, ""
	}
	n, err := strconv.Atoi(f.MaxSongs)
	if err != nil || n < 1 || n > maxSongsLimit {
		return nil, "Max songs must be a number from 1 to 99, or blank for no limit."
	}
	return &n, ""
}

// checkShowFields validates what the new-show and settings forms share.
func (a *app) checkShowFields(f showForm, userID, showID uint) string {
	switch {
	case utf8.RuneCountInString(f.Name) > maxShowNameLen:
		return fmt.Sprintf("Name is too long (maximum is %d characters)", maxShowNameLen)
	case f.ShowType != showTypeKaraoke && f.ShowType != showTypeDJ:
		return "Pick a show type."
	case len(f.Slug) > maxShowSlugLen:
		return fmt.Sprintf("URL slug is too long (maximum is %d characters)", maxShowSlugLen)
	case f.Slug != "" && !handlePattern.MatchString(f.Slug):
		return "URL slug must be lowercase letters, numbers, and hyphens"
	}
	if _, msg := f.maxSongs(); msg != "" {
		return msg
	}
	if f.Slug != "" {
		var taken int64
		if err := a.db.Model(&Show{}).Where("user_id = ? AND slug = ? AND id <> ?", userID, f.Slug, showID).Count(&taken).Error; err != nil || taken > 0 {
			return "URL slug has already been taken"
		}
	}
	return ""
}

type newShowPage struct {
	Form       showForm
	Catalogs   []Catalog
	ShowTypes  []choice
	Error      string
	ActiveShow *Show // set when "start now" is refused
}

func (a *app) newShowPageFor(u *User, f showForm) (newShowPage, error) {
	page := newShowPage{Form: f, ShowTypes: showTypeChoices}
	err := a.db.Where("user_id = ?", u.ID).Order("name").Find(&page.Catalogs).Error
	return page, err
}

func (a *app) handleNewShow(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	page, err := a.newShowPageFor(u, showForm{ShowType: u.DefaultShowType, Approval: true, Timing: "now"})
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if page.ActiveShow, err = a.activeShow(u.ID); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, http.StatusOK, "show_new.html", page)
}

func (a *app) handleCreateShow(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	f := readShowForm(r)
	page, err := a.newShowPageFor(u, f)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	fail := func(msg string) {
		page.Error = msg
		a.render(w, r, http.StatusUnprocessableEntity, "show_new.html", page)
	}

	if msg := a.checkShowFields(f, u.ID, 0); msg != "" {
		fail(msg)
		return
	}
	var catalog *Catalog
	if f.CatalogID != "" {
		for i := range page.Catalogs {
			if strconv.FormatUint(uint64(page.Catalogs[i].ID), 10) == f.CatalogID {
				catalog = &page.Catalogs[i]
			}
		}
		if catalog == nil {
			fail("Pick one of your catalogs.")
			return
		}
	}
	s := Show{
		UserID: u.ID, Name: f.Name, Slug: f.Slug, ShowType: f.ShowType, RotationStyle: rotationStandard,
		ApprovalRequired: f.Approval, ManualEntryEnabled: catalog == nil || f.ManualEntry,
		Status: showActive, Catalog: catalog,
	}
	s.MaxSongsPerSinger, _ = f.maxSongs()
	if catalog != nil {
		s.CatalogID = &catalog.ID
	}
	now := a.now()
	if f.Timing == "later" {
		t, ok := f.scheduledTime()
		if !ok || t == nil {
			fail("Pick a date and time for the show.")
			return
		}
		s.Status, s.ScheduledAt = showScheduled, t
	} else {
		if page.ActiveShow, err = a.activeShow(u.ID); err != nil {
			a.serverError(w, r, err)
			return
		}
		if page.ActiveShow != nil {
			fail("You already have a live show. End it before starting another.")
			return
		}
		s.StartedAt = &now
	}
	if s.Slug == "" {
		if s.Slug, err = a.newShowSlug(u.ID, s.DisplayName()); err != nil {
			a.serverError(w, r, err)
			return
		}
	}
	if u.Slug == nil {
		handle, err := a.newHandle()
		if err == nil {
			err = a.db.Model(u).Update("slug", handle).Error
		}
		if err != nil {
			a.serverError(w, r, err)
			return
		}
	}
	if err := a.db.Create(&s).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	saved := "scheduled"
	if s.Active() {
		saved = "started"
		a.refreshPins()
	}
	http.Redirect(w, r, fmt.Sprintf("/shows/%d?saved=%s", s.ID, saved), http.StatusSeeOther)
}

// newShowSlug makes a slug from the show's name as Rails did: parameterized,
// at most 30 characters, with -1, -2… until it is unused.
func (a *app) newShowSlug(userID uint, name string) (string, error) {
	base := parameterize(name)
	if base == "" || name == "Untitled Show" {
		b := make([]byte, 4)
		if _, err := randRead(b); err != nil {
			return "", err
		}
		for i := range b {
			b[i] = handleChars[int(b[i])%len(handleChars)]
		}
		base = "show-" + string(b)
	}
	candidate := truncateSlug(base, 30)
	for n := 1; ; n++ {
		var taken int64
		if err := a.db.Model(&Show{}).Where("user_id = ? AND slug = ?", userID, candidate).Count(&taken).Error; err != nil {
			return "", err
		}
		if taken == 0 {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", truncateSlug(base, 26), n)
	}
}

func truncateSlug(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.Trim(s, "-")
}

// parameterize is Rails' String#parameterize: accents dropped, lowercase
// ASCII letters and digits, everything else one hyphen.
func parameterize(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range norm.NFKD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		r = unicode.ToLower(r)
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	return b.String()
}

type showPage struct {
	Show          Show
	Handle        string // the host's public handle, for the show's URL
	Saved         string
	Added         string // name of the singer just added
	Error         string
	ActiveShow    *Show // set when starting is refused
	Live          liveQueue
	Settings      showForm
	SettingsError string
	ShowTypes     []choice
	Add           addForm
	Search        catalogSearchView
}

func (a *app) showPageFor(r *http.Request, s Show) (showPage, error) {
	q := r.URL.Query()
	page := showPage{
		Show: s, Handle: currentUser(r).Handle(), Saved: q.Get("saved"), Added: q.Get("added"),
		Settings: settingsFormFor(s), ShowTypes: showTypeChoices,
	}
	var err error
	page.Live, err = a.loadQueue(s)
	if s.Catalog != nil {
		page.Search.CatalogID = s.Catalog.ID
		if query := q.Get("q"); strings.TrimSpace(query) != "" && s.Catalog.ActiveVersion != nil && s.Active() {
			page.Search = a.searchView(r, *s.Catalog, query)
		}
	}
	return page, err
}

func settingsFormFor(s Show) showForm {
	f := showForm{Name: s.Name, Slug: s.Slug, ShowType: s.ShowType, Approval: s.ApprovalRequired, ManualEntry: s.ManualEntryEnabled}
	if s.MaxSongsPerSinger != nil {
		f.MaxSongs = strconv.Itoa(*s.MaxSongsPerSinger)
	}
	if s.ScheduledAt != nil {
		f.ScheduledLocal = s.ScheduledAt.UTC().Format("2006-01-02T15:04")
		f.ScheduledAt = s.ScheduledAt.UTC().Format(time.RFC3339)
	}
	return f
}

func (a *app) renderShow(w http.ResponseWriter, r *http.Request, status int, s Show, edit func(*showPage)) {
	page, err := a.showPageFor(r, s)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if edit != nil {
		edit(&page)
	}
	a.render(w, r, status, "show.html", page)
}

func (a *app) handleShow(w http.ResponseWriter, r *http.Request) {
	s, ok := a.ownShow(w, r)
	if !ok {
		return
	}
	a.renderShow(w, r, http.StatusOK, s, nil)
}

func (a *app) handleShowSettings(w http.ResponseWriter, r *http.Request) {
	s, ok := a.ownShow(w, r)
	if !ok {
		return
	}
	if s.Ended() {
		a.renderShow(w, r, http.StatusConflict, s, func(p *showPage) { p.Error = "This show has ended." })
		return
	}
	f := readShowForm(r)
	fail := func(msg string) {
		a.renderShow(w, r, http.StatusUnprocessableEntity, s, func(p *showPage) { p.Settings, p.SettingsError = f, msg })
	}
	if f.Slug == "" {
		fail("URL slug can't be blank")
		return
	}
	if msg := a.checkShowFields(f, s.UserID, s.ID); msg != "" {
		fail(msg)
		return
	}
	updates := map[string]any{
		"name": f.Name, "slug": f.Slug, "show_type": f.ShowType, "approval_required": f.Approval,
		"manual_entry_enabled": s.CatalogID == nil || f.ManualEntry, "updated_at": a.now(),
	}
	updates["max_songs_per_singer"], _ = f.maxSongs()
	if s.Scheduled() {
		t, ok := f.scheduledTime()
		if !ok || t == nil {
			fail("Pick a date and time for the show.")
			return
		}
		updates["scheduled_at"] = t
	}
	if err := a.db.Model(&Show{}).Where("id = ?", s.ID).Updates(updates).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/shows/%d?saved=settings", s.ID), http.StatusSeeOther)
}

func (a *app) handleStartShow(w http.ResponseWriter, r *http.Request) {
	s, ok := a.ownShow(w, r)
	if !ok {
		return
	}
	if !s.Scheduled() {
		http.Redirect(w, r, fmt.Sprintf("/shows/%d", s.ID), http.StatusSeeOther)
		return
	}
	other, err := a.activeShow(s.UserID)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if other != nil {
		a.renderShow(w, r, http.StatusConflict, s, func(p *showPage) {
			p.Error, p.ActiveShow = "You already have a live show. End it before starting another.", other
		})
		return
	}
	now := a.now()
	res := a.db.Model(&Show{}).Where("id = ? AND status = ?", s.ID, showScheduled).
		Updates(map[string]any{"status": showActive, "started_at": now, "updated_at": now})
	if res.Error != nil {
		a.serverError(w, r, res.Error)
		return
	}
	a.refreshPins()
	http.Redirect(w, r, fmt.Sprintf("/shows/%d?saved=started", s.ID), http.StatusSeeOther)
}

// handleEndShow closes the queue: the song on stage counts as performed, and
// anything still waiting is canceled. Ended is final.
func (a *app) handleEndShow(w http.ResponseWriter, r *http.Request) {
	s, ok := a.ownShow(w, r)
	if !ok {
		return
	}
	if !s.Active() {
		http.Redirect(w, r, fmt.Sprintf("/shows/%d", s.ID), http.StatusSeeOther)
		return
	}
	now := a.now()
	err := a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&SongRequest{}).Where("show_id = ? AND status = ?", s.ID, requestNowPlaying).
			Updates(map[string]any{"status": requestCompleted, "completed_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&SongRequest{}).Where("show_id = ? AND status IN ?", s.ID, []string{requestPending, requestQueued}).
			Updates(map[string]any{"status": requestCanceled, "canceled_at": now, "queue_position": nil}).Error; err != nil {
			return err
		}
		return tx.Model(&Show{}).Where("id = ?", s.ID).
			Updates(map[string]any{"status": showEnded, "ended_at": now, "updated_at": now}).Error
	})
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.refreshPins()
	http.Redirect(w, r, "/shows?saved=ended", http.StatusSeeOther)
}
