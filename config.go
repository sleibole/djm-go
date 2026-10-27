package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const defaultCatalogCacheMax = 10 << 30

type config struct {
	Addr               string
	DBPath             string
	Env                string // "dev" or "prod"
	BaseURL            string // public origin for emailed links, QR codes, and canonical URLs
	AllowIndexing      bool   // prod only, and only when ALLOW_INDEXING=true
	MailBackend        string // "console", "smtp", or empty. Development defaults to console.
	DevAllowSMTP       bool   // dev only: DEV_ALLOW_SMTP=true is required before SMTP settings send mail
	SMTPHost           string // production relay. Development uses it only together with DevAllowSMTP.
	SMTPPort           string
	SMTPUser           string
	SMTPPass           string
	MailFrom           string
	TrustedIPHeader    string // empty uses RemoteAddr; otherwise only this header, set by the trusted proxy
	TurnstileSiteKey   string // public widget key; production requires TURNSTILE_SITE_KEY
	TurnstileSecretKey string // Siteverify secret; never log this
	CatalogCacheDir    string // built catalog databases; safe to delete, refilled from the blob store
	CatalogCacheMax    int64  // bytes; least recently used catalogs are removed past this
	BlobDir            string // local blob store: uploaded CSVs and built catalogs
	DevShowMagicLink   bool   // dev only: put the sign-in link on the check-your-email page
	baseURLSet         bool   // true when BASE_URL was set in the environment
	cacheMaxErr        error
}

func loadConfig() config {
	rawBase := strings.TrimSpace(os.Getenv("BASE_URL"))
	base := "http://localhost:8080"
	if rawBase != "" {
		base = strings.TrimSuffix(rawBase, "/")
	}
	cfg := config{
		Addr:               getenv("ADDR", ":8080"),
		DBPath:             getenv("DB_PATH", "data/djm.db"),
		Env:                getenv("ENV", "dev"),
		BaseURL:            base,
		baseURLSet:         rawBase != "",
		AllowIndexing:      os.Getenv("ALLOW_INDEXING") == "true",
		MailBackend:        strings.TrimSpace(os.Getenv("MAIL_BACKEND")),
		DevAllowSMTP:       os.Getenv("DEV_ALLOW_SMTP") == "true",
		SMTPHost:           os.Getenv("SMTP_HOST"),
		SMTPPort:           getenv("SMTP_PORT", "587"),
		SMTPUser:           os.Getenv("SMTP_USER"),
		SMTPPass:           os.Getenv("SMTP_PASS"),
		MailFrom:           getenv("MAIL_FROM", "DJMagic <noreply@djmagic.io>"),
		TrustedIPHeader:    strings.TrimSpace(os.Getenv("TRUSTED_IP_HEADER")),
		TurnstileSiteKey:   strings.TrimSpace(os.Getenv("TURNSTILE_SITE_KEY")),
		TurnstileSecretKey: strings.TrimSpace(os.Getenv("TURNSTILE_SECRET_KEY")),
		CatalogCacheDir:    getenv("CATALOG_CACHE_DIR", "data/catalogs"),
		CatalogCacheMax:    defaultCatalogCacheMax,
		BlobDir:            getenv("BLOB_DIR", "data/blobs"),
		DevShowMagicLink:   os.Getenv("DEV_SHOW_MAGIC_LINK") == "true",
	}
	if v := os.Getenv("CATALOG_CACHE_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			cfg.cacheMaxErr = fmt.Errorf("CATALOG_CACHE_MAX_BYTES must be a positive number of bytes, got %q", v)
		}
		cfg.CatalogCacheMax = n
	}
	if port := os.Getenv("PORT"); port != "" && os.Getenv("ADDR") == "" {
		cfg.Addr = ":" + port
	}
	cfg.applyTurnstileDevDefaults()
	return cfg
}

