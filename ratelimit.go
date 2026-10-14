package main

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	rateLimitMaxKeys    = 10_000
	rateLimitSweepEvery = 64
)

// rateLimiter allows up to limit events per key in each fixed window. In memory, so it
// resets on restart, which is fine for slowing down password guessing and email spam.
// The map has a hard cap so new keys cannot grow it without bound. Past the cap, a
// new key is refused. Expired windows are dropped every sweepEvery allows, not on
// every call.
type rateLimiter struct {
	mu         sync.Mutex
	limit      int
	window     time.Duration
	now        func() time.Time
	hits       map[string]*windowCount
	maxKeys    int
	sweepEvery int
	calls      int
}

type windowCount struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, window time.Duration, now func() time.Time) *rateLimiter {
	return &rateLimiter{
		limit:      limit,
		window:     window,
		now:        now,
		hits:       map[string]*windowCount{},
		maxKeys:    rateLimitMaxKeys,
		sweepEvery: rateLimitSweepEvery,
	}
}

func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	t := l.now()
	l.calls++
	if l.sweepEvery > 0 && l.calls%l.sweepEvery == 0 {
		l.sweep(t)
	}

	w, ok := l.hits[key]
	if !ok || t.Sub(w.start) >= l.window {
		if !ok && len(l.hits) >= l.maxKeys {
			return false
		}
		l.hits[key] = &windowCount{start: t, count: 1}
		return true
	}
	if w.count >= l.limit {
		return false
	}
	w.count++
	return true
}

func (l *rateLimiter) sweep(t time.Time) {
	for k, w := range l.hits {
		if t.Sub(w.start) >= l.window {
			delete(l.hits, k)
		}
	}
}

// clientIP returns the address used for rate limits. trustedHeader is empty unless
// this process is behind a proxy that strips client-supplied forwarding headers and
// sets that one header itself. Any other header is ignored. X-Forwarded-For is a
// comma-separated list and only its first address is used. Every other trusted
// header is a single address.
func clientIP(r *http.Request, trustedHeader string) string {
	if trustedHeader != "" {
		raw := strings.TrimSpace(r.Header.Get(trustedHeader))
		if trustedHeader == "X-Forwarded-For" {
			raw, _, _ = strings.Cut(raw, ",")
			raw = strings.TrimSpace(raw)
		}
		if ip := net.ParseIP(raw); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *app) clientIP(r *http.Request) string {
	return clientIP(r, a.cfg.TrustedIPHeader)
}
