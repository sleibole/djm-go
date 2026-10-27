# DJMagic (Go rewrite) — Plan

A rewrite of `djm-mono` (two Rails apps plus a shared JWT gem) as one Go web app. The product stays the same: KJs and DJs upload a song catalog, start a show, and the audience scans a QR code to search the catalog, request a song, and see their place in line.

This document is the working plan. It covers what we are building, how it is built, and the order we build it in. Structure and conventions follow `track-anything` (see `../track-anything/docs/development/ARCHITECTURE.md`).

---

## Goals

- **One app.** One Go binary, one process, one application SQLite database. No songs service, no JWTs between services, no shards, no CORS.
- **Simple code.** Standard library first. No web framework, no ORM-heavy patterns, no job runner, no frontend framework. Flat `main` package until it hurts.
- **Tigris is the source of truth for song data.** Uploaded CSVs and built catalog databases live in Tigris. Local disk holds a cache of catalog databases that can be deleted at any time.
- **Cheap to run now, easy to move later.** Start on a single machine under Dokku. The same image runs on Fly.io later. Because local catalog files are only a cache, the 500 GB Fly volume limit stops mattering: the cache has a size budget and evicts.
- **Feature parity with what is live today**, then the roadmap items the Rails app never got to.

## Non-goals (for now)

- Multiple app instances or read replicas. SQLite means exactly one writer and one instance.
- A public API.
- Billing, ads, metrics dashboards, the Fly fleet admin view.
- Native apps. The audience experience is mobile web.

---

## What carries over from the Rails app

This is the feature inventory from `djm-mono/core` and `djm-mono/songs`, with what we keep, change, or drop.

### Keep

| Area | Feature |
| --- | --- |
| Accounts | One email form for sign up and log in. Magic link (single use, hashed, short-lived). Optional password set from Account. Password login locks after 10 failures; a magic link unlocks. Rate limits per email and per IP. Turnstile on email and password forms. |
| Accounts | Roles: `dj`, `audience`, `admin`. Public handle (slug) and display name. Account deletion request. |
| DJ defaults | Default show type (`karaoke` / `dj`), default rotation style, default max songs per singer. |
| Catalogs | Named catalogs per DJ. Upload a CSV to replace a catalog. Background build into an FTS5 SQLite database. Versioned builds with an atomic swap. Status (`pending`, `processing`, `ready`, `failed`) with structured errors. Delete a catalog, blocked while an active show uses it. `variant_display` (`none` / `version` / `id`). |
| CSV rules | Ported exactly: encoding detection, header rules, row rules, error types and limits, grouping rows into songs, the catalog database schema. See "Catalog CSV rules" below. Fixtures in `djm-mono/fixtures/csvs/` come over as test data. |
| Search | Ported exactly: token sanitizing, prefix matching, the three sort modes, artist-match buckets, paging. See "Search" under "Catalog CSV rules". |
| Shows | Name, slug, type, status (`scheduled`, `active`, `ended`), optional catalog, `approval_required`, `manual_entry_enabled`, `max_songs_per_singer`, `rotation_style`. Schedule ahead, start, end. |
| Queue | Song requests with statuses `pending`, `queued`, `now_playing`, `completed`, `skipped`, `rejected`, `canceled`. One `now_playing` per show (partial unique index). Host can add from search or manually, approve, reject with reason, start, mark done, skip, move, remove. Participant autocomplete from this show's singers. |
| Audience | `/dj/{handle}` profile, show page with search and request, queue page ("You are #N"), TV display page, QR code for the show. Guest identity from a cookie, upgraded when they sign in. |
| Song memory | My Performances (completed requests across shows), Sing Again, Saved Songs with request-from-saved. Saved songs keyed by a normalized fingerprint, not a catalog row id. |
| Marketing | Home landing page with the singer-view and KJ-display screenshots. Open Graph and Twitter card tags with the 1200×630 image. |
| Look | Pico, light/dark/system theme switch, the TV display page always dark, the brand mark in the nav. See "Look and feel". |
| Other | Feedback form with an admin list. Admin user list and "log in as". Docs pages (CSV format help). Health check. |

### Change

| Rails | Go rewrite | Why |
| --- | --- | --- |
| `core` + `songs` apps, JWT, CORS, shards | One app | The split existed to shard catalog storage. Tigris plus a bounded local cache solves the same problem without a second service. |
| Browser calls songs app directly for search | Server-rendered search results via HTMX | One origin, no tokens, no JSON API to keep in sync. |
| Solid Queue `BuildCatalogJob` | One goroutine that builds catalogs, fed by a channel; pending builds are re-queued on startup from the database | No job runner. Build state already lives in the `catalogs` table. |
| LiteFS + Consul for app DB replication | Litestream continuous backup to Tigris (see "Backups" below) | We run one instance. We need backup and restore, not replication. |
| Local catalog DBs downloaded on boot (warm all) | Downloaded on first use, evicted by LRU under a size budget | Disk use is bounded no matter how many catalogs exist. |
| Turbo + Stimulus | HTMX + a little vanilla JS | Same approach as track-anything. Forms work without JS. |
| Font Awesome kit from a CDN (one icon, `fa-sparkles`, is from the Pro set) | Tabler SVGs, vendored and inlined, as in track-anything | No third-party script on every page, icons follow `currentColor` in dark mode, no kit account to keep. |
| About 41 KB of custom CSS on top of Pico | Small `app.css`, Pico elements first | track-anything's rule. Port only the styles a screen still needs. |
| Polling via Turbo refresh | HTMX polling first; Server-Sent Events later | SSE is easy in a single process (in-memory fan-out per show). |

### Drop (or defer)

- Songs sharding, `songs_shard`, `SONGS_SHARD_N_URL`, and shard admin tooling.
- Fly Machines fleet view and per-VM catalog status pages. Replaced by one admin page that shows cache and Tigris state for this machine.
- Prometheus metrics and Grafana. Deferred, same as track-anything.
- SES SNS bounce/complaint webhook. Deferred; keep the `email_status` idea for later.
- Demo mode. Deferred until there is a marketing need.

---

## Suggested improvements

These are changes beyond a straight port. Each one is a suggestion to accept or reject.

1. **Litestream instead of LiteFS for backups.** LiteFS replicates SQLite between nodes and needs a lease (Consul on Fly). It does not back up to S3 by itself; that was LiteFS Cloud, which Fly shut down in 2024. Litestream does exactly "continuously stream SQLite to S3-compatible storage and restore it on boot", and Tigris is S3-compatible. If we ever need LiteFS replication on Fly, it can be added then.
2. **Bounded local catalog cache.** `CATALOG_CACHE_MAX_BYTES` caps local disk. Least-recently-searched catalog files are deleted when over budget. The active version of any catalog used by an active show is never evicted. This is what lets us move to Fly without the volume limit being a problem.
3. **Rotation style is one function.** The queue is always an ordered list of requests. A rotation style only decides where a new or re-entering request is inserted (`insertPosition(show, participant)`). Standard rotation and traditional queue become two small functions instead of two code paths, and singer groups later become "insert next to my group".
4. **Search keeps working during a rebuild.** In Rails, search refused any catalog whose status was not `ready`, so a rebuild (or a failed one) made the old version unsearchable even though it was still on disk. Here search uses `active_version` whenever one exists, regardless of build status.
5. **Enforce the limits.** The 1M rows per catalog, 100 catalogs per account, 10M rows per account, and 5 uploads per hour were documented in Rails but not enforced. Enforce them here (details under "Catalog CSV rules").
6. **Skip identical uploads.** Store a sha256 of each uploaded CSV. Re-uploading the same file returns the current version instead of rebuilding.
7. **Cache-friendly search.** Search URLs include the catalog version (`/catalogs/{id}/v{n}/search?q=`). Responses can carry long cache headers because a rebuild changes the URL. Useful if a CDN goes in front later; harmless until then.
8. **Wait estimate from real data.** Estimated wait = position × median of `completed_at - started_at` for this show, falling back to 4 minutes until there are a few completed songs.
9. **"You're up next!" copy.** Position 1 says "You're up next!", now playing says "You're up!", others say "You are #N of M". This was an open TODO.
10. **One active show per DJ.** Enforce with a partial unique index (`user_id` where `status = 'active'`). Also an open TODO.
11. **Audience vs DJ accounts.** Signup keeps the Rails role choice, but with no default: two radio buttons, neither selected, and the server rejects a signup without one (in the Rails beta most audience members left the preselected "DJ/KJ" alone). Links from audience pages can pre-select with `/signup?role=audience`. An account created from the sign-in page, with no choice made, is `audience`. Audience accounts get a "Start hosting" button on Account that upgrades to `dj`. New DJs get a generated `dj-xxxxxx` handle, as Rails' `ensure_slug!` did, which they can change.
12. **Stale guest cleanup.** The in-process cleanup goroutine (same one that deletes expired sessions and tokens) deletes guest profiles with no account and no activity in 14 days, and their saved songs. Completed requests keep their display name for the DJ's history.

