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
| Catalogs | Named catalogs per DJ. Upload a CSV to replace a catalog. Background build into an FTS5 SQLite database. Versioned builds with an atomic swap, so search keeps working during a rebuild. Status (`pending`, `processing`, `ready`, `failed`) with structured errors (all header errors, up to 20 row errors). Delete a catalog, blocked while an active show uses it. `variant_display` (`none` / `version` / `id`). |
| CSV format | Required `title`, `artist`. Optional `version`, `album`, `id`. Case-insensitive, trimmed headers, any order, extra columns ignored. Reject duplicate or empty headers. Streaming parse. Fixtures in `djm-mono/fixtures/csvs/` come over as test data. |
| Search | FTS5 with `unicode61 remove_diacritics 2`, `prefix='2 3'`, bm25 weights title 10 / artist 5 / version 1. Bareword-safe token extraction (the `Mr.` bug fix), reserved words dropped, max 20 tokens, every token a prefix. Highlighted results. |
| Shows | Name, slug, type, status (`scheduled`, `active`, `ended`), optional catalog, `approval_required`, `manual_entry_enabled`, `max_songs_per_singer`, `rotation_style`. Schedule ahead, start, end. |
| Queue | Song requests with statuses `pending`, `queued`, `now_playing`, `completed`, `skipped`, `rejected`, `canceled`. One `now_playing` per show (partial unique index). Host can add from search or manually, approve, reject with reason, start, mark done, skip, move, remove. Participant autocomplete from the DJ's past shows. |
| Audience | `/dj/{handle}` profile, show page with search and request, queue page ("You are #N"), TV display page, QR code for the show. Guest identity from a cookie, upgraded when they sign in. |
| Song memory | My Performances (completed requests across shows), Sing Again, Saved Songs with request-from-saved. Saved songs keyed by a normalized fingerprint, not a catalog row id. |
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
4. **Skip identical uploads.** Store a sha256 of each uploaded CSV. Re-uploading the same file returns the current version instead of rebuilding.
5. **Cache-friendly search.** Search URLs include the catalog version (`/c/{id}/v{n}/search?q=`). Responses can be cached by Cloudflare safely because a rebuild changes the URL. This was an open TODO in Rails and is free here.
6. **Wait estimate from real data.** Estimated wait = position × median of `completed_at - started_at` for this show, falling back to 4 minutes until there are a few completed songs.
7. **"You're up next!" copy.** Position 1 says "You're up next!", now playing says "You're up!", others say "You are #N of M". This was an open TODO.
8. **One active show per DJ.** Enforce with a partial unique index (`user_id` where `status = 'active'`). Also an open TODO.
9. **Audience vs DJ accounts.** Everyone starts as `audience`. "Start hosting" upgrades to `dj` and asks for a handle. Audience pages pre-select the audience path. Keeps one login flow while giving audience members the lighter experience the TODO asked for.
10. **Stale guest cleanup.** The in-process cleanup goroutine (same one that deletes expired sessions and tokens) deletes guest profiles with no account and no activity in 14 days, and their saved songs. Completed requests keep their display name for the DJ's history.

---

## Architecture

### Stack

| Concern | Choice | Notes |
| --- | --- | --- |
| Language | Go (latest, matching track-anything) | |
| HTTP | `net/http` `ServeMux` with method and wildcard patterns | No router library |
| Templates | `html/template`, layout + per-page `content` block | Same `render.go` approach as track-anything |
| Assets | `embed` | Single binary |
| Frontend | HTMX (vendored), Pico.css (vendored), small vanilla JS | No build step |
| App database | SQLite, WAL, one file | `DB_PATH`, default `data/djm.db` |
| SQLite driver | `modernc.org/sqlite` (pure Go, includes FTS5) | No CGO. Same engine track-anything uses through `glebarez/sqlite` |
| Data access | `database/sql` with hand-written SQL | See "ORM or not" below |
| Migrations | Numbered `.sql` files embedded in the binary, applied in order on startup, recorded in a `schema_migrations` table | No migration tool |
| Object storage | Tigris via an S3 client (`aws-sdk-go-v2/service/s3` or `minio-go`) behind a small `blobStore` interface | Local-directory implementation for dev and tests |
| Backups | Litestream, replicating the app DB to Tigris | Catalog DBs are not backed up; they are already in Tigris |
| Email | `net/smtp` to a relay (SES SMTP, as today) | Console logger in dev; MailHog optional |
| Bot protection | Cloudflare Turnstile | Same as both existing apps |
| Passwords | `golang.org/x/crypto/bcrypt` | |
| QR codes | `rsc.io/qr` (tiny) or vendored JS generator | Rendered as PNG or SVG on the show page and display page |
| Logging | `log/slog` | |
| Dev reload | Air, run by version | As in track-anything |

