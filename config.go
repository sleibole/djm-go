package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type config struct {
	Addr            string
	DBPath          string
	Env             string // "dev" or "prod"
	BaseURL         string // public origin for emailed links, QR codes, and Open Graph URLs
	TrustedIPHeader string // empty uses RemoteAddr; otherwise only this header, set by the trusted proxy
	baseURLSet      bool   // true when BASE_URL was set in the environment
}

func loadConfig() config {
	rawBase := strings.TrimSpace(os.Getenv("BASE_URL"))
	base := "http://localhost:8080"
	if rawBase != "" {
		base = strings.TrimSuffix(rawBase, "/")
	}
	cfg := config{
		Addr:            getenv("ADDR", ":8080"),
		DBPath:          getenv("DB_PATH", "data/djm.db"),
		Env:             getenv("ENV", "dev"),
		BaseURL:         base,
		baseURLSet:      rawBase != "",
		TrustedIPHeader: strings.TrimSpace(os.Getenv("TRUSTED_IP_HEADER")),
	}
	if port := os.Getenv("PORT"); port != "" && os.Getenv("ADDR") == "" {
		cfg.Addr = ":" + port
	}
	return cfg
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
	return nil
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
