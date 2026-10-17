CREATE TABLE shows (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id              INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  catalog_id           INTEGER REFERENCES catalogs (id) ON DELETE CASCADE, -- NULL = requests are typed in
  name                 TEXT NOT NULL DEFAULT '',  -- empty falls back to the catalog name
  slug                 TEXT NOT NULL,             -- /dj/{handle}/shows/{slug}
  show_type            TEXT NOT NULL DEFAULT 'karaoke' CHECK (show_type IN ('karaoke', 'dj')),
  status               TEXT NOT NULL CHECK (status IN ('scheduled', 'active', 'ended')),
  scheduled_at         DATETIME,
  started_at           DATETIME,
  ended_at             DATETIME,
  rotation_style       TEXT NOT NULL DEFAULT 'standard' CHECK (rotation_style IN ('standard')),
  approval_required    BOOLEAN NOT NULL DEFAULT 1, -- audience requests wait in pending
  manual_entry_enabled BOOLEAN NOT NULL DEFAULT 0, -- audience may type a song not in the catalog
  max_songs_per_singer INTEGER,                    -- pending + queued per singer; NULL = no limit
  created_at           DATETIME NOT NULL,
  updated_at           DATETIME NOT NULL
);
CREATE UNIQUE INDEX shows_user_id_slug ON shows (user_id, slug);
CREATE UNIQUE INDEX shows_one_active_per_user ON shows (user_id) WHERE status = 'active';
CREATE INDEX shows_catalog_id ON shows (catalog_id);
CREATE INDEX shows_status ON shows (status);

-- Who a request is for. A host-typed singer gets a profile with neither a guest
-- token nor a user; audience guests and accounts arrive in later phases.
CREATE TABLE audience_profiles (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  guest_token_hash TEXT UNIQUE,                                               -- sha256 hex of the guest cookie
  user_id          INTEGER UNIQUE REFERENCES users (id) ON DELETE SET NULL,
  last_seen_at     DATETIME,
  created_at       DATETIME NOT NULL
);

-- A profile's name within one show. The profile can be deleted later (stale
-- guests); the name stays for the show's history.
CREATE TABLE show_participants (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  show_id             INTEGER NOT NULL REFERENCES shows (id) ON DELETE CASCADE,
  audience_profile_id INTEGER REFERENCES audience_profiles (id) ON DELETE SET NULL,
  display_name        TEXT NOT NULL,
  created_at          DATETIME NOT NULL
);
CREATE UNIQUE INDEX show_participants_show_id_profile ON show_participants (show_id, audience_profile_id);
CREATE INDEX show_participants_show_id_name ON show_participants (show_id, display_name);
CREATE INDEX show_participants_profile ON show_participants (audience_profile_id);

CREATE TABLE song_requests (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  show_id             INTEGER NOT NULL REFERENCES shows (id) ON DELETE CASCADE,
  show_participant_id INTEGER NOT NULL REFERENCES show_participants (id) ON DELETE CASCADE,
  song_title          TEXT NOT NULL,
  song_artist         TEXT NOT NULL DEFAULT '',
  song_version        TEXT NOT NULL DEFAULT '',
  song_external_id    TEXT NOT NULL DEFAULT '',
  request_source      TEXT NOT NULL CHECK (request_source IN ('audience', 'host_manual', 'host_catalog', 'saved')),
  status              TEXT NOT NULL CHECK (status IN ('pending', 'queued', 'now_playing', 'completed', 'skipped', 'rejected', 'canceled')),
  queue_position      INTEGER,  -- 1-based among queued requests; NULL otherwise
  requested_at        DATETIME NOT NULL,
  started_at          DATETIME,
  completed_at        DATETIME, -- when it was done or skipped
  rejected_at         DATETIME,
  rejection_reason    TEXT NOT NULL DEFAULT '',
  canceled_at         DATETIME
);
CREATE INDEX song_requests_show_status_position ON song_requests (show_id, status, queue_position);
CREATE UNIQUE INDEX song_requests_one_now_playing ON song_requests (show_id) WHERE status = 'now_playing';
CREATE INDEX song_requests_participant ON song_requests (show_participant_id);
