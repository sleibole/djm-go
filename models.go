package main

import (
	"encoding/json"
	"time"
)

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

const (
	catalogEmpty      = "empty"
	catalogPending    = "pending"
	catalogProcessing = "processing"
	catalogReady      = "ready"
	catalogFailed     = "failed"
)

var variantDisplayChoices = []choice{
	{"none", "None"},
	{"version", "Version (Original, Acoustic, Key +2…)"},
	{"id", "ID (disc/track number)"},
}

type Catalog struct {
	ID                 uint
	UserID             uint
	Name               string
	VariantDisplay     string `gorm:"default:none"`
	Status             string `gorm:"default:empty"`
	ActiveVersion      *int
	BuildingVersion    *int
	SongCount          int
	RowCount           int
	ErrorsJSON         string `gorm:"column:errors_json"`
	ImportMetadataJSON string `gorm:"column:import_metadata_json"`
	LastCSVSHA256      string `gorm:"column:last_csv_sha256"`
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (c Catalog) Building() bool { return c.Status == catalogPending || c.Status == catalogProcessing }

// Errors is why the latest upload failed.
func (c Catalog) Errors() []csvError {
	var errs []csvError
	if c.ErrorsJSON != "" {
		json.Unmarshal([]byte(c.ErrorsJSON), &errs)
	}
	return errs
}

// importMetadata is stored for the active version. The JSON keys are the Rails ones.
type importMetadata struct {
	replacementStats
	SourceEncoding     string `json:"source_encoding"`
	NormalizedEncoding string `json:"normalized_encoding"`
}

func (c Catalog) ImportMetadata() importMetadata {
	var m importMetadata
	if c.ImportMetadataJSON != "" {
		json.Unmarshal([]byte(c.ImportMetadataJSON), &m)
	}
	return m
}

type CatalogVersion struct {
	ID             uint
	CatalogID      uint
	Version        int
	Status         string `gorm:"default:pending"`
	CSVSHA256      string `gorm:"column:csv_sha256"`
	CSVBytes       int64  `gorm:"column:csv_bytes"`
	SourceEncoding string
	RowCount       int
	SongCount      int
	DBBytes        int64 `gorm:"column:db_bytes"`
	CreatedAt      time.Time
}
