package main

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

const (
	minHandleLen      = 3
	maxHandleLen      = 30
	maxDisplayNameLen = 50
	maxSongsLimit     = 99
)

var handlePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// accountPage carries messages and, after a failed save, what was typed.
// Fields left empty fall back to the saved values in the template.
type accountPage struct {
	Saved         string // which section just saved: "password", "password-removed", "profile", "defaults", "hosting"
	PasswordError string
	ProfileError  string
	DefaultsError string
	DeleteError   string
	Handle        string
	DisplayName   string
	ShowType      string
	MaxSongs      string
	ShowTypes     []choice
}

type choice struct {
	Value string
	Label string
}

// choiceLabel is the label for value, or "" if value isn't one of the choices.
func choiceLabel(choices []choice, value string) string {
	for _, c := range choices {
		if c.Value == value {
			return c.Label
		}
	}
	return ""
}

var showTypeChoices = []choice{{showTypeKaraoke, "Karaoke"}, {showTypeDJ, "DJ"}}

func (a *app) accountPageFor(r *http.Request) accountPage {
	u := currentUser(r)
	p := accountPage{
		Saved:       r.URL.Query().Get("saved"),
		Handle:      u.Handle(),
		DisplayName: u.DisplayName,
		ShowType:    u.DefaultShowType,
		ShowTypes:   showTypeChoices,
	}
	if u.DefaultMaxSongsPerSinger != nil {
		p.MaxSongs = strconv.Itoa(*u.DefaultMaxSongsPerSinger)
	}
	return p
}

func (a *app) handleAccount(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "account.html", a.accountPageFor(r))
}

// handleAccountPassword sets or changes the password. It doesn't ask for the current
// one, because "forgot password" is: sign in with an email link, then set a new one here.
// Every other session is logged out, so a changed password locks out anyone else.
func (a *app) handleAccountPassword(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	password := r.PostFormValue("password")
	msg := validatePassword(password)
	if msg == "" && password != r.PostFormValue("confirm") {
		msg = "The two passwords don't match."
	}
	if msg != "" {
		page := a.accountPageFor(r)
		page.PasswordError = msg
		a.render(w, r, http.StatusUnprocessableEntity, "account.html", page)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	err = a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(u).Updates(map[string]any{"password_hash": string(hash), "failed_login_attempts": 0, "locked_at": nil}).Error; err != nil {
			return err
		}
		return tx.Where("user_id = ? AND id <> ?", u.ID, currentSessionID(r)).Delete(&Session{}).Error
	})
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?saved=password", http.StatusSeeOther)
}

func (a *app) handleAccountPasswordRemove(w http.ResponseWriter, r *http.Request) {
	if err := a.db.Model(currentUser(r)).Update("password_hash", "").Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?saved=password-removed", http.StatusSeeOther)
}

func validateHandle(handle string) string {
	switch {
	case len(handle) < minHandleLen || len(handle) > maxHandleLen:
		return "Handle must be 3 to 30 characters."
	case !handlePattern.MatchString(handle):
		return "Handle must be lowercase letters, numbers, and hyphens (can't start or end with a hyphen)."
	}
	return ""
}

func (a *app) handleAccountProfile(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !u.Hosts() {
		a.notFound(w, r)
		return
	}
	page := a.accountPageFor(r)
	page.Handle = strings.ToLower(strings.TrimSpace(r.PostFormValue("handle")))
	page.DisplayName = strings.TrimSpace(r.PostFormValue("display_name"))
	fail := func(msg string) {
		page.ProfileError = msg
		a.render(w, r, http.StatusUnprocessableEntity, "account.html", page)
	}

	if msg := validateHandle(page.Handle); msg != "" {
		fail(msg)
		return
	}
	if utf8.RuneCountInString(page.DisplayName) > maxDisplayNameLen {
		fail("Display name must be at most 50 characters.")
		return
	}
	var taken int64
	if err := a.db.Model(&User{}).Where("slug = ? AND id <> ?", page.Handle, u.ID).Count(&taken).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	if taken > 0 {
		fail("That handle is taken.")
		return
	}
	if err := a.db.Model(u).Updates(map[string]any{"slug": page.Handle, "display_name": page.DisplayName}).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?saved=profile", http.StatusSeeOther)
}

