package main

import (
	"bytes"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// No test in this package may open a real SMTP connection.
	installBlockedSMTPDial(nil)
	os.Exit(m.Run())
}

func installBlockedSMTPDial(dialed *int) {
	refuse := func(addr string) (net.Conn, error) {
		if dialed != nil {
			*dialed++
		}
		return nil, &textproto.Error{Code: 550, Msg: "test blocked SMTP to " + addr}
	}
	smtpNetDial = func(deadline time.Time, network, addr string) (net.Conn, error) {
		return refuse(addr)
	}
	smtpTLSDial = func(deadline time.Time, network, addr string, config *tls.Config) (net.Conn, error) {
		return refuse(addr)
	}
}

func setMailEnv(t *testing.T, values map[string]string) {
	t.Helper()
	for _, key := range []string{
		"ENV", "BASE_URL", "MAIL_BACKEND", "DEV_ALLOW_SMTP", "DEV_SHOW_MAGIC_LINK",
		"SMTP_HOST", "SMTP_PORT", "SMTP_USER", "SMTP_PASS", "MAIL_FROM",
		"TURNSTILE_SITE_KEY", "TURNSTILE_SECRET_KEY",
	} {
		t.Setenv(key, "")
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
}

func mailLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, nil))
}

func assertNoSecrets(t *testing.T, label, text string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatalf("%s exposed %q", label, secret)
		}
	}
}

