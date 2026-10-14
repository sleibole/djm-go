CREATE TABLE catalogs (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id              INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
  name                 TEXT NOT NULL,
  variant_display      TEXT NOT NULL DEFAULT 'none' CHECK (variant_display IN ('none', 'version', 'id')),
  status               TEXT NOT NULL DEFAULT 'empty' CHECK (status IN ('empty', 'pending', 'processing', 'ready', 'failed')), -- of the latest upload
  active_version       INTEGER,                    -- the version searches use; NULL until one builds
  building_version     INTEGER,                    -- the upload waiting for or in the builder
  song_count           INTEGER NOT NULL DEFAULT 0, -- of the active version
  row_count            INTEGER NOT NULL DEFAULT 0, -- CSV data rows of the active version; what the row limits count
  errors_json          TEXT NOT NULL DEFAULT '',   -- why the latest upload failed
  import_metadata_json TEXT NOT NULL DEFAULT '',   -- encoding and U+FFFD counts of the active version
  last_csv_sha256      TEXT NOT NULL DEFAULT '',   -- of the active version's CSV, to skip identical uploads
  created_at           DATETIME NOT NULL,
  updated_at           DATETIME NOT NULL
);
CREATE UNIQUE INDEX catalogs_user_id_name ON catalogs (user_id, name);
CREATE INDEX catalogs_status ON catalogs (status);

-- One row per upload. Blob keys are derived from catalog_id and version.
CREATE TABLE catalog_versions (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  catalog_id      INTEGER NOT NULL REFERENCES catalogs (id) ON DELETE CASCADE,
  version         INTEGER NOT NULL,
  status          TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'ready', 'failed')),
  csv_sha256      TEXT NOT NULL,
  csv_bytes       INTEGER NOT NULL,
  source_encoding TEXT NOT NULL DEFAULT '',
  row_count       INTEGER NOT NULL DEFAULT 0,
  song_count      INTEGER NOT NULL DEFAULT 0,
  db_bytes        INTEGER NOT NULL DEFAULT 0,
  created_at      DATETIME NOT NULL
);
CREATE UNIQUE INDEX catalog_versions_catalog_id_version ON catalog_versions (catalog_id, version);