**ORM or not.** track-anything uses GORM. This app needs raw SQL anyway for FTS5, bm25, partial indexes, and the per-catalog databases, so plain `database/sql` keeps one way of talking to SQLite instead of two. If we would rather match track-anything exactly, GORM for the app database is a reasonable choice too. Decide before phase 0 ends.

### Project layout

Flat `main` package, split by feature into files. Move to `internal/` packages only if navigation becomes painful.

```
djm/
├── PLAN.md
├── go.mod
├── main.go              # config, open DB, migrate, build mux, start server, start goroutines
├── config.go            # env parsing and validation (prod requires BASE_URL, Turnstile keys, Tigris creds)
├── env.go               # load optional .env
├── db.go                # open app DB, pragmas, run migrations
├── migrations/          # 0001_init.sql, 0002_..., embedded
├── models.go            # plain structs
├── auth.go              # email form, magic links, password, lockout, sessions
├── turnstile.go
├── mail.go
├── ratelimit.go
├── middleware.go        # logging, recovery, security headers, CSRF (http.CrossOriginProtection)
├── render.go
├── blobstore.go         # interface + Tigris and local-dir implementations
├── catalog_build.go     # CSV validate → build SQLite+FTS5 → upload → swap pointer
├── catalog_cache.go     # local file cache: fetch on miss, LRU eviction, open handle pool
├── search.go            # query sanitizing, FTS5 query, highlights
├── queue.go             # request lifecycle, rotation insert functions, positions
├── handlers_account.go
├── handlers_catalogs.go
├── handlers_shows.go    # host show page and queue actions
├── handlers_audience.go # /dj/{handle}, show page, request, queue, display
├── handlers_memory.go   # my performances, saved songs, sing again
├── handlers_admin.go    # users, log in as, feedback, cache status
├── cleanup.go           # expired sessions/tokens, stale guests, old local versions
├── templates/
├── static/
├── testdata/csvs/       # copied from djm-mono/fixtures/csvs
├── Dockerfile
├── litestream.yml
├── app.json             # Dokku health check
└── Makefile
```

### Data model (app database)

Ported from `core/db/schema.rb`, simplified. Timestamps are UTC.

- **users** — `email` (unique, lowercased), `role` (`audience`/`dj`/`admin`), `slug` (unique, nullable until hosting), `display_name`, `password_hash`, `email_confirmed_at`, `failed_login_attempts`, `locked_at`, `default_show_type`, `default_rotation_style`, `default_max_songs_per_singer`, `deletion_requested_at`.
- **sessions** — id is sha256 of the cookie token, `user_id`, `expires_at`.
- **magic_tokens** — `user_id`, `token_hash` (unique), `expires_at`, `consumed_at`.
- **catalogs** — `user_id`, `name` (unique per user), `variant_display`, `status`, `active_version`, `building_version`, `song_count`, `errors_json`, `last_csv_sha256`, `delete_requested_at`.
- **catalog_versions** — `catalog_id`, `version`, `csv_key`, `db_key`, `db_bytes`, `song_count`, `status`, `created_at`. Lets cleanup find old Tigris objects, and gives admin a history.
- **shows** — `user_id`, `catalog_id` (nullable), `name`, `slug` (unique per user), `show_type`, `status`, `scheduled_at`, `started_at`, `ended_at`, `rotation_style`, `approval_required`, `manual_entry_enabled`, `max_songs_per_singer`. Partial unique index: one `active` show per user.
- **audience_profiles** — `guest_token_hash` (unique, nullable), `user_id` (unique, nullable), `last_seen_at`.
- **show_participants** — `show_id`, `audience_profile_id` (unique together), `display_name`.
- **song_requests** — `show_id`, `show_participant_id`, `song_title`, `song_artist`, `song_version`, `song_external_id`, `request_source` (`audience`, `host_manual`, `host_catalog`, `saved`), `status`, `queue_position`, `requested_at`, `started_at`, `completed_at`, `rejected_at`, `rejection_reason`, `canceled_at`. Partial unique index: one `now_playing` per show.
- **saved_songs** — `audience_profile_id`, `fingerprint` (unique together), song fields, `source_catalog_id`, `request_count`, `last_requested_at`.
- **feedback** — `user_id` (nullable), `category`, `message`, `page_url`, `status`, `admin_notes`.