func TestDevMailDefaultsToConsole(t *testing.T) {
	setMailEnv(t, map[string]string{"ENV": "dev"})
	var buf bytes.Buffer
	cfg := loadConfig()
	if cfg.MailBackend != "" || cfg.DevAllowSMTP || cfg.SMTPHost != "" {
		t.Fatalf("mail config = backend %q allow %v host %q", cfg.MailBackend, cfg.DevAllowSMTP, cfg.SMTPHost)
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	m, err := newMailer(cfg, mailLogger(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(consoleMailer); !ok {
		t.Fatalf("mailer = %T, want console", m)
	}
	body := "Visit http://localhost:8080/auth/dev-default-token"
	if err := m.Send("a@example.com", "Your DJMagic.io login link", body); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "backend=console") {
		t.Fatalf("startup log = %s", got)
	}
	if strings.Contains(got, "dev-default-token") || strings.Contains(got, body) {
		t.Fatalf("console log included the message: %s", got)
	}
}

func TestDevSMTPCredentialsDoNotSend(t *testing.T) {
	const (
		host = "smtp.example.com"
		user = "inherited-smtp-user"
		pass = "inherited-smtp-pass"
	)
	setMailEnv(t, map[string]string{
		"ENV":       "dev",
		"SMTP_HOST": host,
		"SMTP_PORT": "587",
		"SMTP_USER": user,
		"SMTP_PASS": pass,
	})
	cfg := loadConfig()
	if cfg.DevAllowSMTP || cfg.MailBackend != "" {
		t.Fatalf("opt-in was inferred from credentials: backend %q allow %v", cfg.MailBackend, cfg.DevAllowSMTP)
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "DEV_ALLOW_SMTP=true") {
		t.Fatalf("validate err = %v", err)
	}
	assertNoSecrets(t, "startup error", err.Error(), user, pass, host)

	var buf bytes.Buffer
	m, err := newMailer(cfg, mailLogger(&buf))
	if err == nil || m != nil {
		t.Fatalf("newMailer = %T, %v", m, err)
	}
	assertNoSecrets(t, "mailer error", err.Error(), user, pass)
	if strings.Contains(buf.String(), pass) || strings.Contains(buf.String(), "backend=smtp") {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestDevSMTPRequiresOptIn(t *testing.T) {
	const pass = "opt-in-smtp-pass"

	t.Run("backend without flag", func(t *testing.T) {
		setMailEnv(t, map[string]string{
			"ENV":          "dev",
			"MAIL_BACKEND": "smtp",
			"SMTP_HOST":    "smtp.example.com",
			"SMTP_USER":    "mail-user",
			"SMTP_PASS":    pass,
		})
		err := loadConfig().validate()
		if err == nil || !strings.Contains(err.Error(), "DEV_ALLOW_SMTP=true") {
			t.Fatalf("err = %v", err)
		}
		assertNoSecrets(t, "error", err.Error(), pass)
	})

	t.Run("flag must be exact", func(t *testing.T) {
		setMailEnv(t, map[string]string{
			"ENV":            "dev",
			"MAIL_BACKEND":   "smtp",
			"DEV_ALLOW_SMTP": "TRUE",
			"SMTP_HOST":      "smtp.example.com",
			"SMTP_PASS":      pass,
		})
		if loadConfig().DevAllowSMTP {
			t.Fatal("only the exact string true enables DEV_ALLOW_SMTP")
		}
		if err := loadConfig().validate(); err == nil {
			t.Fatal("expected startup to fail")
		}
	})

	t.Run("flag without host does not select a relay", func(t *testing.T) {
		setMailEnv(t, map[string]string{
			"ENV":            "dev",
			"DEV_ALLOW_SMTP": "true",
		})
		var buf bytes.Buffer
		cfg := loadConfig()
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
		m, err := newMailer(cfg, mailLogger(&buf))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := m.(consoleMailer); !ok {
			t.Fatalf("mailer = %T, want console", m)
		}
		if strings.Contains(buf.String(), "backend=smtp") {
			t.Fatalf("log = %s", buf.String())
		}
	})

	t.Run("smtp backend without host", func(t *testing.T) {
		setMailEnv(t, map[string]string{
			"ENV":            "dev",
			"MAIL_BACKEND":   "smtp",
			"DEV_ALLOW_SMTP": "true",
		})
		err := loadConfig().validate()
		if err == nil || !strings.Contains(err.Error(), "SMTP_HOST") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("flag and host", func(t *testing.T) {
		setMailEnv(t, map[string]string{
			"ENV":            "dev",
			"DEV_ALLOW_SMTP": "true",
			"SMTP_HOST":      "localhost",
			"SMTP_PORT":      "1025",
			"SMTP_USER":      "mailhog",
			"SMTP_PASS":      pass,
			"MAIL_FROM":      "DJMagic <noreply@djmagic.io>",
		})
		var buf bytes.Buffer
		cfg := loadConfig()
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
		m, err := newMailer(cfg, mailLogger(&buf))
		if err != nil {
			t.Fatal(err)
		}
		smtp, ok := m.(smtpMailer)
		if !ok {
			t.Fatalf("mailer = %T, want smtp", m)
		}
		if smtp.addr != "localhost:1025" || smtp.requireTLS {
			t.Fatalf("addr %q requireTLS %v", smtp.addr, smtp.requireTLS)
		}
		got := buf.String()
		if !strings.Contains(got, "backend=smtp") {
			t.Fatalf("log = %s", got)
		}
		assertNoSecrets(t, "startup log", got, pass, "mailhog")
		body := "Visit http://localhost:8080/auth/opt-in-token"
		if err := m.Send("a@example.com", "Your DJMagic.io login link", body); err == nil || !strings.Contains(err.Error(), "test blocked SMTP") {
			t.Fatalf("send err = %v", err)
		}
		assertNoSecrets(t, "send log", buf.String(), pass, "opt-in-token", body)
	})
}

func TestDevExplicitConsoleIgnoresSMTP(t *testing.T) {
	const pass = "console-override-pass"
	setMailEnv(t, map[string]string{
		"ENV":            "dev",
		"MAIL_BACKEND":   "console",
		"DEV_ALLOW_SMTP": "true",
		"SMTP_HOST":      "smtp.example.com",
		"SMTP_USER":      "ses-user",
		"SMTP_PASS":      pass,
	})
	var buf bytes.Buffer
	cfg := loadConfig()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	m, err := newMailer(cfg, mailLogger(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(consoleMailer); !ok {
		t.Fatalf("mailer = %T, want console", m)
	}
	got := buf.String()
	if !strings.Contains(got, "backend=console") || !strings.Contains(got, "smtp_settings=ignored") {
		t.Fatalf("log = %s", got)
	}
	assertNoSecrets(t, "log", got, pass, "ses-user", "smtp.example.com")
	body := "Visit http://localhost:8080/auth/ignored-smtp-token"
	if err := m.Send("a@example.com", "subject", body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "ignored-smtp-token") {
		t.Fatalf("log included the link: %s", buf.String())
	}
}

func TestProductionMailStaysConfiguredAndQuiet(t *testing.T) {
	const (
		user  = "prod-smtp-user"
		pass  = "prod-smtp-pass"
		token = "prod-magic-token"
	)
	prodEnv := map[string]string{
		"ENV":                  "prod",
		"BASE_URL":             "https://next.djmagic.io",
		"TURNSTILE_SITE_KEY":   "site",
		"TURNSTILE_SECRET_KEY": "secret",
		"SMTP_HOST":            "smtp.example.com",
		"SMTP_PORT":            "587",
		"SMTP_USER":            user,
		"SMTP_PASS":            pass,
		"MAIL_FROM":            "DJMagic <noreply@djmagic.io>",
	}

	t.Run("requires SMTP host", func(t *testing.T) {
		setMailEnv(t, prodEnv)
		t.Setenv("SMTP_HOST", "")
		err := loadConfig().validate()
		if err == nil || !strings.Contains(err.Error(), "requires SMTP_HOST") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("rejects the dev opt-in", func(t *testing.T) {
		setMailEnv(t, prodEnv)
		t.Setenv("DEV_ALLOW_SMTP", "true")
		err := loadConfig().validate()
		if err == nil || !strings.Contains(err.Error(), "DEV_ALLOW_SMTP") {
			t.Fatalf("err = %v", err)
		}
		assertNoSecrets(t, "error", err.Error(), pass, token)
	})

	t.Run("rejects the console backend", func(t *testing.T) {
		setMailEnv(t, prodEnv)
		t.Setenv("MAIL_BACKEND", "console")
		err := loadConfig().validate()
		if err == nil || !strings.Contains(err.Error(), "must be smtp") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("uses smtp and does not log the message", func(t *testing.T) {
		setMailEnv(t, prodEnv)
		var buf bytes.Buffer
		cfg := loadConfig()
		if err := cfg.validate(); err != nil {
			t.Fatal(err)
		}
		m, err := newMailer(cfg, mailLogger(&buf))
		if err != nil {
			t.Fatal(err)
		}
		smtp, ok := m.(smtpMailer)
		if !ok {
			t.Fatalf("mailer = %T, want smtp", m)
		}
		if !smtp.requireTLS || smtp.addr != "smtp.example.com:587" {
			t.Fatalf("addr %q requireTLS %v", smtp.addr, smtp.requireTLS)
		}
		body := "Visit https://next.djmagic.io/auth/" + token
		err = m.Send("person@example.com", "Your DJMagic.io login link", body)
		if err == nil || !strings.Contains(err.Error(), "test blocked SMTP") {
			t.Fatalf("send err = %v", err)
		}
		got := buf.String()
		if !strings.Contains(got, "backend=smtp") {
			t.Fatalf("log = %s", got)
		}
		assertNoSecrets(t, "log", got, user, pass, token, body, "/auth/")
		assertNoSecrets(t, "send error", err.Error(), pass, token, body)
	})
}

func TestEnvFileCannotOverrideConsoleMail(t *testing.T) {
	const pass = "dotenv-smtp-pass"
	setMailEnv(t, map[string]string{
		"ENV":            "dev",
		"MAIL_BACKEND":   "console",
		"DEV_ALLOW_SMTP": "false",
		"SMTP_HOST":      "",
		"SMTP_USER":      "",
		"SMTP_PASS":      "",
	})
	path := filepath.Join(t.TempDir(), ".env")
	file := "MAIL_BACKEND=smtp\nDEV_ALLOW_SMTP=true\nSMTP_HOST=smtp.example.com\nSMTP_USER=ses-user\nSMTP_PASS=" + pass + "\n"
	if err := os.WriteFile(path, []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MAIL_BACKEND") != "console" || os.Getenv("DEV_ALLOW_SMTP") != "false" || os.Getenv("SMTP_HOST") != "" {
		t.Fatalf("dotenv overrode the process: backend %q allow %q host %q", os.Getenv("MAIL_BACKEND"), os.Getenv("DEV_ALLOW_SMTP"), os.Getenv("SMTP_HOST"))
	}
	var buf bytes.Buffer
	cfg := loadConfig()
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	m, err := newMailer(cfg, mailLogger(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(consoleMailer); !ok {
		t.Fatalf("mailer = %T, want console", m)
	}
	body := "Visit http://localhost:8080/auth/dotenv-token"
	if err := m.Send("a@example.com", "subject", body); err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "log", buf.String(), pass, "dotenv-token", "ses-user")
}

func TestAppMailStaysInMemory(t *testing.T) {
	var dials int
	installBlockedSMTPDial(&dials)
	t.Cleanup(func() { installBlockedSMTPDial(nil) })

	a := newTestApp(t)
	if _, ok := a.mailer.(*testMailer); !ok {
		t.Fatalf("app mailer = %T, want in-memory", a.mailer)
	}
	b := newBrowser(t, a)
	rec := b.post("/signin/link", url.Values{"email": {"memory@example.com"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("link request: %d\n%s", rec.Code, rec.Body.String())
	}
	sent := a.mailer.(*testMailer).all()
	if len(sent) != 1 || !strings.Contains(sent[0].Body, "/auth/") {
		t.Fatalf("recorded mail = %+v", sent)
	}
	if dials != 0 {
		t.Fatalf("SMTP dials = %d", dials)
	}
}
