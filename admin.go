package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const adminUsersPageSize = 50

var roleChoices = []choice{{roleAudience, "Audience"}, {roleDJ, "DJ"}, {roleAdmin, "Admin"}}

// requireAdmin answers 404 to everyone but admins, so the pages don't advertise themselves.
func (a *app) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return a.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r).Role != roleAdmin {
			a.notFound(w, r)
			return
		}
		next(w, r)
	})
}

type adminHome struct {
	Roles         []roleCount
	LiveShows     int
	RequestsToday int
	NewFeedback   int
	CacheFiles    int
	CacheBytes    int64
	CacheMaxBytes int64
	CacheHits     int
	CacheMisses   int
}

type roleCount struct {
	Role  string
	Count int
}

func (r roleCount) Label() string { return choiceLabel(roleChoices, r.Role) }

func (a *app) handleAdmin(w http.ResponseWriter, r *http.Request) {
	cs := a.cache.stats()
	page := adminHome{
		CacheFiles: int(cs.Files), CacheBytes: cs.Bytes, CacheMaxBytes: cs.MaxBytes,
		CacheHits: int(cs.Hits), CacheMisses: int(cs.Misses),
	}
	now := a.now()
	utc := now.UTC()
	// Times are stored as text in the clock's zone, so compare in that zone.
	today := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).In(now.Location())
	err := a.db.Model(&User{}).Select("role, COUNT(*) AS count").Group("role").Order("role").Scan(&page.Roles).Error
	if err == nil {
		err = a.db.Raw(`SELECT COUNT(*) FROM shows WHERE status = ?`, showActive).Scan(&page.LiveShows).Error
	}
	if err == nil {
		err = a.db.Raw(`SELECT COUNT(*) FROM song_requests WHERE requested_at >= ?`, today).Scan(&page.RequestsToday).Error
	}
	if err == nil {
		err = a.db.Raw(`SELECT COUNT(*) FROM feedback WHERE status = 'new'`).Scan(&page.NewFeedback).Error
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, http.StatusOK, "admin.html", page)
}

type adminUsersPage struct {
	Query string
	Users []User
	Page  int
	More  bool
}

func (p adminUsersPage) pageURL(n int) string {
	return fmt.Sprintf("/admin/users?q=%s&page=%d", url.QueryEscape(p.Query), n)
}

func (p adminUsersPage) PrevURL() string { return p.pageURL(p.Page - 1) }
func (p adminUsersPage) NextURL() string { return p.pageURL(p.Page + 1) }

func (u User) RoleLabel() string { return choiceLabel(roleChoices, u.Role) }

func (a *app) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	page := adminUsersPage{Query: strings.TrimSpace(r.URL.Query().Get("q"))}
	page.Page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	page.Page = max(page.Page, 1)
	q := a.db.Order("created_at DESC, id DESC")
	if page.Query != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(strings.ToLower(page.Query)) + "%"
		q = q.Where(`email LIKE ? ESCAPE '\' OR slug LIKE ? ESCAPE '\' OR lower(display_name) LIKE ? ESCAPE '\'`, like, like, like)
	}
	if err := q.Offset((page.Page - 1) * adminUsersPageSize).Limit(adminUsersPageSize + 1).Find(&page.Users).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	if len(page.Users) > adminUsersPageSize {
		page.Users, page.More = page.Users[:adminUsersPageSize], true
	}
	a.render(w, r, http.StatusOK, "admin_users.html", page)
}

type adminUserPage struct {
	Target   User
	Shows    int
	Catalogs int
	Roles    []choice
	Saved    string
	Error    string
}

func (a *app) adminUser(w http.ResponseWriter, r *http.Request) (User, bool) {
	var found []User
	if err := a.db.Where("id = ?", r.PathValue("id")).Limit(1).Find(&found).Error; err != nil {
		a.serverError(w, r, err)
		return User{}, false
	}
	if len(found) == 0 {
		a.notFound(w, r)
		return User{}, false
	}
	return found[0], true
}