### Catalog databases and the local cache

Each catalog version is its own SQLite file with the same `songs` + `songs_fts` schema as today.

**Tigris layout**

```
catalogs/{catalog_id}/v{version}.csv   # the upload, source of truth
catalogs/{catalog_id}/v{version}.db    # built artifact
```

**Build (one goroutine)**

1. Upload handler streams the multipart file straight to Tigris as `v{n}.csv` (with a size limit, 100 MB), computing sha256 as it goes. If the hash matches `last_csv_sha256`, discard and keep the current version.
2. Insert `catalog_versions` row, set catalog `building_version = n`, `status = pending`, send the catalog id on the build channel.
3. The builder downloads the CSV to a temp file, validates it in a streaming pass, builds `v{n}.db` in a temp path, runs `PRAGMA integrity_check` and a non-empty check, uploads it to Tigris, then moves it into the cache directory.
4. In one transaction: `active_version = n`, `status = ready`, `song_count`. Searches switch on the next request. The old version stays until cleanup.
5. On failure: `status = failed`, `errors_json` set, `active_version` unchanged, so search keeps working.
6. On startup, any catalog left `pending` or `processing` is sent to the channel again.

**Cache**

- Directory `CATALOG_CACHE_DIR` (default `data/catalogs/`), files named `{catalog_id}_v{version}.db`.
- On search: if the file is missing, download from Tigris (one download per file at a time, guarded by a per-key mutex), then open it read-only (`mode=ro`, `immutable=1`).
- Keep a small pool of open read-only handles, closed when evicted.
- Track last use in memory (and touch the file mtime so it survives restarts).
- When total bytes exceed `CATALOG_CACHE_MAX_BYTES`, delete least-recently-used files. Never evict the active version of a catalog that an active show uses.
- Admin page shows: cached files and bytes, budget, hits/misses since boot, catalogs whose active version is missing from Tigris.

**Remote cleanup.** The cleanup goroutine deletes Tigris objects for versions older than the previous one and older than 48 hours, and everything for deleted catalogs.

### Queue

- The live queue is `song_requests` with `status = queued`, ordered by `queue_position`. Positions are rewritten densely in the same transaction as any change.
- `pending` requests (approval on) have no position until approved.
- Starting a request marks any current `now_playing` as `completed`, then sets this one `now_playing`.
- `max_songs_per_singer` counts that participant's `pending` + `queued`.
- Rotation style is a function that returns the insert position:
  - `standard` (launch): new requests go to the bottom.
  - `traditional` (later): same as standard for new requests; differs only in what happens after a singer finishes.
  - Groups and fairness rules (later) live in these functions.
- Host actions are POSTs that redirect back to the show page; HTMX swaps the queue in place.

### Real-time updates

Phase 1: HTMX polling on the host show page, audience queue page, and TV display (every 5 seconds, paused when the tab is hidden).

Later: a per-show in-memory broadcaster. Any queue change publishes the show id; `GET /shows/{id}/events` (SSE) tells the page to refetch its queue fragment. Polling remains the fallback. Web Push for "You're up next!" builds on the same hook.

### Routes (first pass)