---

## Architecture

### Stack

| Concern | Choice | Notes |
| --- | --- | --- |
| Language | Go (latest, matching track-anything) | |
| HTTP | `net/http` `ServeMux` with method and wildcard patterns | No router library |
| Templates | `html/template`, layout + per-page `content` block | Same `render.go` approach as track-anything |
| Assets | `embed` | Single binary |
| Frontend | HTMX (vendored), small vanilla JS | No build step |
| CSS | Pico 2.1.1, full build (`pico.min.css`), vendored | 2.1.1 is the latest release, and what both djm-mono and track-anything use. The Rails README says "classless", but both apps load the full build; keep the full build so `role="group"`, `.container`, `.outline`, `.secondary` work |
| Icons | Tabler, individual SVGs vendored in `icons/` and inlined by an `icon` template func | Same as track-anything |
| App database | SQLite, WAL, one file | `DB_PATH`, default `data/djm.db` |
| SQLite driver | `github.com/glebarez/sqlite` (pure Go, built on `modernc.org/sqlite`, includes FTS5) | No CGO. Same as track-anything |
| Data access | GORM for the app database; raw SQL through GORM (`db.Raw`, `db.Exec`) where GORM gets in the way | Raw SQL for: partial unique indexes, queue renumbering, and everything in catalog databases (FTS5, bm25, aggregation) |
| Schema | Numbered SQL files in `migrations/` (`001_accounts.sql`, ...), embedded, run on startup by a small runner in `migrate.go`. SQLite's `PRAGMA user_version` records the last one applied | Hand-rolled instead of Goose or `AutoMigrate` (see Decisions). GORM only maps rows; it never changes the schema |
| Catalog databases | Opened with plain `database/sql` and the same driver, not GORM | They are built once and read-only after that; FTS5 queries are raw SQL anyway |
| CSV parsing | Our own reader (`csv_read.go`), a port of Ruby CSV's `liberal_parsing` | `encoding/csv` differs from Ruby on blank lines, stray quotes, and quoted vs unquoted empty fields, all of which change results. A test compares the reader with Ruby on 3,713 generated inputs (apart from mixed line endings, which we accept) |
| Legacy encodings | `golang.org/x/text/encoding/charmap` (Windows-1252, ISO-8859-1) | |
| Object storage | Tigris via an S3 client (`aws-sdk-go-v2/service/s3` or `minio-go`) behind a small `blobStore` interface | Local-directory implementation (`dirBlobStore`) for dev and tests, done in phase 2. The Tigris implementation comes with deployment in phase 6 |
| Backups | Litestream, replicating the app DB to Tigris | Catalog DBs are not backed up; they are already in Tigris |
| Email | `net/smtp` to Amazon SES SMTP | Console backend in dev. SMTP in dev requires `DEV_ALLOW_SMTP=true`. MailHog is that opt-in, on the personal server only. |
| Bot protection | Cloudflare Turnstile | Same as both existing apps |
| Passwords | `golang.org/x/crypto/bcrypt` | |
| QR codes | `rsc.io/qr` | Rendered as inline SVG in the host's share dialog and on the display page |
| Logging | `log/slog` | |
| Dev reload | Air, run by version | As in track-anything |

### Project layout

Flat `main` package, split by feature into files. Move to `internal/` packages only if navigation becomes painful.

```
djm/
├── PLAN.md
├── go.mod
├── main.go              # config, open DB, migrate, build mux, start server, start goroutines
├── config.go            # env parsing and validation (prod requires BASE_URL, Turnstile keys, Tigris creds)
├── env.go               # load optional .env
├── db.go                # open app DB (GORM), pragmas, run migrations
├── migrate.go           # migration runner (user_version, one transaction per file)
├── migrations/          # 001_accounts.sql, ... never edited after they ship
├── models.go            # GORM structs
├── auth.go              # email form, magic links, password, lockout, sessions
├── turnstile.go
├── mail.go
├── ratelimit.go
├── middleware.go        # logging, recovery, security headers
├── render.go
├── blobstore.go         # interface + local-dir implementation (Tigris in phase 6)
├── csv_read.go          # CSV reader matching Ruby's liberal_parsing
├── csv_import.go        # encoding, header and row rules, U+FFFD stats, row limit, build SQLite+FTS5
├── catalog_build.go     # builder goroutine: fetch CSV, import, publish, swap active version
├── catalog_cache.go     # local file cache: fetch on miss, LRU eviction, one read-only handle per file
├── catalog_search.go    # token sanitizing, sort modes, artist buckets, paging
├── catalogs.go          # catalog pages: list, create, upload, status, search, settings, delete
├── fingerprint.go       # saved-song fingerprint
├── shows.go             # shows list, new show, show page, settings, start, end, cache pins
├── queue.go             # request lifecycle, rotation insert, positions, host add, autocomplete
├── account.go           # Account page: password, profile, defaults, start hosting, delete
├── audience.go          # guest identity, /dj/{handle}, show page, request, cancel, queue, display
├── qr.go                # QR code as inline SVG
├── memory.go            # guest linking on sign-in, saved songs, my performances, sing again
├── admin.go             # admin home, users, roles, log in as
├── feedback.go          # feedback form and admin triage
├── cleanup.go           # expired sessions/tokens, stale guests, old local versions
├── static.go            # content hashes for /static URLs, cache headers
├── icons.go             # embedded Tabler SVGs + `icon` template func
├── templates/
├── icons/               # Tabler SVGs we use (MIT, LICENSE included)
├── static/
│   ├── pico.min.css     # Pico 2.1.1 (copied in)
│   ├── app.css
│   ├── htmx.min.js
│   ├── app.js           # theme switch, dialogs, local times, polling guard, copy link
│   ├── reorder.js       # queue drag and drop (host show page)
│   ├── manifest.webmanifest
│   ├── favicon.ico, icon.png, apple-touch-icon.png   # copied from djm-mono
│   └── images/
│       ├── brand/       # dark_64.png, dark_192.png, open_graph_1200_630.png (copied)
│       └── landing/     # queue/{singer_view,kj_display}.png (copied)
├── testdata/
│   ├── golden.rb        # runs the Rails services over the fixtures; writes the expected results
│   ├── csvs/            # Rails fixtures + edge cases, each with the .json Rails produced
│   ├── search/          # search catalog, queries, and Rails' results
│   └── csvparse.json    # generated inputs and what Ruby's CSV parser made of them
├── Dockerfile
├── entrypoint.sh        # litestream restore, then litestream replicate -exec /djm
├── litestream.yml
├── app.json             # Dokku health check
└── Makefile
```

### Data model (app database)

Ported from `core/db/schema.rb`, simplified. Timestamps are UTC.

- **users** — `email` (unique, lowercased), `role` (`audience`/`dj`/`admin`), `slug` (unique, nullable until hosting), `display_name`, `password_hash`, `email_confirmed_at`, `failed_login_attempts`, `locked_at`, `default_show_type`, `default_rotation_style`, `default_max_songs_per_singer`. Deleting an account removes it right away, so there is no `deletion_requested_at`.
- **sessions** — id is sha256 of the cookie token, `user_id`, `expires_at`, `impersonator_id` (the admin, during "log in as").
- **magic_tokens** — `user_id`, `token_hash` (unique), `expires_at`, `consumed_at`.
- **catalogs** — `user_id`, `name` (unique per user), `variant_display`, `status` (`empty` until the first upload, then `pending`, `processing`, `ready`, `failed`), `active_version`, `building_version`, `song_count`, `row_count`, `errors_json`, `import_metadata_json` (source encoding, U+FFFD counts), `last_csv_sha256`. Deleting a catalog removes the row and its files right away.
- **catalog_versions** — `catalog_id`, `version`, `status`, `csv_sha256`, `csv_bytes`, `source_encoding`, `row_count`, `song_count`, `db_bytes`, `created_at`. One row per upload; blob keys are derived from catalog and version. Lets cleanup find old files, and gives admin a history.
- **shows** — `user_id`, `catalog_id` (nullable), `name`, `slug` (unique per user), `show_type`, `status`, `scheduled_at`, `started_at`, `ended_at`, `rotation_style`, `approval_required`, `manual_entry_enabled`, `max_songs_per_singer`. Partial unique index: one `active` show per user.
- **audience_profiles** — `guest_token_hash` (unique, nullable), `user_id` (unique, nullable), `last_seen_at`.
- **show_participants** — `show_id`, `audience_profile_id` (unique together), `display_name`.
- **song_requests** — `show_id`, `show_participant_id`, `song_title`, `song_artist`, `song_version`, `song_external_id`, `request_source` (`audience`, `host_manual`, `host_catalog`, `saved`), `status`, `queue_position`, `requested_at`, `started_at`, `completed_at`, `rejected_at`, `rejection_reason`, `canceled_at`. Partial unique index: one `now_playing` per show.
- **saved_songs** — `audience_profile_id`, `fingerprint` (unique together), song fields, `source_catalog_id`, `request_count`, `last_requested_at`.
- **feedback** — `user_id` (nullable), `category`, `message`, `page_url`, `status`, `admin_notes`.

