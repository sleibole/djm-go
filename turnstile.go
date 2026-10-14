package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	turnstileSiteverifyURL   = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	turnstileField           = "cf-turnstile-response"
	turnstileRejectedMessage = "Verification failed. Please try again."
	turnstileTimeout         = 5 * time.Second

	// Cloudflare's published always-pass dummy keys. They are not production
	// credentials. Development uses them when both Turnstile variables are unset.
	// https://developers.cloudflare.com/turnstile/troubleshooting/testing/
	turnstileTestSiteKey   = "1x00000000000000000000AA"
	turnstileTestSecretKey = "1x0000000000000000000000000000000AA"
)

// turnstileVerifier checks a widget token with Cloudflare's Siteverify API.
// The HTTP client is replaceable so tests never call Cloudflare.
type turnstileVerifier struct {
	secret   string
	endpoint string
	client   *http.Client
}

func newTurnstileVerifier(secret string) *turnstileVerifier {
	return &turnstileVerifier{
		secret:   secret,
		endpoint: turnstileSiteverifyURL,
		client:   &http.Client{Timeout: turnstileTimeout},
	}
}

// verify returns an error for a missing token and for every Siteverify failure.
// The error is safe to log: it does not include the secret or the token.
func (v *turnstileVerifier) verify(ctx context.Context, token, remoteIP string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("missing token")
	}
	form := url.Values{}
	form.Set("secret", v.secret)
	form.Set("response", token)
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("siteverify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("siteverify request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("siteverify response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("siteverify status %d", resp.StatusCode)
	}

	var parsed struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return errors.New("siteverify malformed response")
	}
	if !parsed.Success {
		if len(parsed.ErrorCodes) > 0 {
			return fmt.Errorf("siteverify rejected: %s", strings.Join(parsed.ErrorCodes, ","))
		}
		return errors.New("siteverify rejected")
	}
	return nil
}

// turnstileFailed reports whether this request may not perform a protected
// action. A failure is logged without the secret or the submitted token.
func (a *app) turnstileFailed(r *http.Request) bool {
	err := a.turnstile.verify(r.Context(), r.PostFormValue(turnstileField), a.clientIP(r))
	if err == nil {
		return false
	}
	a.logger.Warn("turnstile verification failed",
		"method", r.Method,
		"path", logPath(r.URL.Path),
		"err", err,
	)
	return true
}