func (a *app) handleAccountDefaults(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if !u.Hosts() {
		a.notFound(w, r)
		return
	}
	page := a.accountPageFor(r)
	page.ShowType = r.PostFormValue("show_type")
	page.MaxSongs = strings.TrimSpace(r.PostFormValue("max_songs"))

	if page.ShowType != showTypeKaraoke && page.ShowType != showTypeDJ {
		page.DefaultsError = "Pick a show type."
		a.render(w, r, http.StatusUnprocessableEntity, "account.html", page)
		return
	}
	var maxSongs *int
	if page.MaxSongs != "" {
		n, err := strconv.Atoi(page.MaxSongs)
		if err != nil || n < 1 || n > maxSongsLimit {
			page.DefaultsError = "Songs per singer must be a number from 1 to 99, or blank for no limit."
			a.render(w, r, http.StatusUnprocessableEntity, "account.html", page)
			return
		}
		maxSongs = &n
	}
	err := a.db.Model(u).Updates(map[string]any{
		"default_show_type":            page.ShowType,
		"default_rotation_style":       rotationStandard,
		"default_max_songs_per_singer": maxSongs,
	}).Error
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?saved=defaults", http.StatusSeeOther)
}

// handleAccountHost turns an audience account into a DJ account with a generated handle.
func (a *app) handleAccountHost(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role != roleAudience {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	updates := map[string]any{"role": roleDJ}
	if u.Slug == nil {
		handle, err := a.newHandle()
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		updates["slug"] = handle
	}
	if err := a.db.Model(u).Updates(updates).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/account?saved=hosting", http.StatusSeeOther)
}

func (a *app) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	fail := func(msg string) {
		page := a.accountPageFor(r)
		page.DeleteError = msg
		a.render(w, r, http.StatusUnprocessableEntity, "account.html", page)
	}
	if normalizeEmail(r.PostFormValue("confirm")) != u.Email {
		fail("Type your email address exactly to confirm.")
		return
	}
	if u.Role == roleAdmin {
		var admins int64
		if err := a.db.Model(&User{}).Where("role = ?", roleAdmin).Count(&admins).Error; err != nil {
			a.serverError(w, r, err)
			return
		}
		if admins <= 1 {
			fail("Cannot delete the only admin account.")
			return
		}
	}
	live, err := a.activeShow(u.ID)
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if live != nil {
		fail("End all active shows before deleting your account.")
		return
	}
	if err := a.deleteUser(u.ID); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.logger.Info("account deleted", "user_id", u.ID)
	a.clearSessionCookie(w)
	r = r.WithContext(context.WithValue(r.Context(), userKey, (*User)(nil)))
	a.message(w, r, http.StatusOK, "Account deleted", "Your account and everything in it have been deleted.")
}

// deleteUser removes the user. Rows that belong to them go with it (ON DELETE
// CASCADE); their catalog files are removed after.
func (a *app) deleteUser(id uint) error {
	var catalogs []uint
	if err := a.db.Model(&Catalog{}).Where("user_id = ?", id).Pluck("id", &catalogs).Error; err != nil {
		return err
	}
	// Their saved songs go with the profile; their singers keep their names in show history.
	if err := a.db.Where("user_id = ?", id).Delete(&AudienceProfile{}).Error; err != nil {
		return err
	}
	res := a.db.Delete(&User{}, id)
	if res.Error == nil && res.RowsAffected == 0 {
		return errors.New("user not found")
	}
	if res.Error != nil {
		return res.Error
	}
	for _, c := range catalogs {
		a.removeCatalogData(context.Background(), c)
	}
	return nil
}
