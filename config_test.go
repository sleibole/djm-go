package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	prod := config{Env: "prod", BaseURL: "https://next.djmagic.io", baseURLSet: true, TurnstileSiteKey: "site", TurnstileSecretKey: "secret"}
	tests := []struct {
		name    string
		cfg     config
		wantErr string
	}{
		{"dev defaults", config{Env: "dev", BaseURL: "http://localhost:8080"}, ""},
		{"prod", prod, ""},
		{"unknown env", config{Env: "staging"}, "ENV must be dev or prod"},
		{"prod without BASE_URL", config{Env: "prod", BaseURL: "http://localhost:8080"}, "requires BASE_URL"},
		{"prod with http BASE_URL", config{Env: "prod", BaseURL: "http://next.djmagic.io", baseURLSet: true}, "must be an https URL"},
		{"trusted Fly header", func() config { c := prod; c.TrustedIPHeader = "Fly-Client-IP"; return c }(), ""},
		{"unknown trusted header", func() config { c := prod; c.TrustedIPHeader = "X-Real-IP"; return c }(), "TRUSTED_IP_HEADER"},
		{"prod without Turnstile", func() config { c := prod; c.TurnstileSiteKey, c.TurnstileSecretKey = "", ""; return c }(), "requires TURNSTILE"},
		{"half a Turnstile pair", config{Env: "dev", TurnstileSiteKey: "site"}, "must both be set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfigUsesPortWhenAddrUnset(t *testing.T) {
	t.Setenv("ADDR", "")
	t.Setenv("PORT", "5000")
	t.Setenv("BASE_URL", "https://next.djmagic.io/")
	cfg := loadConfig()
	if cfg.Addr != ":5000" {
		t.Errorf("Addr = %q", cfg.Addr)
	}
	if cfg.BaseURL != "https://next.djmagic.io" || !cfg.baseURLSet {
		t.Errorf("BaseURL = %q set=%v", cfg.BaseURL, cfg.baseURLSet)
	}
}

func TestLoadConfigTurnstileTestKeysInDevOnly(t *testing.T) {
	t.Setenv("TURNSTILE_SITE_KEY", "")
	t.Setenv("TURNSTILE_SECRET_KEY", "")
	t.Setenv("ENV", "dev")
	if cfg := loadConfig(); cfg.TurnstileSiteKey != turnstileTestSiteKey || cfg.TurnstileSecretKey != turnstileTestSecretKey {
		t.Errorf("dev keys = %q %q", cfg.TurnstileSiteKey, cfg.TurnstileSecretKey)
	}
	t.Setenv("ENV", "prod")
	if cfg := loadConfig(); cfg.TurnstileSiteKey != "" || cfg.TurnstileSecretKey != "" {
		t.Errorf("prod got test keys")
	}
}

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	body := "# comment\n\nDJM_TEST_A=1\nexport DJM_TEST_B=two # note\nDJM_TEST_C='keep # this'\nDJM_TEST_SET=from-file\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"DJM_TEST_A", "DJM_TEST_B", "DJM_TEST_C"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Setenv("DJM_TEST_SET", "from-env")

	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"DJM_TEST_A":   "1",
		"DJM_TEST_B":   "two",
		"DJM_TEST_C":   "keep # this",
		"DJM_TEST_SET": "from-env",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if err := loadDotEnv(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	bad := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(bad, []byte("not a pair\n"), 0o644)
	if err := loadDotEnv(bad); err == nil {
		t.Fatal("expected an error for a malformed line")
	}
}