### Catalog CSV rules (ported from the Rails songs app)

These rules took real work to settle and are ported as they behave in the Rails code (`songs/app/services/catalog_csv_encoding.rb`, `csv_validator.rb`, `catalog_db_builder.rb`, `catalog_search_service.rb`). Where the Rails docs (`docs/csv-upload.md`, `docs/song-search.md`) disagree with that code, the code wins; the differences are noted. Every rule here gets a test, and the Rails fixtures in `djm-mono/fixtures/csvs/` must give the same result in Go.

**Upload**

- One file per upload, multipart field `file`. No file: "No file provided".
- Maximum 100 MB. Larger: 413 with "File too large. Maximum size is 100MB." Enforced while streaming (`http.MaxBytesReader`), not after.
- Nothing about the contents is checked at upload time. Validation happens in the build.
- Limit: 5 uploads per user per hour (documented in Rails, not enforced there).

**Encoding**

1. If the file starts with the UTF-8 BOM (`EF BB BF`), skip those 3 bytes. The BOM must never end up in the first header name.
2. Scan the rest of the file in 64 KB chunks to check that it is valid UTF-8, carrying a partial multibyte sequence across chunk boundaries. Overlong forms, surrogate halves, and code points above U+10FFFF are invalid (Go's `utf8.Valid` rules match). Memory stays constant.
3. Valid: read as UTF-8. The source encoding is recorded as `UTF-8` or `UTF-8-BOM`.
4. Not valid: treat it as a Windows "ANSI" export and decode every byte as Windows-1252, which gets curly quotes, dashes, and the euro sign right. If the file contains any of the five bytes Windows-1252 leaves undefined (`0x81`, `0x8D`, `0x8F`, `0x90`, `0x9D`), decode as ISO-8859-1 instead. The same scan finds them. **Changed from Rails:** Ruby looked for them only in the first 64 KB, so a file with one further in crashed the build; Go checks the whole file. Also, Rails kept a BOM in the first header when the rest of the file was not UTF-8; Go always skips it.
5. ISO-8859-1 decodes every byte, so step 4 never fails. Rails' `encoding_error` ("We couldn't read this CSV file…") could not actually happen and is not ported.
6. The decoded stream is UTF-8 from here on. The source encoding is stored in `import_metadata_json`.

**Parsing**

Ruby's CSV with `liberal_parsing: true`, ported exactly in `csv_read.go`:

- Rows may have more or fewer fields than the header. A field missing from a short row reads as blank.
- Stray quotes inside an unquoted field are kept as text. Text right after a closing quote is kept too, with the quotes: `"Hey" Jude` reads as `"Hey" Jude`.
- An unquoted empty field is nil and a quoted one (`""`) is an empty string. That only matters for header cells (see Headers).
- CRLF, LF, and CR all end a row, in any mix. **Changed from Rails:** Ruby took the first line ending in the file and failed the upload on any other ("Unquoted fields do not allow new line…"). Every file Rails accepted reads the same. Newlines inside quoted fields are kept as they are.
- **A blank line is a row with no fields**, so a blank line between songs (or several at the end) fails with blank title and artist errors on that row, as in Rails (kept on purpose). A single newline at the end of the file is fine.
- Row numbers in messages count CSV records, not physical lines: the header is row 1 and the first data row is row 2. A quoted field with a newline in it does not shift later row numbers.
- A file that cannot be parsed fails with type `parse_error`: "Invalid CSV: {Ruby's message}", e.g. "Invalid CSV: Unclosed quoted field in line 4.". Rails let these escape as a generic job error, so the DJ never saw why; Go shows Ruby's message.

**Headers**

- The first record is the header. If the file is empty or every header cell is empty, it fails with one error, type `no_headers`: "CSV has no header row".
- A header with no data rows after it fails with type `no_rows`: "CSV has no songs". **Changed from Rails**, which said "CSV has no header row" here too.
- Each header cell is trimmed (ASCII whitespace and NUL, as Ruby's `strip`; not non-breaking spaces) and lowercased (Unicode, as Ruby's `downcase`). Column order does not matter.
- Recognized columns: `title`, `artist`, `version`, `album`, `id`. Any other column is ignored.
- Header errors, all collected in one pass:
  - `empty_header`, one per empty cell, with the 1-based column number: "Header cell {n} is empty". An empty cell anywhere is an error, including trailing empty cells from a spreadsheet export.
  - `duplicate_header`, one per duplicated normalized name: "Duplicate header '{name}'". `Title` and ` title ` are duplicates. Two cells that are quoted-empty or only spaces also give "Duplicate header ''"; unquoted empty cells do not.
  - `missing_header`, for `title` and for `artist`: "Required header '{name}' is missing".
- If any header error exists, stop. Rows are not checked.

**Rows**

- `title` and `artist` are trimmed and must not be empty. Each blank one is an error, type `blank_field`, with `row` and `column`: "Title is blank on row {n}" or "Artist is blank on row {n}". A row with both blank produces two errors.
- `version`, `album`, and `id` are trimmed. Blank (any Unicode whitespace) becomes NULL. They are always optional.
- Stop collecting row errors after 20 (a row with two blanks can bring the total to 21, as in Rails).
- Any row error fails the whole file. A catalog never goes live with some rows silently dropped.
- Limits, new in Go (Rails documented them but did not enforce them), both type `too_many_rows`:
  - More than 1,000,000 data rows: "This file has more than 1,000,000 rows, the most one catalog can hold."
  - 10,000,000 rows per account, counting the other catalogs' active versions plus this file: "This file would put your account over 10,000,000 rows. Your other catalogs have {n}." Whichever limit is lower applies.
  - 100 catalogs per account is checked when creating a catalog.
- If the build fails for a reason that isn't the file's fault (disk, storage), type `build_error`: "Something went wrong while processing this file. Please try uploading it again." The details go to the log.

**Corrupted characters (U+FFFD)**

- While validating, count rows that contain U+FFFD, and the number of those characters (counting stops at 10,000). These usually mean the file was damaged before it reached us; they do not fail the build.
- Store `has_replacement_characters`, `replacement_row_count`, and `replacement_character_count` in `import_metadata_json`.
- When the build finishes with any such rows, the catalog page shows a warning in place of the plain "Catalog is ready!": "**Some song titles have funny or incorrect characters ({n} songs affected).** Everything will still work — this just affects how some titles appear. To fix it, try uploading a fresh copy of your original song list."

**Error format**

`errors_json` is a list of `{type, detail}` objects, with `row`, `column` where they apply, exactly as Rails stored them. The catalog page lists every `detail`. (The Rails `[object Object]` display bug is avoided by rendering server-side.)

**Building the catalog database**

The Rails docs show one `songs` row per CSV row with `version` and `external_id` columns. The code does something better, and that is what we port: rows with the same title and artist, ignoring case, become one song that carries every version and every ID.

```sql
CREATE TABLE songs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  title TEXT NOT NULL,
  artist TEXT NOT NULL,
  album TEXT,
  versions_json TEXT,      -- JSON array of distinct versions, or NULL
  external_ids_json TEXT,  -- JSON array of distinct ids, or NULL
  versions_text TEXT       -- distinct versions joined with spaces, for FTS
);

CREATE VIRTUAL TABLE songs_fts USING fts5(
  title,
  artist,
  versions_text,
  content=songs,
  content_rowid=id,
  tokenize="unicode61 remove_diacritics 2",
  prefix='2 3'
);
```

1. Validation and building are one streaming pass: each valid row is inserted into a scratch table `raw_songs(title, artist, album, version, external_id, lower_title, lower_artist)` inside one transaction with a prepared insert. At the first row error inserting stops and only errors are collected; the temp file is thrown away. `lower_*` are lowercased in Go to match Ruby's Unicode `downcase` (`strings.ToLower`, except `İ`, which Ruby lowercases to `i̇`).
2. Index `raw_songs(lower_title, lower_artist)`.
3. Group into `songs` with one `INSERT … SELECT`, the same SQL as Rails:
   - Group key: `lower_title`, `lower_artist`.
   - `title`, `artist`, `album`: `MIN()` of each. That picks the first spelling in byte order (so `Queen` beats `queen`) and the first non-NULL album.
   - `versions_json`: `json_group_array` over distinct non-NULL versions. `versions_text`: the same versions joined with spaces.
   - `external_ids_json`: `json_group_array` over distinct non-NULL IDs.
4. Drop `raw_songs`, then fill the FTS index with `INSERT INTO songs_fts(songs_fts) VALUES('rebuild')`.
5. `song_count` is the number of grouped songs. `row_count` (data rows in the CSV) is stored separately and is what the row limits count.
6. Run `PRAGMA quick_check`. Build into `{path}.tmp` and rename when finished; delete the temp file on any failure.

The build uses `journal_mode=OFF` and `synchronous=OFF`, since a failed build is thrown away anyway.

**Search**

- Input: `q`, `limit` (clamped to 1–100; the DJ page asks for 50), `offset` (default 0, at least 0), `sort` (`best_match` default, `title`, `artist`, trimmed and lowercased; anything else means `best_match`). Rails turned a missing `limit` into 1 result; every Rails page sent one, so this never showed.
- Turning `q` into an FTS5 query:
  - Unicode NFC normalize (`golang.org/x/text/unicode/norm`), then split on ASCII whitespace.
  - Split each piece into runs of characters that are legal in FTS5 barewords: ASCII letters, digits, `_`, and any code point above U+007F. Every other character ends a run, so `Mr.` gives `Mr` and `AC/DC` gives `AC` and `DC`.
  - Drop `AND`, `OR`, `NOT`, `NEAR` (any case). Keep at most 20 tokens.
  - Each token becomes a prefix term (`token*`), joined with spaces. Single characters too, so `Mr. B` finds "Mr. Brightside".
  - No tokens left: no results, and no query is run.
  - The MATCH string is always a bound parameter.
- `total` is `SELECT COUNT(*) FROM songs_fts WHERE songs_fts MATCH ?`.
- Ordering:
  - `best_match`: an artist-match bucket first, using the raw query lowercased and trimmed with whitespace collapsed: exact artist match 0, artist starts with the query 1, artist contains the query 2, anything else 3. Then title (case-insensitive), artist (case-insensitive). The bucket uses SQLite `LOWER()`, which only folds ASCII; ported as is.
  - `title`: title, artist (both case-insensitive).
  - `artist`: artist, title (both case-insensitive).
  - `ORDER BY` clauses are fixed strings chosen by the sort mode, never built from input.
  - **Dropped from Rails: bm25 and `id` as the last tie-breakers.** They never decide anything. Two songs can only tie if their titles and artists are equal ignoring ASCII case, and then they are also equal after full lowercasing, so the build already merged them into one song. Results are identical (the golden search tests pass), and not computing bm25 makes searches on large catalogs about 4× faster. The Rails docs describe ranking by bm25; the code never really did.
- Each result: `id`, `title`, `artist`, `album`, `versions` (array), `external_ids` (array). No highlighting; the Rails docs mention `*_highlighted` fields the code never returned.
- Searches slower than 500 ms are logged with sort mode, limit, offset, and result count, but not the query.

**Versions and IDs on requests (`variant_display`)**

- `none`: no version or ID column, nothing stored beyond title and artist.
- `version`: a Version column appears in results only when at least one result has versions. A song with versions gets a picker, defaulting to the first; the choice is saved as `song_version` on the request.
- `id`: the same, using `external_ids`, saved as `song_external_id`.
- This applies on the DJ catalog page, the host add-song search, and the audience search.

**Saved-song fingerprint**

- Normalize a value by trimming, lowercasing, and collapsing whitespace runs to one space.
- If the song has an external ID: `ext:{trimmed id}`.
- Otherwise: `norm:{title}|{artist}|{version}`, with blank artist or version left empty.

### Catalog databases and the local cache

Each catalog version is its own SQLite file with the schema above.

**Tigris layout**

```
catalogs/{catalog_id}/v{version}.csv   # the upload, source of truth
catalogs/{catalog_id}/v{version}.db    # built artifact
```

**Build (one goroutine)**

1. The upload handler streams the multipart file to a temp file (size limit 100 MB, enforced while reading), computing sha256 as it goes. If the hash matches `last_csv_sha256` and the catalog is ready, it is discarded: "That file is the same as the one already live, so nothing changed."
2. In a transaction, allocate version `n` and insert its `catalog_versions` row. Store the CSV as `v{n}.csv`, then set catalog `building_version = n`, `status = pending`, and wake the builder.
3. The work queue is the catalogs table: the builder takes the oldest `pending` catalog and sets it `processing`. It downloads the CSV to a temp file and builds `v{n}.db` directly in the cache directory (per "Catalog CSV rules" above), then stores the DB as `v{n}.db`.
4. In one transaction, only if `building_version` is still `n`: `active_version = n`, `status = ready`, `song_count`, `row_count`, `import_metadata_json`, `errors_json` cleared. Searches switch on the next request. The old version stays until cleanup. If a newer upload or a delete happened during the build, this build's result is dropped.
5. On failure: `status = failed`, `errors_json` set, `active_version` unchanged, so search keeps serving the previous version.
6. Only one process builds at a time: the builder holds an exclusive `flock` on `.builder.lock` in the cache directory, waiting for it during a zero-downtime deploy until the old container exits. Once it has the lock, any catalog left `processing` goes back to `pending`. Besides being woken by uploads, the builder looks for pending work every 30 seconds, so a catalog left pending by another process doesn't wait for the next upload.

**Cache**

- Directory `CATALOG_CACHE_DIR` (default `data/catalogs/`), files named `{catalog_id}_v{version}.db`.
- On search: if the file is missing, download from Tigris (one download per file at a time; other searches for it wait), then open it read-only (`mode=ro`, `immutable=1`).
- One open read-only handle per cached file, closed when evicted.
- Track last use in memory, and touch the file mtime (at most once a minute) so it survives restarts. On startup the directory is scanned. Leftover `.tmp` files are deleted by the builder once nothing has written them for 10 minutes; startup can't delete them all, because during a deploy the other container may still be writing them.
- When total bytes exceed `CATALOG_CACHE_MAX_BYTES` (default 10 GB), delete least-recently-used files. Catalogs of live shows are pinned and never evicted (any version, so a host's search results from just before a rebuild still resolve). Pins are refreshed at startup and whenever a show starts or ends.
- Admin page shows: cached files and bytes, budget, hits/misses since boot, catalogs whose active version is missing from Tigris.

**Remote cleanup.** The hourly cleanup deletes the files and row of any version that is neither active nor building and is older than 48 hours. Deleting a catalog (or the account) deletes all its files right away.

### Queue

- The live queue is `song_requests` with `status = queued`, ordered by `queue_position`. Positions are rewritten densely in the same transaction as any change.
- `pending` requests (approval on) have no position until approved.
- Starting a request marks any current `now_playing` as `completed`, then sets this one `now_playing`.
- `max_songs_per_singer` counts that participant's `pending` + `queued`, including requests the host added.
- Ending a show marks the `now_playing` request `completed` and cancels everything `pending` or `queued`, in one transaction with the status change.
- Rotation style is a function that returns the insert position:
  - `standard` (launch): new requests go to the bottom.
  - `traditional` (later): same as standard for new requests; differs only in what happens after a singer finishes.
  - Groups and fairness rules (later) live in these functions.
- Host actions are POSTs that redirect back to the show page; HTMX swaps the live part of the page in place. A refused action (409) shows Rails' message above the queue.

### Real-time updates

Phase 1: HTMX polling on the host show page, audience queue page, and TV display (every 5 seconds, paused when the tab is hidden).

Later: a per-show in-memory broadcaster. Any queue change publishes the show id; `GET /shows/{id}/events` (SSE) tells the page to refetch its queue fragment. Polling remains the fallback. Web Push for "You're up next!" builds on the same hook.

### Routes (first pass)

```
GET  /                              landing (logged out) or dashboard
GET  /signin, GET /signup          email form; signup adds the role choice
POST /signin/link                   email a magic link (creates the account if new)
POST /signin                        password login
GET  /auth/{token}, POST /auth/{token}   confirm page, then redeem
POST /logout
GET  /account
POST /account/password, /account/password/delete, /account/profile,
     /account/defaults, /account/host, /account/delete

GET  /catalogs                      list with status
GET  /catalogs/new, POST /catalogs  create
GET  /catalogs/{id}                 status, errors, song count, DJ search (?q= works without JS)
POST /catalogs/{id}                 variant display
POST /catalogs/{id}/upload          multipart CSV
GET  /catalogs/{id}/status          polled while building; HX-Redirect when done
GET  /catalogs/{id}/search          DJ search fragment
POST /catalogs/{id}/delete
GET  /docs/csv-format               public CSV guide

GET  /shows, POST /shows
GET  /shows/new
GET  /shows/{id}                    host view: pending, now playing, queue, completed (?q= searches without JS)
POST /shows/{id}                    settings
POST /shows/{id}/start, /end
GET  /shows/{id}/live               polled queue fragment; HX-Redirect once the show isn't live
GET  /shows/{id}/search             Add to Queue search fragment
POST /shows/{id}/requests           host add (song={version}:{song id}, or title and artist)
POST /requests/{rid}/approve|reject|start|done|skip|delete
POST /requests/{rid}/move           after={rid} (empty = top); drag and drop, 204
GET  /shows/{id}/participants?participant_name=   autocomplete <option>s

GET  /dj/{handle}                         DJ profile, upcoming and live shows
GET  /dj/{handle}/shows/{slug}            audience show page (?q= searches, ?song= picks, without JS)
GET  /dj/{handle}/shows/{slug}/search     search fragment (?q=&v={version}&offset=); cacheable for the current version
POST /dj/{handle}/shows/{slug}/requests   request a song
POST /dj/{handle}/shows/{slug}/requests/{rid}/cancel   cancel your own pending or queued request
GET  /dj/{handle}/shows/{slug}/queue      queue and "where am I"; HX-Target audience-live returns the polled part
GET  /dj/{handle}/shows/{slug}/display    TV display; HX-Target display-live returns the polled part

GET  /me/performances, POST /me/performances/{rid}/request   (?show= / show= is the live show to request at)
GET  /me/saved, POST /me/saved, POST /me/saved/{id}/delete, POST /me/saved/{id}/request

GET  /feedback, POST /feedback

GET  /admin                         counts and catalog cache stats
GET  /admin/users, /admin/users/{id}
POST /admin/users/{id}/role, /admin/users/{id}/impersonate
POST /admin/return                  end "log in as"
GET  /admin/feedback (?status=), /admin/feedback/{id}, POST /admin/feedback/{id}

GET  /healthz
GET  /static/...
```

All state changes are POST. GETs never write (except touching cache files and `last_seen_at`), which keeps the door open for read replicas or CDN caching later.

### Look and feel

**Pico, the track-anything way.** See `../track-anything/templates/layout.html` and `docs/development/DESIGN.md`.

- One `layout.html`: `<header class="container"><nav>` with the brand mark on the left and account links on the right, then `<main class="container" id="content">` as the HTMX swap target. Variants for the audience page and the TV display are blocks in the same layout, not separate layouts.
- Classless first. Use Pico's elements (`nav`, `article` for cards, `table`, `details`, `[role="group"]`, `[aria-busy]`, `button.outline.secondary`) before adding classes. `app.css` holds only what Pico does not cover: queue row controls, the now-playing panel, the search results list, the TV display's large type, the landing hero sections.
- The UI color is the logo purple (`#7E29D8` in light, `#A78BFA` in dark), set with Pico custom properties in `app.css`. Surfaces stay white or soft neutral. Control boundaries stay darker than card and divider lines. The brand mark is the existing purple-and-white artwork.
- A small inline `<style>` sets the `html` background before Pico loads (`#FAF9FC`, or `#14111C` for dark) so pages do not flash. The browser `theme-color` meta follows that pair and the saved theme choice. The web manifest `theme_color` and `background_color` stay `#13171f`.
- HTMX config meta tag as in track-anything, so 4xx validation messages swap in place.

**Theme.** Port the Rails behavior: System / Light / Dark, chosen from a three-button control in the footer (so logged-out and audience visitors have it too) and on the Account page. The control is hidden until `app.js` runs, since it does nothing without JavaScript. The choice is saved in `localStorage` under `theme`. An inline script in `<head>` sets `data-theme` before the stylesheet paints; "System" removes the attribute and lets `prefers-color-scheme` decide. The TV display page is always `data-theme="dark"`. Turnstile renders with `theme: auto`. (track-anything has no switch; this is a DJM-specific addition carried over.)

**Images carried over.** Copied from `djm-mono/core` into `static/` unchanged. The fingerprinted copies under `public/assets/` were Rails build output and were not copied.

| File | Size | Used on |
| --- | --- | --- |
| `static/favicon.ico` | 16×16 (a PNG named `.ico`) | Every page |
| `static/icon.png` | 32×32 | Every page (`<link rel="icon">`), web manifest |
| `static/apple-touch-icon.png` | 180×180 | Every page |
| `static/images/brand/dark_64.png` | 64×64 | Brand mark in the nav |
| `static/images/brand/dark_192.png` | 192×192 | Not referenced in Rails views; use it as the 192 manifest icon |
| `static/images/brand/open_graph_1200_630.png` | 1200×630, 1.1 MB | `og:image` and `twitter:image` on every public page |
| `static/images/landing/queue/singer_view.png` | 1290×2796, 279 KB | Home landing, singer view screenshot |
| `static/images/landing/queue/kj_display.png` | 1516×850, 128 KB | Home landing, KJ display screenshot |

The paper landing page (`/landing/paper`, `bartop.png`, `paper_slip.png`) is no longer used and is not ported.

Image fixes to make while porting:

- **The manifest icons are wrong in Rails.** `manifest.json` lists `/icon.png` as 512×512 (and as the maskable icon), but the file is 32×32, so home-screen installs get a blurry icon. The manifest lists only `dark_192.png` as a 192×192 icon, with no maskable entry. Adding a 512 is a TODO under "Later". `theme_color` and `background_color` stay `#13171f`.
- **Heavy images.** `open_graph_1200_630.png` (1.1 MB) is far larger than it needs to be. Losslessly compress it (e.g. `oxipng`), and resize the screenshots to roughly twice their displayed width. Keep PNG for the Open Graph image, since some link-preview crawlers don't handle WebP.
- Every `<img>` gets `width`, `height`, and `alt` (the Rails alt text carries over), and below-the-fold images get `loading="lazy"`.
- Images are embedded in the binary with the rest of `static/`. Static responses get a long `Cache-Control` with a content hash in the URL (`/static/{hash}/...` or `?v={hash}`), since Rails' fingerprinting goes away.

**Icons.** Font Awesome icons map to Tabler like this. Copy in only these SVGs and add `aria-hidden="true"`, as track-anything does.

| Font Awesome | Tabler | | Font Awesome | Tabler |
| --- | --- | --- | --- | --- |
| `lock` | `lock` | | `rotate` | `refresh` |
| `user` | `user` | | `person-walking-arrow-right` | `walk` |
| `music` | `music` | | `paper-plane` | `send` |
| `flask` | `flask` | | `moon` | `moon` |
| `envelope` | `mail` | | `sun` | `sun` |
| `share-nodes` | `share` | | `mobile-screen-button` | `device-mobile` |
| `print` | `printer` | | `magnifying-glass` | `search` |
| `microphone` | `microphone` | | `list-check` | `list-check` |
| `link` | `link` | | `lightbulb` | `bulb` |
| `guitar` | `guitar-pick` | | `keyboard` | `keyboard` |
| `copy` | `copy` | | `folder-open` | `folder-open` |
| `circle-half-stroke` | `circle-half-2` | | `file-arrow-up` | `file-upload` |
| `bell` | `bell` | | `face-smile` | `mood-smile` |
| `wand-magic-sparkles` | `wand` | | `clock-rotate-left` | `history` |
| `sparkles` | `sparkles` | | `circle-check` | `circle-check` |
| `user-group` | `users-group` | | `check` | `check` |
| `tv` | `device-tv` | | `calendar` | `calendar` |
| `trash-can` | `trash` | | `bolt` | `bolt` |
| `shuffle` | `arrows-shuffle` | | `circle` (regular) | `circle` |
| `brands/reddit` | `brand-reddit` | | (new) drag handle | `grip-vertical` |

### Security

Same rules as track-anything: hashed session and magic tokens, `HttpOnly; Secure; SameSite=Lax` cookies, `http.CrossOriginProtection` plus a session-bound CSRF token on unsafe requests (see `docs/development/CSRF.md`), `safeNext` redirects, security headers, trusted client IP header only when set explicitly. Never show an email on an audience-facing page. The guest cookie holds a random token; the database stores its hash.

### Configuration

| Variable | Purpose |
| --- | --- |
| `ADDR` / `PORT` | Listen address |
| `ENV` | `dev` or `prod` |
| `BASE_URL` | Public origin for emailed links and QR codes |
| `DB_PATH` | App SQLite file |
| `CATALOG_CACHE_DIR`, `CATALOG_CACHE_MAX_BYTES` | Local catalog cache |
| `BLOB_DIR` | Directory for the local blob store (default `data/blobs`) |
| `BLOB_BACKEND` | `local` (dev/tests) or `s3` (phase 6) |
| `S3_ENDPOINT`, `S3_BUCKET`, `S3_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | Tigris (phase 6) |
| `MAIL_BACKEND`, `DEV_ALLOW_SMTP`, `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS`, `MAIL_FROM` | Email. Production uses SES SMTP. Development uses the console unless `DEV_ALLOW_SMTP=true`. |
| `TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET_KEY` | Bot protection |
| `TRUSTED_IP_HEADER` | `Fly-Client-IP` in production; empty in dev. `CF-Connecting-IP` and `X-Forwarded-For` accepted for later setups |

---

## Deployment

### Now: homelab Dokku behind Fly.io ingress

The same setup as track-anything (see its ARCHITECTURE.md, "Deployment"). Fly.io provides the public IP and TLS and forwards requests to the homelab box over WireGuard. The router has no open ports. The app knows nothing about this.

```
Browser ──HTTPS──▶ Fly Proxy (sets Fly-Client-IP)
                     ▶ Fly nginx (overwrites Fly-Client-IP)
                       ──plain HTTP over WireGuard──▶ box:80 (Dokku nginx, routes by Host)
                                                        ▶ djm container (Go, plain HTTP on $PORT)
```

- No TLS in the Go server. `Secure` cookies still work because the browser sees HTTPS.
- `TRUSTED_IP_HEADER=Fly-Client-IP` for rate limits and Turnstile `remoteip`.
- The Host header must survive the chain, for Dokku routing and the CSRF origin check.
- Upload size: Fly nginx and Dokku nginx both need `client_max_body_size` of at least 100 MB (`dokku nginx:set djm client-max-body-size 101m`), and timeouts long enough for a 100 MB upload over a slow connection.
- Multi-stage Dockerfile: build the Go binary with `CGO_ENABLED=0`, copy it and the `litestream` binary into a small base image.
- Entrypoint: `litestream restore -if-db-not-exists -if-replica-exists $DB_PATH`, then `litestream replicate -exec /djm`. Litestream runs the app as a child process and replicates while it runs.
- `dokku storage:mount` a host directory at `/data` for `djm.db` and the catalog cache.
- Health check via `app.json` on `/healthz`.
- `git push dokku main` to deploy. Expect a few seconds of downtime per deploy (single instance, SQLite); acceptable for now. Avoid deploying during shows.
- Email through Amazon SES SMTP (the existing `djmagic.io` SES setup, SPF and DKIM, carries over).
- Tigris access is outbound HTTPS from the box. The catalog cache keeps that traffic low after the first search of each catalog.

### First-time setup on the box

```sh
dokku apps:create djm
dokku domains:set djm next.djmagic.io
sudo mkdir -p /var/lib/dokku/data/storage/djm
sudo chown 65532:65532 /var/lib/dokku/data/storage/djm   # the container runs as uid 65532
dokku storage:mount djm /var/lib/dokku/data/storage/djm:/data
dokku nginx:set djm client-max-body-size 101m
dokku config:set djm BASE_URL=https://next.djmagic.io TRUSTED_IP_HEADER=Fly-Client-IP \
  S3_BUCKET=<bucket> AWS_ACCESS_KEY_ID=<key> AWS_SECRET_ACCESS_KEY=<secret>
git remote add dokku dokku@<box>:djm
git push dokku main
```

On the Fly ingress app, route `next.djmagic.io` over WireGuard to the box, the same way track-anything's hostname is routed, and add the certificate (`fly certs add next.djmagic.io`).

Restore test: `dokku ps:stop djm`, move the database files out of the storage directory, `dokku ps:start djm`, and confirm the entrypoint restored the database from Tigris.

### Backups

- Litestream replicates the app DB to `s3://{bucket}/litestream/djm.db` on Tigris.
- Catalog CSVs and built DBs are already in Tigris and are not part of the backup.
- Before the first real show: restore the backup to a scratch machine and confirm the app boots with all users and shows. Repeat after any backup config change.

### Later: Fly.io

Same image. One machine, one volume for `djm.db` and the catalog cache, Litestream to the same Tigris bucket. The cache budget is set below the volume size, so the volume never needs to grow with the number of catalogs. Moving is: stop the Dokku app, let Litestream finish, start on Fly (restore runs on boot), point the Fly ingress at the new app instead of WireGuard. `TRUSTED_IP_HEADER` stays `Fly-Client-IP`.

---

## Testing

Standard library `testing` and `httptest`, as in track-anything. Each test gets a temp app DB and a local-directory blob store.

- **Golden files from the real Rails code.** `ruby testdata/golden.rb ../djm-mono` runs the Rails services over every CSV in `testdata/csvs/`, the search queries in `testdata/search/`, and 3,713 generated parser inputs, and writes what Rails produced as JSON. The Go tests compare against those files. Rerun it only when adding a case. It needs Ruby with the `activesupport`, `csv`, and `sqlite3` gems, not a Rails install.
- **CSV rules:** every fixture in `testdata/csvs/` gives the same pass/fail and the same error list as Rails. Plus: UTF-8 with and without BOM (no BOM in the first header); Windows-1252 curly quotes and euro sign; a file with an undefined Windows-1252 byte decodes as ISO-8859-1; a multibyte character split across a 64 KB chunk boundary is still valid UTF-8; trailing empty header cells; `Title` vs ` title ` duplicates; a header-only file; ragged rows; stray quotes; a quoted newline does not shift row numbers; the 20-row-error cap; the 1M row limit; U+FFFD row and character counts and the 10,000 cap.
- **Build:** rows that differ only in title or artist case become one song; `MIN()` picks the spelling and album; versions and IDs are distinct and complete; `versions_text` is searchable; `song_count` vs `row_count`; a failed build leaves no temp file.
- **Search:** token sanitizing (`Mr.`, `Mr. B`, `AC/DC`, reserved words, non-ASCII, empty input, 20-token cap); diacritics (`Beyonce` finds `Beyoncé`); each sort mode's order including the artist buckets; `limit` and `offset` clamping; an unknown sort falls back to `best_match`.
- **Other logic:** rotation insert positions; position renumbering; wait estimate; fingerprint normalization.
- **Catalog pipeline:** upload → build → search; a rebuild in progress and a failed rebuild both keep the old version searchable; over-100 MB upload is rejected while streaming; upload and account limits; identical upload is skipped; cache miss downloads from the blob store; eviction respects the budget and never evicts an active show's catalog; restart re-queues a pending build.
- **Handlers:** happy path and failures for every route; CSRF; logged-out redirect; a DJ cannot see or change another DJ's catalog, show, or request (404).
- **Flows:** sign in by magic link; create catalog and upload; create and start a show; audience requests as a guest, sees position, signs in, and keeps their history; host approves, starts, completes; My Performances lists it; Sing Again requests it in the next show.

---

## Phases

Phases 0–5 build the whole app and run it locally (`make dev`). Deployment waits until phase 6. Each phase ends usable locally, with tests passing.

### Phase 0 — Skeleton

**Status:** done.

- `main.go`, config, `.env`, middleware, render, `/healthz`.
- Layout with Pico 2.1.1 and HTMX, the theme switch, brand mark, favicons, manifest (with the fixed icon sizes), Open Graph tags, Tabler icons, cache-busted static URLs.
- App DB with GORM and pragmas. (Started on `AutoMigrate`; replaced by SQL migrations during phase 1.)
- `make dev` (Air autoreload).
- The Dockerfile, `entrypoint.sh`, `litestream.yml`, and `app.json` are written but untested; they are finished in phase 6.

### Phase 1 — Accounts

**Status:** done.

- Email form, magic link, sessions, optional password, lockout, rate limits, Turnstile, Account page, handle and display name, DJ defaults.
- **Done when:** a new user can sign in by email, set a password, and log in with it; ten bad passwords lock password login and a magic link unlocks it.

How it works (`auth.go`, `account.go`, `cleanup.go`; mail, Turnstile, and rate limiting are track-anything's code):

- **One email form.** Sign in and sign up both post to `/signin/link`. An existing account gets a sign-in link; a new email gets an account and a welcome link. The page says "check your email" either way, so the form doesn't reveal who has an account. (Rails told unknown emails "we couldn't find an account" on the sign-in page; that goes away because the same link now creates the account.)
- **Magic links** as in Rails: 10 minutes, single use, a new link replaces the old one, email copy ported. Opening the link shows a confirm page that posts itself, so mail scanners can't use it up. Redeeming confirms the email and clears any lockout.
- **Passwords:** 8–72 characters, set from Account once signed in, so the mailbox is already proven. No "current password" prompt; forgot password means "sign in by email, set a new one". Changing it signs out other devices. 10 wrong passwords lock password login until an email link is used.
- **Rate limits** (in memory): email links 5 per address and 10 per IP per hour (Rails' numbers; past the per-address limit the page looks the same but nothing is sent), password attempts 10 per IP per minute. Turnstile on both forms; development uses Cloudflare's always-pass keys.
- **Sessions:** random cookie token, sha256 in the `sessions` table, 30 days, renewed past halfway.
- **Cleanup goroutine** (hourly): expired sessions, used or expired links, and accounts whose email was never confirmed within 7 days (mistyped addresses).
- The first admin is still made by hand (`UPDATE users SET role='admin'`); after that, admins assign roles from `/admin/users` (phase 5).

### Phase 2 — Catalogs and search

- Blob store (local and Tigris), upload, encoding detection, validation, build goroutine, versions, status page with errors and the U+FFFD warning, limits, cache with eviction, search with sort modes and variant pickers, delete, CSV format doc page (ported from the Rails docs page).
- **Done when:** every Rails fixture gives the same result, a 1M-row CSV builds without memory growth, search answers in well under 100 ms on a warm cache, and deleting the local cache directory only costs one download on the next search.

**Status:** done, apart from the Tigris backend (phase 6).

How it works (`csv_read.go`, `csv_import.go`, `catalog_build.go`, `catalog_cache.go`, `catalog_search.go`, `catalogs.go`, `blobstore.go`):

- **Hosts only.** Catalog pages require the host role ("Start hosting from your account page…" otherwise). Another DJ's catalog is a 404.
- **Catalog page:** song count, status, search, and collapsible Upload, Settings, and Delete sections. While a build runs, the status polls every 2 seconds and the page reloads with "Catalog is ready!" (or the U+FFFD warning, or the error list) when it finishes. A failed upload keeps the previous version searchable.
- **Fidelity:** parsing, encoding, validation, grouping, and search ordering match the golden files from the Rails code. The places Go deliberately differs are marked "Changed from Rails" or "new in Go" in "Catalog CSV rules".
- **Measured** on a synthetic 1M-row CSV (`DJM_BIG=1 go test -run TestBigCatalog`; it repeats a small set of artist and title words, so queries match far more rows than a real catalog would): the build takes 36 s with about 7 MB peak heap, producing a 204 MB DB. Warm searches: "queen" 55 ms sorted by title and 120 ms best match; "l" 117–237 ms; "artist 42" 159–255 ms. A query matching every row ("key") is the worst case, about 1.4–1.7 s before bm25 was dropped (not re-measured since). Typical catalogs (tens of thousands of songs) should be far faster, but that hasn't been measured on real data yet. Search time grows with how many songs match, because sorting by title needs all of them.
- **Not yet:**
  - The Tigris blob store (phase 6); dev and tests use `BLOB_DIR`.

### Phase 3 — Shows and the host queue

- Shows CRUD, schedule, start, end, one active show per DJ. Host show page: pending, now playing, queue, completed. Add from search or manual entry, approve, reject, start, done, skip, move, remove. Participant autocomplete. Polling refresh.
- **Done when:** a KJ can run a full night from the host page.
- Decided before starting:
  - **Starting a song while another is playing** marks the current one completed and starts the new one in one click. (Rails refused with "Finish or skip … before starting another song.")
  - **Singer autocomplete** suggests singers already in this show, as Rails. Suggesting singers from past shows can come once guest identity exists (phase 4/5).
  - **Starting a second show** while one is active is refused, with a link to the active show. Creating a show without a schedule starts it, so this applies there too.
  - **Deleting an account with an active show** is refused: "End all active shows before deleting your account." (as Rails).
  - **Ended is final.** An ended show can't be started again; the host creates a new one. Ending a show asks for confirmation first. (Rails allowed restarting and ended without asking.)
  - **Drag and drop to reorder the queue**, replacing Rails' ↑/↓ buttons. Ported from track-anything's `static/reorder.js` and `order.go`:
    - Each queued row has a grip handle (`grip-vertical` icon), the only drag target, so the row's buttons still work. Pointer events, so mouse and touch both work; an 8 px threshold before a drag starts; auto-scroll near the screen edges; the row moves on screen immediately and the save happens in the background.
    - Without JavaScript the handles stay hidden. The host page already needs HTMX for polling.
    - **Change from track-anything:** it posts the whole list and refuses (409) if the list changed meanwhile. A live queue changes constantly as audience requests arrive, so here the client posts only the moved request and the request it now follows (`after`, empty for the top). The server places it there in the current queue, in one transaction with renumbering. If either request has left the queue (started, removed), the server answers 409 and the page reloads the queue.
    - Polling pauses during a drag and while the save is in flight, so a refresh can't snap the row back.

**Status:** done.

How it works (`shows.go`, `queue.go`, `migrations/003_shows.sql`, `templates/show*.html`, `static/reorder.js`):

- **Shows list** (`/shows`, "Shows" in the nav and "My Shows" on the dashboard): Live, Scheduled, and the 20 most recent ended shows with how many songs were performed.
- **New show:** name, slug, catalog, freeform requests (only shown with a catalog; a show without one always allows them), show type, approval, max songs, and Start Now or Schedule for Later. Defaults come from the DJ's Account defaults. A blank slug is made from the name as Rails did (`parameterize`, 30 characters, `-1`, `-2`… on collision); an unnamed show gets `show-xxxx`. The first show gives the DJ a handle if they don't have one.
- **Times** are stored in UTC and shown in the viewer's time zone by `app.js` (`<time data-local>`). The schedule field is a `datetime-local` input; `app.js` adds the same moment in UTC on submit. Without JavaScript the typed time is taken as UTC.
- **Show page:** the live region (pending requests with approve and a reject dialog with an optional reason, Now Playing with Done and Skip, the queue with drag handles, Start and Remove, and a collapsed Completed list) polls `/shows/{id}/live` every 5 seconds. Polling waits while the tab is hidden, during a drag, and while a dialog is open; open `<details>` stay open across refreshes. Below it: Add to Queue, Show Settings, and End Show (with a confirmation dialog). An ended show shows Songs Performed instead.
- **Add to Queue:** the singer's name (suggestions are this show's singers), then a catalog search whose results have Add buttons and the version or ID picker when the catalog uses one, or the manual title and artist fields. The Add button names the catalog version searched, so a rebuild in between can't swap in a different song ("That song is no longer in the catalog. Search again."). A name matching an existing singer in this show (ignoring case) reuses them; a new name gets an anonymous audience profile and participant, which phase 4/5 can link to a real guest later.
- **Host actions** use Rails' error messages, e.g. "Only the current song can be marked done." and "Alice already has the maximum number of songs queued." Requests for another DJ's show are a 404.
- **Catalogs and accounts:** deleting a catalog an active show uses is refused ("End all active shows before deleting this catalog."), as is deleting an account with an active show. Deleting a catalog deletes its past shows too, as in Rails; the delete dialog says so.
- **Not yet:**
  - Rotation styles other than standard.

### Phase 4 — Audience

- DJ profile page, audience show page with search and request, guest cookie identity, queue page with "You're up next!" wording and wait estimate, TV display page, QR code.
- **Done when:** a singer can scan, request, and watch their position with no account.
- Decided before starting:
  - **Duplicate names stay separate, as Rails.** Names aren't unique within a show. An audience request always uses that phone's own participant, so a guest named "Alice" and a host-added "Alice" are two singers with separate song limits. (A host add reuses an existing singer in the show with the same name; Go matches ignoring case, Rails matched exactly.)
  - **Audience members can cancel their own requests** while pending or queued, with a confirmation. (Rails: only the host could remove them.)
  - **Rate limit on audience requests**, in memory, per IP and show (about 10 per 10 minutes), on top of the per-singer song limit. No Turnstile, so requesting stays one tap.
  - **The request form fills in the name the guest last used at any show**, still editable. (Rails: only once they had requested in this show.)

**Status:** done.

How it works (`audience.go`, `qr.go`, `templates/dj_profile.html`, `audience_show.html`, `audience_queue.html`, `display.html`, `partials/queue_banner.html`):

- **Identity.** A guest gets a random token in an `HttpOnly` cookie (`guest`, 400 days) the first time they request a song, not when they only look; the database stores its sha256 in `audience_profiles`. A signed-in account uses a profile keyed by its user instead, with no cookie (phase 5 extended this from audience accounts to every role). An unknown cookie is cleared. `last_seen_at` is touched at most hourly.
- **DJ profile** (`/dj/{handle}`): Live Now with "Browse Songs & Request" (catalog) or "Make a Request", Upcoming, or an empty message. Unknown handle: "DJ Not Found". Unknown show: "Show Not Found" with a link back to the DJ.
- **Request page.** With a catalog: Step 1 searches as you type (HTMX, 25 results with Load more), Select picks a song (and version or ID when the catalog shows them), Step 2 asks for the name. Without JavaScript, the search box and Select buttons are plain GET forms, so the same steps work. Freeform: "Can't find your song?" under the search when the show allows it, or "Request a Song" when the show has no catalog. The name field fills in the name from this show, else the last name used at any show. At the song limit the form is replaced by Rails' message.
- **Search fragment** URLs carry the catalog version (`?v=`). The response is `Cache-Control: private, no-store` because the HTML includes the CSRF token.
- **Requesting** checks, in order: the show is live (Rails' scheduled and ended messages), a catalog pick or a title, the name (Rails' messages), then the rate limit (10 per IP per show per 10 minutes: "You're sending requests too quickly. Please wait a few minutes and try again."). In one transaction it finds or creates this profile's participant (renaming it if the name changed), checks the song limit, and creates the request (`pending` with approval, else queued at the bottom). It redirects to the queue page with Rails' notice.
- **Queue page** polls every 5 seconds while the show is live. It shows a callout ("You're up!", "You're up next!", or "You're in the queue!" with "You are number N of M." / "Your spots: #2, #5 of 9." and the wait estimate), Now Playing with "(That's you!)", the queue with the visitor's rows highlighted, Your Pending Requests, and Rejected Requests with the reason. Signed-out visitors get the "Create a free account" tip.
- **Cancel:** an X on the visitor's own pending or queued rows, with a confirmation. Anyone else's request is a 404; one that has moved on gives "This request can no longer be canceled."; an ended show gives "This show has ended."
- **Wait estimate:** position × the median start-to-done time of this show's completed songs, or 4 minutes per song until 3 are done.
- **Queue banner** on the DJ profile and request page: each live show where the visitor has a request waiting or on stage, linking to its queue.
- **TV display** (`.../display`, opened from the host page in a new tab): always dark, no nav, show name, QR code with "Scan to request a song", Now Playing, Up Next. Polls every 5 seconds.
- **Host page:** Share opens a dialog with the QR code, the public URL, Copy link, and Print (prints only the dialog); Display opens the TV page. A DJ without a handle sees "Set your handle" instead.
- **QR codes** are SVG from `rsc.io/qr` (medium error correction, white background, 4-module quiet zone), inline in the page.
- **Not yet:**
  - Live updates without polling (later: SSE).

### Phase 5 — Song memory and admin

- Guest-to-account upgrade, My Performances, Sing Again, Saved Songs and request-from-saved. Feedback form. Admin users, log in as, feedback, catalog cache status. Cleanup goroutine.
- **Done when:** feature parity with the Rails app (minus the dropped items above).
- Decided before starting:
  - **Song memory is for any signed-in account**, including a DJ who sings at someone else's show. (Rails: audience accounts only; a DJ was always an anonymous guest.) Signing in links the browser's guest history to the account, merging participants in the same show, in one transaction, as Rails' `LinkOrMerge`.
  - **Sing Again:** My Performances has a Request button on each past song while a show is live, using the same flow as requesting a saved song. (Not in Rails.)
  - **Hearts** (save a song) in audience search results and on My Performances. (Rails: search results only.)
  - **Log in as** shows a banner with "Return to admin", which switches back to the admin's own session. (Rails: sign out to end it.) Locked accounts and other admins can't be impersonated, as in Rails.
  - **Admin home** shows simple counts (users by role, live shows, requests today, new feedback) and the catalog cache stats, instead of Rails' signup charts and Fly fleet view.

**Status:** done.

How it works (`memory.go`, `fingerprint.go`, `admin.go`, `feedback.go`, `migrations/004_memory_admin.sql`, `templates/saved_songs.html`, `performances.html`, `feedback.html`, `admin*.html`, `partials/memory.html`, `partials/admin.html`):

- **Identity.** Any signed-in account (any role) uses a profile keyed by its user; guests keep the cookie profile from phase 4.
- **Linking on sign-in** (magic link or password): if the browser has a guest profile, it becomes the account's, or, when the account already has one, it's merged into it. In a show where both have a singer, the guest singer's requests move to the account's singer and the guest singer is deleted; saved songs move unless the account already saved the same song. One transaction; the guest cookie is cleared after it commits. A failure is logged and sign-in goes ahead, leaving the cookie to try next time.
- **Saved songs** are keyed by a fingerprint, as Rails: `ext:{external id}`, else `norm:{title}|{artist}|{version}` lowercased with whitespace collapsed. Saving twice keeps one row.
- **Hearts** (`partials/memory.html`) toggle in place with HTMX and work as plain forms without JavaScript. In audience search results a heart saves the catalog song with the version or ID chosen in the row, and shows as saved when any version of that title and artist is saved. Signed-in search results aren't cached, since hearts change. On My Performances a heart saves that exact past request.
- **Saved Songs** (`/me/saved`) and **My Performances** (`/me/performances`, completed songs, newest first) are linked from the nav for audience accounts, from the dashboard for everyone, and from a show's request and queue pages with `?show=`. With a live show in `?show=`, each row has a Request button that opens a name field (filled in as on the request page) and requests at that show, through the same checks as the request page plus "That show is not accepting requests right now." Requests from either page have `request_source = 'saved'`; saved songs count their requests. A signed-out visitor gets "Save your song history and favorite songs by creating a free account."
- **Catalog-only shows** only take remembered songs that are in their catalog, matching title and artist ignoring case ("That song isn't in this show's catalog."). Changed from Rails, which took any saved song.
- **Feedback** (`/feedback`, in the footer for everyone): category and message (up to 5,000 characters), with the page it was opened from (the Referer, kept only if it's this site). Signed-in senders are recorded. 5 per IP per hour. "Thanks for your feedback!" links back to that page.
- **Admin** (404 for everyone else): home counts and cache stats; users, newest first, searchable by email, handle, or name, 50 per page; a user page with details, role change, and Log in as; feedback filtered by status, with status and notes per item ("Feedback updated.").
- **Roles** follow Rails: "You cannot change your own role.", "Cannot remove the last admin.", and a new DJ or admin gets a generated handle.
- **Log in as** starts a session for the user that remembers the admin (`sessions.impersonator_id`), lasts at most an hour, and isn't renewed. A banner shows "Logged in as {email}" and "Return to admin", which starts a fresh admin session and goes back to that user's admin page. If the admin was demoted meanwhile, it signs out instead. Locked accounts and other admins are refused: "Cannot sign in as a locked account." / "Cannot sign in as another admin."
- **Cleanup** (hourly) also deletes guest profiles not seen for 14 days; their singers keep their names in show history. Deleting an account deletes its profile and saved songs.
- **Not yet:** nothing planned for this phase.

### Phase 6 — Deploy

Everything under "Deployment" above.

- Build and run the Docker image locally (`make docker-run`).
- Create the Tigris bucket and keys.
- Create the Dokku app (see "First-time setup on the box"), route `next.djmagic.io` through the Fly ingress, `git push dokku main`.
- Restore test, and the scratch-machine backup restore under "Backups".
- Run a real show on `next.djmagic.io`, then cut `djmagic.io` over from the Rails apps.
- **Done when:** a push deploys, `/healthz` is green, a Litestream restore onto a fresh container brings the DB back, and `djmagic.io` serves the Go app.

### Later (roadmap carried from `djm-mono/TODO.md`)

- SSE live updates, then Web Push "You're up next!" / "You're up!".
- Traditional queue rotation, singer groups, fairness enforcement.
- Change my song until I'm close to up (`docs/possible-features.md`).
- Song suggestions (add to catalog next time).
- Most Requested list per catalog (a songbook replacement).
- "Catalog" vs "Songbook" label per DJ.
- White-label show pages (DJ logo).
- CompuHost and other karaoke software integration research.
- Support email and About page; public "what's live" list.
- **TODO:** export a 512×512 brand icon (plus a maskable version with safe-zone padding) from the original artwork and add both to the manifest alongside the 192.
- Billing.

---

## Decisions

| Topic | Decision |
| --- | --- |
| Existing data | None to migrate. The Go app starts with an empty database. |
| Data access | GORM, with raw SQL where needed. Catalog databases use `database/sql`. |
| Hosting | Homelab Dokku behind Fly.io ingress over WireGuard, as track-anything. `TRUSTED_IP_HEADER=Fly-Client-IP`. |
| Email | Amazon SES over SMTP. |
| Limits | 100 MB per upload, 1M rows per catalog, 100 catalogs per account, 10M rows per account, 5 uploads per user per hour. Enforced. |
| CSV and search rules | Ported from the Rails code as described in "Catalog CSV rules". Deliberate changes: mixed line endings are accepted, a header-only file says "CSV has no songs", parse errors show Ruby's message, and the row limits are enforced. Blank lines still fail the upload, as in Rails. |
| Backups | Litestream to Tigris, not LiteFS. |
| Domain | The Go app deploys to a subdomain (e.g. `next.djmagic.io`) while the Rails apps keep `djmagic.io`. Cut over at the end of phase 6. |
| Migrations | Hand-rolled numbered SQL files, not `AutoMigrate` and not Goose. The schema is written down and reviewed; renames, backfills, and SQLite table rebuilds are explicit. One process and one database means the runner only needs `user_version` and a transaction per file. No down migrations: recover with a Litestream restore. Rules: never edit a shipped file; a table rebuild runs with foreign keys off and `PRAGMA foreign_key_check` before commit (the runner does both); `TestMigrationsMatchModels` fails if a GORM struct and its table disagree. |
| Order of work | Build the whole app locally (phases 0–5) before any deployment work (phase 6). |

## Open questions

None right now.