```
GET  /                              landing (logged out) or dashboard
GET  /signin, POST /signin          email form (magic link) and password login
GET  /auth/{token}, POST /auth/{token}   confirm page, then redeem
POST /logout
GET  /account, POST /account/...    password set/remove, handle, display name, defaults, delete

GET  /catalogs                      list with status
POST /catalogs                      create
GET  /catalogs/{id}                 status, errors, song count, DJ search
POST /catalogs/{id}/upload          multipart CSV
POST /catalogs/{id}                 rename, variant display
POST /catalogs/{id}/delete
GET  /catalogs/{id}/v{v}/search     search fragment (used by DJ and audience pages)

GET  /shows, POST /shows
GET  /shows/{id}                    host view: pending, now playing, queue, completed
POST /shows/{id}                    settings
POST /shows/{id}/start, /end
POST /shows/{id}/requests           host add (catalog or manual)
POST /requests/{rid}/approve|reject|start|done|skip|move|delete
GET  /shows/{id}/participants?q=    autocomplete fragment
GET  /shows/{id}/qr.png

GET  /dj/{handle}                         DJ profile, upcoming and live shows
GET  /dj/{handle}/shows/{slug}            audience show page with search
POST /dj/{handle}/shows/{slug}/requests   request a song
GET  /dj/{handle}/shows/{slug}/queue      queue and "where am I"
GET  /dj/{handle}/shows/{slug}/display    TV display

GET  /me/performances
GET  /me/saved, POST /me/saved, POST /me/saved/{id}/delete, POST /me/saved/{id}/request

POST /feedback
GET  /docs/{slug}

GET  /admin, /admin/users, /admin/users/{id}, POST /admin/users/{id}/impersonate
GET  /admin/feedback, POST /admin/feedback/{id}
GET  /admin/catalogs                cache and Tigris status

GET  /healthz
GET  /static/...
```

All state changes are POST. GETs never write (except touching cache files and `last_seen_at`), which keeps the door open for read replicas or CDN caching later.

### Security

Same rules as track-anything: hashed session and magic tokens, `HttpOnly; Secure; SameSite=Lax` cookies, `http.CrossOriginProtection` on non-GET requests, `safeNext` redirects, security headers, trusted client IP header only when set explicitly. Never show an email on an audience-facing page. The guest cookie holds a random token; the database stores its hash.

### Configuration

| Variable | Purpose |
| --- | --- |
| `ADDR` / `PORT` | Listen address |
| `ENV` | `dev` or `prod` |
| `BASE_URL` | Public origin for emailed links and QR codes |
| `DB_PATH` | App SQLite file |
| `CATALOG_CACHE_DIR`, `CATALOG_CACHE_MAX_BYTES` | Local catalog cache |
| `BLOB_BACKEND` | `local` (dev/tests) or `s3` |
| `BLOB_DIR` | Directory for the local backend |
| `S3_ENDPOINT`, `S3_BUCKET`, `S3_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | Tigris |
| `SMTP_HOST`, `SMTP_PORT`, `SMTP_USER`, `SMTP_PASS`, `MAIL_FROM` | Email |
| `TURNSTILE_SITE_KEY`, `TURNSTILE_SECRET_KEY` | Bot protection |
| `TRUSTED_IP_HEADER` | `CF-Connecting-IP`, `Fly-Client-IP`, `X-Forwarded-For`, or empty |

---

## Deployment

### Now: one machine with Dokku

- Multi-stage Dockerfile: build the Go binary with `CGO_ENABLED=0`, copy it and the `litestream` binary into a small base image.
- Entrypoint: `litestream restore -if-db-not-exists -if-replica-exists $DB_PATH`, then `litestream replicate -exec /djm`. Litestream runs the app as a child process and replicates while it runs.
- `dokku storage:mount` a host directory at `/data` for `djm.db` and the catalog cache.
- Health check via `app.json` on `/healthz`.
- `git push dokku main` to deploy. Expect a few seconds of downtime per deploy (single instance, SQLite); acceptable for now. Avoid deploying during shows.
- Cloudflare in front for TLS and, later, caching of versioned search URLs.

### Backups

- Litestream replicates the app DB to `s3://{bucket}/litestream/djm.db` on Tigris.
- Catalog CSVs and built DBs are already in Tigris and are not part of the backup.
- Before the first real show: restore the backup to a scratch machine and confirm the app boots with all users and shows. Repeat after any backup config change.

