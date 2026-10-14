CREATE TABLE users (
  id                           INTEGER PRIMARY KEY AUTOINCREMENT,
  email                        TEXT NOT NULL UNIQUE, -- stored lowercased
  role                         TEXT NOT NULL DEFAULT 'audience' CHECK (role IN ('audience', 'dj', 'admin')),
  slug                         TEXT UNIQUE,          -- public handle; NULL until they host
  display_name                 TEXT NOT NULL DEFAULT '',
  password_hash                TEXT NOT NULL DEFAULT '', -- empty if they only use magic links
  email_confirmed_at           DATETIME,
  failed_login_attempts        INTEGER NOT NULL DEFAULT 0,
  locked_at                    DATETIME,
  default_show_type            TEXT NOT NULL DEFAULT 'karaoke' CHECK (default_show_type IN ('karaoke', 'dj')),
  default_rotation_style       TEXT NOT NULL DEFAULT 'standard',
  default_max_songs_per_singer INTEGER,              -- NULL = no limit
  created_at                   DATETIME NOT NULL,
  updated_at                   DATETIME NOT NULL
);

CREATE TABLE sessions (
  id         TEXT PRIMARY KEY, -- sha256 hex of the cookie value
  user_id    INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  expires_at DATETIME NOT NULL,
  created_at DATETIME NOT NULL
);
CREATE INDEX sessions_user_id ON sessions (user_id);

CREATE TABLE magic_tokens (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id     INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  token_hash  TEXT NOT NULL UNIQUE, -- sha256 hex of the token in the emailed link
  expires_at  DATETIME NOT NULL,
  consumed_at DATETIME,
  created_at  DATETIME NOT NULL
);
CREATE INDEX magic_tokens_user_id ON magic_tokens (user_id);