// applyTurnstileDevDefaults fills Cloudflare's always-pass test keys when
// development leaves both variables unset. Production never receives them.
func (c *config) applyTurnstileDevDefaults() {
	if c.Env == "prod" {
		return
	}
	if c.TurnstileSiteKey == "" && c.TurnstileSecretKey == "" {
		c.TurnstileSiteKey = turnstileTestSiteKey
		c.TurnstileSecretKey = turnstileTestSecretKey
	}
}

func (c config) validate() error {
	switch c.Env {
	case "dev", "prod":
	default:
		return fmt.Errorf("ENV must be dev or prod, got %q", c.Env)
	}
	switch c.TrustedIPHeader {
	case "", "CF-Connecting-IP", "Fly-Client-IP", "X-Forwarded-For":
	default:
		return fmt.Errorf("TRUSTED_IP_HEADER must be empty, CF-Connecting-IP, Fly-Client-IP, or X-Forwarded-For, got %q", c.TrustedIPHeader)
	}
	if c.cacheMaxErr != nil {
		return c.cacheMaxErr
	}
	if (c.TurnstileSiteKey == "") != (c.TurnstileSecretKey == "") {
		return errors.New("TURNSTILE_SITE_KEY and TURNSTILE_SECRET_KEY must both be set or both be empty")
	}
	if _, err := c.resolveMailBackend(); err != nil {
		return err
	}
	if c.Env != "prod" {
		return nil
	}
	if !c.baseURLSet {
		return errors.New("production requires BASE_URL")
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return fmt.Errorf("production BASE_URL must be an https URL, got %q", c.BaseURL)
	}
	if c.TurnstileSiteKey == "" {
		return errors.New("production requires TURNSTILE_SITE_KEY and TURNSTILE_SECRET_KEY")
	}
	if c.SMTPHost == "" {
		return errors.New("production requires SMTP_HOST")
	}
	if c.DevShowMagicLink {
		return errors.New("production rejects DEV_SHOW_MAGIC_LINK")
	}
	return nil
}

const (
	mailBackendConsole = "console"
	mailBackendSMTP    = "smtp"
)

// resolveMailBackend chooses where mail goes.
// Development uses the console unless SMTP is selected and DEV_ALLOW_SMTP=true.
// SMTP settings with no opt-in are a startup failure, not a delivery path.
// MAIL_BACKEND=console stays on the console even when those settings are present.
// An empty backend with no SMTP host is the console. It never becomes SMTP.
// Production uses SMTP. The console backend and DEV_ALLOW_SMTP are rejected.
func (c config) resolveMailBackend() (string, error) {
	backend := strings.ToLower(strings.TrimSpace(c.MailBackend))
	switch c.Env {
	case "prod":
		if c.DevAllowSMTP {
			return "", errors.New("production rejects DEV_ALLOW_SMTP")
		}
		switch backend {
		case "", mailBackendSMTP:
			return mailBackendSMTP, nil
		default:
			return "", fmt.Errorf("production mail backend must be smtp, got %q", c.MailBackend)
		}
	case "dev":
		switch backend {
		case mailBackendConsole:
			return mailBackendConsole, nil
		case mailBackendSMTP:
			if !c.DevAllowSMTP {
				return "", errors.New("development MAIL_BACKEND=smtp requires DEV_ALLOW_SMTP=true in addition to SMTP settings")
			}
			if c.SMTPHost == "" {
				return "", errors.New("development MAIL_BACKEND=smtp requires SMTP_HOST; missing mail configuration will not use a relay")
			}
			return mailBackendSMTP, nil
		case "":
			if c.SMTPHost != "" && !c.DevAllowSMTP {
				return "", errors.New("development SMTP requires DEV_ALLOW_SMTP=true before it will send mail. Set DEV_ALLOW_SMTP=true to allow SMTP, or set MAIL_BACKEND=console to keep the console backend")
			}
			if c.SMTPHost != "" && c.DevAllowSMTP {
				return mailBackendSMTP, nil
			}
			return mailBackendConsole, nil
		default:
			return "", fmt.Errorf("MAIL_BACKEND must be console or smtp, got %q", c.MailBackend)
		}
	default:
		return "", fmt.Errorf("ENV must be dev or prod, got %q", c.Env)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
