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

func signInAdmin(t *testing.T, a *app, email string) (*browser, User) {
	t.Helper()
	b := newBrowser(t, a)
	u := signIn(t, a, b, email, roleDJ)
	a.db.Model(&u).Update("role", roleAdmin)
	u.Role = roleAdmin
	return b, u
}

func TestAdminPagesAreHidden(t *testing.T) {
	a := newTestApp(t)
	dj, _ := signInHost(t, a, "dj@example.com")
	for _, path := range []string{"/admin", "/admin/users", "/admin/users/1", "/admin/feedback"} {
		if rec := dj.get(path); rec.Code != http.StatusNotFound {
			t.Errorf("%s for a DJ: %d", path, rec.Code)
		}
	}
	if rec := dj.post("/admin/users/1/role", url.Values{"role": {roleAdmin}}); rec.Code != http.StatusNotFound {
		t.Errorf("role change by a DJ: %d", rec.Code)
	}
	if strings.Contains(dj.get("/").Body.String(), `href="/admin"`) {
		t.Error("a DJ should not see the admin link")
	}
}

func TestAdminUsersAndRoles(t *testing.T) {
	a := newTestApp(t)
	admin, me := signInAdmin(t, a, "admin@example.com")
	fan := signIn(t, a, newBrowser(t, a), "fan@example.com", roleAudience)

	mustContain(t, "admin home", admin.get("/admin").Body.String(), "Catalog cache", "Audience: <strong>1</strong>", "Admin: <strong>1</strong>")
	mustContain(t, "layout", admin.get("/").Body.String(), `href="/admin"`)
	body := admin.get("/admin/users?q=FAN").Body.String()
	mustContain(t, "user search", body, "fan@example.com", fmt.Sprintf("/admin/users/%d", fan.ID))
	if strings.Contains(body, "admin@example.com") {
		t.Error("the search should filter")
	}
	if body := admin.get("/admin/users?q=%25").Body.String(); strings.Contains(body, "fan@example.com") {
		t.Error("% should match literally")
	}

	rec := admin.post(fmt.Sprintf("/admin/users/%d/role", fan.ID), url.Values{"role": {roleDJ}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != fmt.Sprintf("/admin/users/%d?saved=role", fan.ID) {
		t.Fatalf("role change: %d %v", rec.Code, rec.Header())
	}
	a.db.First(&fan, fan.ID)
	if fan.Role != roleDJ || fan.Slug == nil {
		t.Fatalf("a new DJ needs a handle: %+v", fan)
	}
	mustContain(t, "user page", admin.get(fmt.Sprintf("/admin/users/%d?saved=role", fan.ID)).Body.String(), "Role updated.", "Log in as fan@example.com")

	if rec := admin.post(fmt.Sprintf("/admin/users/%d/role", fan.ID), url.Values{"role": {"wizard"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad role: %d", rec.Code)
	}
	rec = admin.post(fmt.Sprintf("/admin/users/%d/role", me.ID), url.Values{"role": {roleAudience}})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "You cannot change your own role.") {
		t.Errorf("own role: %d", rec.Code)
	}

	// With two admins, one can demote the other.
	a.db.Model(&fan).Update("role", roleAdmin)
	if rec := admin.post(fmt.Sprintf("/admin/users/%d/role", fan.ID), url.Values{"role": {roleAudience}}); rec.Code != http.StatusSeeOther {
		t.Errorf("demote another admin: %d\n%s", rec.Code, rec.Body)
	}
}

func TestImpersonation(t *testing.T) {
	a := newTestApp(t)
	admin, _ := signInAdmin(t, a, "admin@example.com")
	fan := signIn(t, a, newBrowser(t, a), "fan@example.com", roleAudience)
	signInAdmin(t, a, "other-admin@example.com")

	var otherAdmin User
	a.db.Where("email = ?", "other-admin@example.com").First(&otherAdmin)
	rec := admin.post(fmt.Sprintf("/admin/users/%d/impersonate", otherAdmin.ID), nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Cannot sign in as another admin.") {
		t.Fatalf("impersonate admin: %d", rec.Code)
	}
	locked := signIn(t, a, newBrowser(t, a), "locked@example.com", roleAudience)
	a.db.Model(&locked).Update("locked_at", time.Now())
	if rec := admin.post(fmt.Sprintf("/admin/users/%d/impersonate", locked.ID), nil); rec.Code != http.StatusConflict {
		t.Fatalf("impersonate locked: %d", rec.Code)
	}

	rec = admin.post(fmt.Sprintf("/admin/users/%d/impersonate", fan.ID), nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("impersonate: %d %v", rec.Code, rec.Header())
	}
	body := admin.get("/").Body.String()
	mustContain(t, "as fan", body, "Signed in as fan@example.com", "Logged in as <strong>fan@example.com</strong>", `action="/admin/return"`)
	if admin.get("/admin").Code != http.StatusNotFound {
		t.Error("while logged in as a fan, admin pages are hidden")
	}

	rec = admin.post("/admin/return", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != fmt.Sprintf("/admin/users/%d", fan.ID) {
		t.Fatalf("return: %d %v", rec.Code, rec.Header())
	}
	if body := admin.get("/admin").Body.String(); !strings.Contains(body, "Catalog cache") || strings.Contains(body, "Return to admin") {
		t.Fatal("back as the admin, without the banner")
	}
	if rec := admin.post("/admin/return", nil); rec.Header().Get("Location") != "/" {
		t.Errorf("return when not impersonating: %v", rec.Header())
	}

	// A "log in as" session lasts an hour and isn't renewed.
	admin.post(fmt.Sprintf("/admin/users/%d/impersonate", fan.ID), nil)
	start := time.Now()
	a.now = func() time.Time { return start.Add(30 * time.Minute) }
	if !strings.Contains(admin.get("/").Body.String(), "Signed in as fan@example.com") {
		t.Fatal("still signed in after half an hour")
	}
	a.now = func() time.Time { return start.Add(impersonationLifetime + time.Minute) }
	if rec := admin.get("/account"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/signin") {
		t.Fatalf("after an hour: %d %v", rec.Code, rec.Header())
	}
}

func TestReturnToAdminAfterDemotionSignsOut(t *testing.T) {
	a := newTestApp(t)
	admin, me := signInAdmin(t, a, "admin@example.com")
	fan := signIn(t, a, newBrowser(t, a), "fan@example.com", roleAudience)
	admin.post(fmt.Sprintf("/admin/users/%d/impersonate", fan.ID), nil)
	a.db.Model(&me).Update("role", roleDJ)
	admin.post("/admin/return", nil)
	if rec := admin.get("/account"); rec.Code != http.StatusSeeOther {
		t.Fatalf("a demoted admin's return should sign out: %d", rec.Code)
	}
}

func feedbackGet(b *browser, referer string) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/feedback", nil)
	req.Header.Set("Referer", referer)
	return b.do(req)
}

func TestFeedback(t *testing.T) {
	a := newTestApp(t)
	g := guest(t, a, "192.0.2.10")
	mustContain(t, "footer", g.get("/").Body.String(), `href="/feedback"`)
	mustContain(t, "form", feedbackGet(g, "http://example.test/dj/abc?x=1").Body.String(), `name="page_url" value="/dj/abc?x=1"`, "Bug", "Feature Request")
	mustContain(t, "foreign referer", feedbackGet(g, "https://evil.example/x").Body.String(), `name="page_url" value=""`)

	rec := g.post("/feedback", url.Values{"message": {" "}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank: %d", rec.Code)
	}
	mustContain(t, "blank", rec.Body.String(), "Could not submit feedback: Category can&#39;t be blank, Message can&#39;t be blank")
	rec = g.post("/feedback", url.Values{"category": {"bug"}, "message": {strings.Repeat("a", maxFeedbackLen+1)}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("too long: %d", rec.Code)
	}

	rec = g.post("/feedback", url.Values{"category": {"bug"}, "message": {"It broke"}, "page_url": {"/dj/abc"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("send: %d\n%s", rec.Code, rec.Body)
	}
	mustContain(t, "thanks", rec.Body.String(), "Thanks for your feedback!", `href="/dj/abc"`)
	for i := 1; i < feedbackLimit; i++ {
		g.post("/feedback", url.Values{"category": {"other"}, "message": {fmt.Sprint("more ", i)}})
	}
	if rec := g.post("/feedback", url.Values{"category": {"other"}, "message": {"one too many"}}); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit: %d", rec.Code)
	}

	fan := newBrowser(t, a)
	fan.ip = "192.0.2.11"
	u := signIn(t, a, fan, "fan@example.com", roleAudience)
	fan.post("/feedback", url.Values{"category": {"feature_request"}, "message": {"Dark mode please"}})
	var fb Feedback
	a.db.Where("message = ?", "Dark mode please").First(&fb)
	if fb.UserID == nil || *fb.UserID != u.ID || fb.Status != "new" {
		t.Fatalf("signed-in feedback: %+v", fb)
	}

	admin, _ := signInAdmin(t, a, "admin@example.com")
	mustContain(t, "admin home", admin.get("/admin").Body.String(), "New feedback</a>: <strong>6</strong>")
	body := admin.get("/admin/feedback?status=new").Body.String()
	mustContain(t, "list", body, "It broke", "Dark mode please", "fan@example.com", "Guest")
	rec = admin.post(fmt.Sprintf("/admin/feedback/%d", fb.ID), url.Values{"status": {"resolved"}, "admin_notes": {"Shipped"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update: %d\n%s", rec.Code, rec.Body)
	}
	mustContain(t, "detail", admin.get(rec.Header().Get("Location")).Body.String(), "Feedback updated.", "Shipped", `value="resolved" selected`)
	if strings.Contains(admin.get("/admin/feedback?status=new").Body.String(), "Dark mode please") {
		t.Error("resolved feedback should leave the new filter")
	}
	if rec := admin.post(fmt.Sprintf("/admin/feedback/%d", fb.ID), url.Values{"status": {"lost"}}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad status: %d", rec.Code)
	}
	if rec := admin.get("/admin/feedback/99999"); rec.Code != http.StatusNotFound {
		t.Errorf("missing feedback: %d", rec.Code)
	}
}
