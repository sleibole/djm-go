package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxFeedbackLen   = 5000
	feedbackLimit    = 5
	feedbackWindow   = time.Hour
	maxAdminNotesLen = 5000
)

type feedbackForm struct {
	Categories []choice
	Category   string
	Message    string
	PageURL    string // the page they came from, as a path
	Error      string
}

// samePagePath is u as a local path if it points into this site (the request's
// host or BASE_URL's), else "".
func (a *app) samePagePath(u string, r *http.Request) string {
	p, err := url.Parse(u)
	if err != nil {
		return ""
	}
	if p.Host != "" && p.Host != r.Host {
		if base, err := url.Parse(a.cfg.BaseURL); err != nil || p.Host != base.Host {
			return ""
		}
	}
	return safeNext(p.RequestURI())
}

func (a *app) handleFeedbackForm(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, http.StatusOK, "feedback.html", feedbackForm{
		Categories: feedbackCategories, PageURL: a.samePagePath(r.Referer(), r),
	})
}

func (a *app) handleFeedback(w http.ResponseWriter, r *http.Request) {
	form := feedbackForm{
		Categories: feedbackCategories,
		Category:   r.PostFormValue("category"),
		Message:    strings.TrimSpace(r.PostFormValue("message")),
		PageURL:    a.samePagePath(r.PostFormValue("page_url"), r),
	}
	var problems []string
	if choiceLabel(feedbackCategories, form.Category) == "" {
		problems = append(problems, "Category can't be blank")
	}
	if form.Message == "" {
		problems = append(problems, "Message can't be blank")
	} else if utf8.RuneCountInString(form.Message) > maxFeedbackLen {
		problems = append(problems, fmt.Sprintf("Message is too long (maximum is %d characters)", maxFeedbackLen))
	}
	if len(problems) > 0 {
		form.Error = "Could not submit feedback: " + strings.Join(problems, ", ")
		a.render(w, r, http.StatusUnprocessableEntity, "feedback.html", form)
		return
	}
	if !a.feedbackLimiter.allow(a.clientIP(r)) {
		form.Error = "Too much feedback from this network. Please try again later."
		a.render(w, r, http.StatusTooManyRequests, "feedback.html", form)
		return
	}
	fb := Feedback{Category: form.Category, Message: form.Message, PageURL: form.PageURL}
	if u := currentUser(r); u != nil {
		fb.UserID = &u.ID
	}
	if err := a.db.Create(&fb).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	page := messagePage{Title: "Thanks for your feedback!", Text: "We read every message."}
	if form.PageURL != "" && form.PageURL != "/" {
		page.LinkURL, page.LinkText = form.PageURL, "Back to where you were"
	}
	a.render(w, r, http.StatusOK, "message.html", page)
}

// feedbackRow is feedback with the sender's email, for the admin pages.
type feedbackRow struct {
	Feedback
	Email string
}

type adminFeedbackPage struct {
	Statuses []choice
	Status   string // the filter, "" for all
	Items    []feedbackRow
}

func (a *app) handleAdminFeedback(w http.ResponseWriter, r *http.Request) {
	page := adminFeedbackPage{Statuses: feedbackStatuses, Status: r.URL.Query().Get("status")}
	q := a.db.Table("feedback").Select("feedback.*, COALESCE(users.email, '') AS email").
		Joins("LEFT JOIN users ON users.id = feedback.user_id").Order("feedback.created_at DESC, feedback.id DESC")
	if choiceLabel(feedbackStatuses, page.Status) != "" {
		q = q.Where("feedback.status = ?", page.Status)
	} else {
		page.Status = ""
	}
	if err := q.Scan(&page.Items).Error; err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, http.StatusOK, "admin_feedback.html", page)
}

type adminFeedbackItemPage struct {
	Item     feedbackRow
	Statuses []choice
	Saved    bool
	Error    string
}

func (a *app) adminFeedbackItem(w http.ResponseWriter, r *http.Request) (feedbackRow, bool) {
	var found []feedbackRow
	err := a.db.Table("feedback").Select("feedback.*, COALESCE(users.email, '') AS email").
		Joins("LEFT JOIN users ON users.id = feedback.user_id").
		Where("feedback.id = ?", r.PathValue("id")).Limit(1).Scan(&found).Error
	if err != nil {
		a.serverError(w, r, err)
		return feedbackRow{}, false
	}
	if len(found) == 0 {
		a.notFound(w, r)
		return feedbackRow{}, false
	}
	return found[0], true
}

func (a *app) handleAdminFeedbackItem(w http.ResponseWriter, r *http.Request) {
	fb, ok := a.adminFeedbackItem(w, r)
	if !ok {
		return
	}
	a.render(w, r, http.StatusOK, "admin_feedback_item.html", adminFeedbackItemPage{
		Item: fb, Statuses: feedbackStatuses, Saved: r.URL.Query().Get("saved") != "",
	})
}

func (a *app) handleAdminFeedbackUpdate(w http.ResponseWriter, r *http.Request) {
	fb, ok := a.adminFeedbackItem(w, r)
	if !ok {
		return
	}
	status := r.PostFormValue("status")
	notes := strings.TrimSpace(r.PostFormValue("admin_notes"))
	fail := func(msg string) {
		fb.Status, fb.AdminNotes = status, notes
		a.render(w, r, http.StatusUnprocessableEntity, "admin_feedback_item.html", adminFeedbackItemPage{
			Item: fb, Statuses: feedbackStatuses, Error: msg,
		})
	}
	if choiceLabel(feedbackStatuses, status) == "" {
		fail("Status is not included in the list")
		return
	}
	if utf8.RuneCountInString(notes) > maxAdminNotesLen {
		fail(fmt.Sprintf("Admin notes are too long (maximum is %d characters)", maxAdminNotesLen))
		return
	}
	err := a.db.Model(&Feedback{ID: fb.ID}).Updates(map[string]any{"status": status, "admin_notes": notes}).Error
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/feedback/%d?saved=1", fb.ID), http.StatusSeeOther)
}
