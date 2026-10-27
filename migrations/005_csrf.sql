-- Session-bound CSRF secrets. The cookie stores a random id; only the hash of
-- that id is kept here, same as sessions. The token column is the value forms
-- and scripts send back. Empty csrf_token on a session is filled on the next
-- request so a deploy does not sign everyone out.

ALTER TABLE sessions ADD COLUMN csrf_token TEXT NOT NULL DEFAULT '';

CREATE TABLE csrf_sessions (
  id         TEXT PRIMARY KEY, -- sha256 hex of the anonymous csrf cookie
  token      TEXT NOT NULL,
  expires_at DATETIME NOT NULL,
  created_at DATETIME NOT NULL
);
CREATE INDEX csrf_sessions_expires_at ON csrf_sessions (expires_at);
