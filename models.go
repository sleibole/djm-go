package main

import "time"

// The schema lives in migrations/*.sql. These structs map rows for GORM.
// A `default` tag tells GORM to leave a zero value out of INSERT so the
// column default in the migration applies.

const (
	roleAudience = "audience"
	roleDJ       = "dj"
	roleAdmin    = "admin"

	showTypeKaraoke = "karaoke"
	showTypeDJ      = "dj"

	rotationStandard = "standard"
)

type User struct {
	ID                       uint
	Email                    string  // stored lowercased
	Role                     string  `gorm:"default:audience"`
	Slug                     *string // public handle for /dj/{slug}; nil until they host
	DisplayName              string  // shown to the audience; empty falls back to the slug
	PasswordHash             string  // empty if they only use magic links
	EmailConfirmedAt         *time.Time
	FailedLoginAttempts      int
	LockedAt                 *time.Time
	DefaultShowType          string `gorm:"default:karaoke"`
	DefaultRotationStyle     string `gorm:"default:standard"`
	DefaultMaxSongsPerSinger *int   // nil = no limit
	CreatedAt                time.Time
	UpdatedAt                time.Time
}

func (u User) HasPassword() bool { return u.PasswordHash != "" }

// Hosts reports whether the user can run shows and catalogs.
func (u User) Hosts() bool { return u.Role == roleDJ || u.Role == roleAdmin }

func (u User) Handle() string {
	if u.Slug == nil {
		return ""
	}
	return *u.Slug
}

func (u User) PublicName() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Handle()
}

type Session struct {
	ID        string // sha256 hex of the cookie value; the raw token is only in the cookie
	UserID    uint
	ExpiresAt time.Time
	CreatedAt time.Time
}

// MagicToken is a one-time sign-in link. Only the hash is stored,
// so a database leak can't be used to sign in.
type MagicToken struct {
	ID         uint
	UserID     uint
	TokenHash  string // sha256 hex of the token in the emailed link
	ExpiresAt  time.Time
	ConsumedAt *time.Time
	CreatedAt  time.Time
}
