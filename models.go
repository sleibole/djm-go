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
	ID             string // sha256 hex of the cookie value; the raw token is only in the cookie
	UserID         uint
	ImpersonatorID *uint // the admin using "log in as"
	ExpiresAt      time.Time
	CreatedAt      time.Time
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

const (
	showScheduled = "scheduled"
	showActive    = "active"
	showEnded     = "ended"
)

type Show struct {
	ID                 uint
	UserID             uint
	CatalogID          *uint
	Name               string
	Slug               string
	ShowType           string `gorm:"default:karaoke"`
	Status             string
	ScheduledAt        *time.Time
	StartedAt          *time.Time
	EndedAt            *time.Time
	RotationStyle      string `gorm:"default:standard"`
	ApprovalRequired   bool
	ManualEntryEnabled bool
	MaxSongsPerSinger  *int
	CreatedAt          time.Time
	UpdatedAt          time.Time

	Catalog *Catalog `gorm:"-"` // loaded by the handlers that need it
}

func (s Show) Scheduled() bool { return s.Status == showScheduled }
func (s Show) Active() bool    { return s.Status == showActive }
func (s Show) Ended() bool     { return s.Status == showEnded }
func (s Show) Karaoke() bool   { return s.ShowType == showTypeKaraoke }

// DisplayName is the show's name, else its catalog's, as in Rails.
func (s Show) DisplayName() string {
	switch {
	case s.Name != "":
		return s.Name
	case s.Catalog != nil:
		return s.Catalog.Name
	default:
		return "Untitled Show"
	}
}

// ParticipantLabel is what the people in the queue are called.
func (s Show) ParticipantLabel() string {
	if s.Karaoke() {
		return "Singer"
	}
	return "Guest"
}

// VariantDisplay is the catalog's setting. A show without a catalog shows
// whatever a request carries.
func (s Show) VariantDisplay() string {
	if s.Catalog == nil {
		return ""
	}
	return s.Catalog.VariantDisplay
}

type AudienceProfile struct {
	ID             uint
	GuestTokenHash *string
	UserID         *uint
	LastSeenAt     *time.Time
	CreatedAt      time.Time
}

type ShowParticipant struct {
	ID                uint
	ShowID            uint
	AudienceProfileID *uint
	DisplayName       string
	CreatedAt         time.Time
}

const (
	requestPending    = "pending"
	requestQueued     = "queued"
	requestNowPlaying = "now_playing"
	requestCompleted  = "completed"
	requestSkipped    = "skipped"
	requestRejected   = "rejected"
	requestCanceled   = "canceled"

	sourceAudience    = "audience"
	sourceHostManual  = "host_manual"
	sourceHostCatalog = "host_catalog"
	sourceSaved       = "saved" // from Saved Songs or My Performances
)

type SongRequest struct {
	ID                uint
	ShowID            uint
	ShowParticipantID uint
	SongTitle         string
	SongArtist        string
	SongVersion       string
	SongExternalID    string `gorm:"column:song_external_id"`
	RequestSource     string
	Status            string
	QueuePosition     *int
	RequestedAt       time.Time
	StartedAt         *time.Time
	CompletedAt       *time.Time
	RejectedAt        *time.Time
	RejectionReason   string
	CanceledAt        *time.Time

	Participant string `gorm:"-"` // the participant's display name, filled by queries that join it
}

type SavedSong struct {
	ID                uint
	AudienceProfileID uint
	Fingerprint       string
	SongTitle         string
	SongArtist        string
	SongVersion       string
	SongExternalID    string `gorm:"column:song_external_id"`
	SourceCatalogID   *uint
	RequestCount      int
	LastRequestedAt   *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

var feedbackCategories = []choice{
	{"bug", "Bug"},
	{"feature_request", "Feature Request"},
	{"confusing_ux", "Confusing UX"},
	{"other", "Other"},
}

var feedbackStatuses = []choice{
	{"new", "New"},
	{"reviewed", "Reviewed"},
	{"resolved", "Resolved"},
	{"dismissed", "Dismissed"},
}

type Feedback struct {
	ID         uint
	UserID     *uint
	Category   string
	Message    string
	PageURL    string `gorm:"column:page_url"`
	Status     string `gorm:"default:new"`
	AdminNotes string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (Feedback) TableName() string { return "feedback" }

func (f Feedback) CategoryLabel() string { return choiceLabel(feedbackCategories, f.Category) }
func (f Feedback) StatusLabel() string   { return choiceLabel(feedbackStatuses, f.Status) }

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
