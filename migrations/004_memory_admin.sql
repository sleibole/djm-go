-- Songs a signed-in visitor saved (the heart). One row per song per profile.
CREATE TABLE saved_songs (
  id                  INTEGER PRIMARY KEY AUTOINCREMENT,
  audience_profile_id INTEGER NOT NULL REFERENCES audience_profiles (id) ON DELETE CASCADE,
  fingerprint         TEXT NOT NULL, -- ext:{id}, or norm:{title}|{artist}|{version}
  song_title          TEXT NOT NULL,
  song_artist         TEXT NOT NULL DEFAULT '',
  song_version        TEXT NOT NULL DEFAULT '',
  song_external_id    TEXT NOT NULL DEFAULT '',
  source_catalog_id   INTEGER REFERENCES catalogs (id) ON DELETE SET NULL,
  request_count       INTEGER NOT NULL DEFAULT 0,
  last_requested_at   DATETIME,
  created_at          DATETIME NOT NULL,
  updated_at          DATETIME NOT NULL
);
CREATE UNIQUE INDEX saved_songs_profile_fingerprint ON saved_songs (audience_profile_id, fingerprint);

CREATE TABLE feedback (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER REFERENCES users (id) ON DELETE SET NULL, -- NULL when sent signed out
  category    TEXT NOT NULL CHECK (category IN ('bug', 'feature_request', 'confusing_ux', 'other')),
  message     TEXT NOT NULL,
  page_url    TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL DEFAULT 'new' CHECK (status IN ('new', 'reviewed', 'resolved', 'dismissed')),
  admin_notes TEXT NOT NULL DEFAULT '',
  created_at  DATETIME NOT NULL,
  updated_at  DATETIME NOT NULL
);
CREATE INDEX feedback_status ON feedback (status, created_at);

-- An admin's "log in as" session for another user; "Return to admin" goes back to this user.
ALTER TABLE sessions ADD COLUMN impersonator_id INTEGER REFERENCES users (id) ON DELETE CASCADE;
