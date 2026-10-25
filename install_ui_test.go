package main

import (
	"encoding/json"
	"net/url"
	"os/exec"
	"strings"
	"testing"
)

func TestInstallEntry(t *testing.T) {
	a := newTestApp(t)
	h := a.routes()
	body := get(t, h, "/").Body.String()
	mustContain(t, "install footer", body,
		`<button type="button" class="install-app" data-open-dialog="install-app-dialog">Install app</button>`,
		`id="install-app-dialog"`,
		`aria-labelledby="install-app-heading"`,
		`aria-describedby="install-app-summary"`,
		`Install DJMagic.io`,
		`Add DJMagic.io to your Home Screen or install it for convenient access. It opens in its own window.`,
		`Add to Home Screen`,
		`Open as Web App`,
		`Having trouble? Open this page in Safari and follow these steps.`,
		`data-install-copy`,
		`Add to Home screen`,
		`install icon in the address bar`,
		`Not every browser can install this site.`,
		`data-install-native hidden`,
		`data-install-prompt`,
		`Other ways to install`,
		`window.djmInstallEarly`,
		`/static/install.js?v=`,
		`images/brand/dark_192.png?v=`,
	)
	if strings.Contains(body, `data-install-ui="off"`) {
		t.Error("ordinary pages should keep the install dialog")
	}
	if strings.Contains(body, "serviceWorker") {
		t.Error("the install dialog must not register a service worker")
	}
	lower := strings.ToLower(body)
	for _, word := range []string{"offline", "notification", "synchron"} {
		if strings.Contains(lower, word) {
			t.Errorf("install copy mentions %q", word)
		}
	}
	for _, mode := range []string{"standalone", "fullscreen", "minimal-ui", "window-controls-overlay", "tabbed"} {
		if !strings.Contains(body, mode) {
			t.Errorf("early install script missing display mode %s", mode)
		}
	}

	script := get(t, h, "/static/install.js")
	if script.Code != 200 {
		t.Fatalf("install.js: %d", script.Code)
	}
	js := script.Body.String()
	for _, mode := range []string{"standalone", "fullscreen", "minimal-ui", "window-controls-overlay", "tabbed"} {
		if !strings.Contains(js, mode) {
			t.Errorf("install.js missing display mode %s", mode)
		}
	}

	fragment := hxGet(guest(t, a, "192.0.2.40"), "/", "content")
	if strings.Contains(fragment.Body.String(), "install-app-dialog") {
		t.Error("a content fragment should not include the install dialog")
	}

	_, _, path := audienceShow(t, a, url.Values{"name": {"Friday Night"}})
	display := guest(t, a, "192.0.2.41").get(path + "/display").Body.String()
	if strings.Contains(display, `id="install-app-dialog"`) || strings.Contains(display, `class="install-app"`) {
		t.Error("the venue display should not grow an install footer")
	}
	if !strings.Contains(display, `data-install-ui="off"`) {
		t.Error("the venue display should leave the browser install prompt alone")
	}
}

func TestWebManifestInstallable(t *testing.T) {
	a := newTestApp(t)
	h := a.routes()
	rec := get(t, h, "/static/manifest.webmanifest")
	if rec.Code != 200 {
		t.Fatalf("manifest: %d", rec.Code)
	}
	var manifest struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		StartURL  string `json:"start_url"`
		Scope     string `json:"scope"`
		Display   string `json:"display"`
		Icons     []struct {
			Src   string `json:"src"`
			Sizes string `json:"sizes"`
			Type  string `json:"type"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "DJMagic.io" || manifest.ShortName != "DJMagic" {
		t.Fatalf("name = %q / %q", manifest.Name, manifest.ShortName)
	}
	if manifest.StartURL != "/" || manifest.Scope != "/" || manifest.Display != "standalone" {
		t.Fatalf("identity start=%q scope=%q display=%q", manifest.StartURL, manifest.Scope, manifest.Display)
	}
	if manifest.ID != "" && manifest.ID != "/" {
		t.Fatalf("manifest id = %q", manifest.ID)
	}
	if len(manifest.Icons) == 0 || manifest.Icons[0].Src != "/static/images/brand/dark_192.png" || manifest.Icons[0].Sizes != "192x192" {
		t.Fatalf("icons = %+v", manifest.Icons)
	}
	if icon := get(t, h, manifest.Icons[0].Src); icon.Code != 200 || !strings.Contains(icon.Header().Get("Content-Type"), "image/png") {
		t.Fatalf("icon: %d %q", icon.Code, icon.Header().Get("Content-Type"))
	}
}

func TestInstallScript(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node is required to run testdata/install_test.js")
	}
	out, err := exec.Command("node", "--test", "testdata/install_test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("install.js tests failed: %v\n%s", err, out)
	}
}
