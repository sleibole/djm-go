package main

import (
	"context"
	"time"
)

const (
	cleanupInterval = time.Hour
	// unconfirmedLifetime is how long an account whose email link was never used is kept.
	// Typos in the email form create these; they can't sign in, so they hold nothing.
	unconfirmedLifetime = 7 * 24 * time.Hour
)

// cleanup runs in this process and returns when ctx is cancelled.
func (a *app) cleanup(ctx context.Context) {
	a.cleanupOnce()
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.cleanupOnce()
		}
	}
}

func (a *app) cleanupOnce() {
	now := a.now()
	if err := a.db.Where("expires_at <= ?", now).Delete(&Session{}).Error; err != nil {
		a.logger.Error("cleanup sessions", "err", err)
	}
	if err := a.db.Where("consumed_at IS NOT NULL OR expires_at <= ?", now).Delete(&MagicToken{}).Error; err != nil {
		a.logger.Error("cleanup magic tokens", "err", err)
	}

	var stale []uint
	err := a.db.Model(&User{}).
		Where("email_confirmed_at IS NULL AND created_at <= ?", now.Add(-unconfirmedLifetime)).
		Pluck("id", &stale).Error
	if err != nil {
		a.logger.Error("cleanup unconfirmed users", "err", err)
		return
	}
	for _, id := range stale {
		if err := a.deleteUser(id); err != nil {
			a.logger.Error("cleanup unconfirmed user", "user_id", id, "err", err)
		}
	}
}