### Later: Fly.io

Same image. One machine, one volume for `djm.db` and the catalog cache, Litestream to the same Tigris bucket. The cache budget is set below the volume size, so the volume never needs to grow with the number of catalogs. Moving is: stop Dokku app, let Litestream finish, start on Fly (restore runs on boot), switch DNS.

---

## Testing

Standard library `testing` and `httptest`, as in track-anything. Each test gets a temp app DB and a local-directory blob store.

- **Pure logic:** CSV header and row validation against every fixture in `testdata/csvs/`; search query sanitizing (`Mr.`, `AC/DC`, reserved words, empty input, 20-token cap); rotation insert positions; position renumbering; wait estimate; fingerprint normalization.
- **Catalog pipeline:** upload → build → search returns ranked, highlighted results; failed rebuild keeps the old version searchable; identical upload is skipped; cache miss downloads from the blob store; eviction respects the budget and never evicts an active show's catalog; restart re-queues a pending build.
- **Handlers:** happy path and failures for every route; CSRF; logged-out redirect; a DJ cannot see or change another DJ's catalog, show, or request (404).
- **Flows:** sign in by magic link; create catalog and upload; create and start a show; audience requests as a guest, sees position, signs in, and keeps their history; host approves, starts, completes; My Performances lists it; Sing Again requests it in the next show.

---

## Phases

Each phase ends deployed and usable.

### Phase 0 — Skeleton and deploy

- `main.go`, config, `.env`, middleware, render, layout with Pico and HTMX, `/healthz`.
- App DB with migration runner.
- Dockerfile with Litestream, Dokku app, storage mount, Tigris bucket.
- **Done when:** a push deploys, `/healthz` is green, and a Litestream restore onto a fresh container brings the DB back.

### Phase 1 — Accounts

- Email form, magic link, sessions, optional password, lockout, rate limits, Turnstile, Account page, handle and display name, DJ defaults.
- **Done when:** a new user can sign in by email, set a password, and log in with it; ten bad passwords lock password login and a magic link unlocks it.

### Phase 2 — Catalogs and search

- Blob store (local and Tigris), upload, build goroutine, versions, status page with errors, cache with eviction, search with highlights, delete, CSV format doc page.
- **Done when:** a 1M-row CSV builds without memory growth, search answers in well under 100 ms on a warm cache, and deleting the local cache directory only costs one download on the next search.

### Phase 3 — Shows and the host queue

- Shows CRUD, schedule, start, end, one active show per DJ. Host show page: pending, now playing, queue, completed. Add from search or manual entry, approve, reject, start, done, skip, move, remove. Participant autocomplete. Polling refresh.
- **Done when:** a KJ can run a full night from the host page.

### Phase 4 — Audience

- DJ profile page, audience show page with search and request, guest cookie identity, queue page with "You're up next!" wording and wait estimate, TV display page, QR code.
- **Done when:** a singer can scan, request, and watch their position with no account.

### Phase 5 — Song memory and admin

- Guest-to-account upgrade, My Performances, Sing Again, Saved Songs and request-from-saved. Feedback form. Admin users, log in as, feedback, catalog cache status. Cleanup goroutine.
- **Done when:** feature parity with the Rails app (minus the dropped items above).

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
- Billing.

---

## Open questions

1. **Backups:** OK to use Litestream rather than LiteFS? (LiteFS has no built-in S3 backup since LiteFS Cloud was retired.)
2. **Data migration:** is there production data in the Rails apps (users, catalogs, shows, history) that must move over, or do we start fresh? If it moves, we write a one-off importer that reads the Rails SQLite files and re-uploads catalog CSVs.
3. **ORM:** plain `database/sql` (suggested) or GORM to match track-anything?
4. **Where does Dokku run:** homelab behind Fly ingress like track-anything, or a VPS? Affects `TRUSTED_IP_HEADER` and whether Cloudflare proxies from day one.
5. **Email relay:** keep SES?
6. **Domain:** keep `djmagic.io` and cut over from the Rails apps at the end of phase 5?
7. **Catalog limits:** keep 1M rows per catalog, 100 catalogs per account, 10M rows per account, 5 uploads per hour?