func (a *app) renderAdminUser(w http.ResponseWriter, r *http.Request, status int, u User, saved, errMsg string) {
	page := adminUserPage{Target: u, Roles: roleChoices, Saved: saved, Error: errMsg}
	err := a.db.Raw(`SELECT COUNT(*) FROM shows WHERE user_id = ?`, u.ID).Scan(&page.Shows).Error
	if err == nil {
		err = a.db.Raw(`SELECT COUNT(*) FROM catalogs WHERE user_id = ?`, u.ID).Scan(&page.Catalogs).Error
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, status, "admin_user.html", page)
}

func (a *app) handleAdminUser(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminUser(w, r)
	if !ok {
		return
	}
	a.renderAdminUser(w, r, http.StatusOK, u, r.URL.Query().Get("saved"), "")
}

// handleAdminUserRole changes a user's role, with Rails' rules.
func (a *app) handleAdminUserRole(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminUser(w, r)
	if !ok {
		return
	}
	role := r.PostFormValue("role")
	fail := func(msg string) { a.renderAdminUser(w, r, http.StatusUnprocessableEntity, u, "", msg) }
	switch {
	case choiceLabel(roleChoices, role) == "":
		fail("Could not update role.")
		return
	case role == u.Role:
		http.Redirect(w, r, fmt.Sprintf("/admin/users/%d?saved=unchanged", u.ID), http.StatusSeeOther)
		return
	case u.ID == currentUser(r).ID:
		fail("You cannot change your own role.")
		return
	}
	if u.Role == roleAdmin {
		var admins int64
		if err := a.db.Model(&User{}).Where("role = ?", roleAdmin).Count(&admins).Error; err != nil {
			a.serverError(w, r, err)
			return
		}
		if admins <= 1 {
			fail("Cannot remove the last admin.")
			return
		}
	}
	updates := map[string]any{"role": role}
	if role != roleAudience && u.Slug == nil {
		handle, err := a.newHandle()
		if err != nil {
			a.serverError(w, r, err)
			return
		}
		updates["slug"] = handle
	}
	if err := a.db.Model(&u).Updates(updates).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	a.logger.Info("role changed", "admin_id", currentUser(r).ID, "user_id", u.ID, "role", role)
	http.Redirect(w, r, fmt.Sprintf("/admin/users/%d?saved=role", u.ID), http.StatusSeeOther)
}

// handleImpersonate signs the admin in as another user, with Rails' rules.
// "Return to admin" (handleStopImpersonating) switches back.
func (a *app) handleImpersonate(w http.ResponseWriter, r *http.Request) {
	u, ok := a.adminUser(w, r)
	if !ok {
		return
	}
	admin := currentUser(r)
	switch {
	case u.LockedAt != nil:
		a.renderAdminUser(w, r, http.StatusConflict, u, "", "Cannot sign in as a locked account.")
		return
	case u.Role == roleAdmin && u.ID != admin.ID:
		a.renderAdminUser(w, r, http.StatusConflict, u, "", "Cannot sign in as another admin.")
		return
	}
	if err := a.startSessionAs(w, r, u.ID, &admin.ID); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.logger.Info("impersonation started", "admin_id", admin.ID, "user_id", u.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) handleStopImpersonating(w http.ResponseWriter, r *http.Request) {
	adminID := impersonatorID(r)
	if adminID == 0 {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var admin User
	if err := a.db.Take(&admin, adminID).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	userID := currentUser(r).ID
	if admin.Role != roleAdmin {
		// No longer an admin: end the session instead of handing back admin access.
		a.handleLogout(w, r)
		return
	}
	if err := a.startSession(w, r, admin.ID); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.logger.Info("impersonation ended", "admin_id", admin.ID, "user_id", userID)
	http.Redirect(w, r, fmt.Sprintf("/admin/users/%d", userID), http.StatusSeeOther)
}

// humanBytes is a byte count for people: 1.5 GB.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
