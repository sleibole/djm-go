# DJMagic

A web app for karaoke hosts and DJs. A host uploads a song catalog, starts a show, and the audience scans a QR code to search the catalog, request a song, and see their place in line.

The app is one Go process and one SQLite database. Pages are server-rendered HTML. Forms work without JavaScript; HTMX updates search, the queue, and the TV display when it is available.

This is the Go rewrite of the Rails apps. Product and implementation notes are in [`PLAN.md`](PLAN.md).

## What you can do

- Sign up with an email. A magic link logs you in. A password is optional and is set on the Account page after the address is confirmed.
- Upload a CSV song catalog. The app builds a searchable catalog and reports import errors.
- Create a show, schedule it, and start it. One show can be live at a time.
- Run the queue from the host page: approve or reject requests, start a song, mark it done, skip it, remove it, or drag it to a new position.
- Share the show with a QR code, and open a dark TV display of now playing and up next.
- Request a song from a phone, as a guest or with an account, and see your place in line.
- Sign in to keep My Performances and Saved Songs, and request a remembered song at a live show.
- Send feedback. An admin can review users and feedback, change roles, and log in as another user.

## Requirements

- [Go 1.27](https://go.dev/dl/) or newer

## Run locally

```sh
make dev
```

That starts [Air](https://github.com/air-verse/air), which rebuilds the server when you save a Go file, an HTML template, CSS or JavaScript under `static/`, or an SVG icon. Open http://localhost:8200 and refresh after a restart.

`make dev` and `make run` set `ADDR=:8200`, `PORT=8200`, and `BASE_URL=http://localhost:8200` before the process starts. `ADDR` is the address the server binds. `PORT` is the same number so the two cannot disagree. The app loads `.env` after that and leaves variables that are already set alone, so a file that still says port 8080 does not move this server. `make run` starts the server once, without reload.

Port 8200 is the personal server. Named agent servers use `make dev-agent NAME=<unique-name>` and take a port from 8300 through 8399, with their own database, blobs, and catalog cache under `tmp/dev-agents/<name>/`. `make dev-list` shows them. `make dev-stop NAME=<name>` stops one. Those commands do not start or stop the personal server. track-anything keeps port 8080 and ports 8100–8199.

With `SMTP_HOST` unset, login and verification links are written to the server log. To send mail locally, copy `.env.example` to `.env` and run [MailHog](https://github.com/mailhog/MailHog) (SMTP on port 1025, inbox at http://localhost:8025). A missing `.env` is fine. MailHog is shared by every local server.

The personal SQLite file is created at `data/djm.db`. Uploaded CSVs go in `data/blobs`, and built catalogs are cached in `data/catalogs`. An agent server does not use those directories.

When `ADDR` and `PORT` are both unset, the binary listens on `:8080`. That is what the container uses. Production does not read this Makefile.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `ADDR` | `:8080` | Listen address. If `ADDR` is unset, `PORT` is used as `:$PORT`. `make dev` and `make run` set `ADDR` and `PORT` to 8200, which wins over `.env`. |
| `DB_PATH` | `data/djm.db` | SQLite file. |
| `ENV` | `dev` | `dev` or `prod`. `dev` turns off the `Secure` cookie flag so `http://localhost` works. |
| `BASE_URL` | `http://localhost:8080` | Public origin used in emailed links, QR codes, and Open Graph URLs. Required in production, and it must be `https`. `make dev` and `make run` set `http://localhost:8200`. |
| `BLOB_DIR` | `data/blobs` | Local blob store for uploaded CSVs and built catalogs. |
| `CATALOG_CACHE_DIR` | `data/catalogs` | Local cache of built catalogs. Safe to delete; a search refills it. |
| `CATALOG_CACHE_MAX_BYTES` | `10737418240` | Cache budget in bytes (10 GiB). Least recently used catalogs are removed past this. |
| `SMTP_HOST` | empty | Empty logs links to the console. Set it to send mail. |
| `SMTP_PORT` | `587` | Implicit TLS on `465`; STARTTLS otherwise. |
| `SMTP_USER` | empty | SMTP username. |
| `SMTP_PASS` | empty | SMTP password. |
| `MAIL_FROM` | `DJMagic <noreply@djmagic.io>` | From address. |
| `TRUSTED_IP_HEADER` | empty | Client IP header set by a trusted proxy: `Fly-Client-IP`, `CF-Connecting-IP`, or `X-Forwarded-For`. Empty uses the connection address. |
| `TURNSTILE_SITE_KEY` | test key in development | Public Turnstile widget key. Required in production. |
| `TURNSTILE_SECRET_KEY` | test key in development | Turnstile secret used for server-side verification. Required in production. Never log it. |

## Tests

```sh
make test
```

## Build

```sh
make build
```

Writes a static binary to `./djm`.

## Docker

```sh
make docker-build
```

```sh
make docker-run
```

`make docker-run` builds the image and publishes host port 8200 to the container's port 8080, with a volume at `/data`. The container is not given `ADDR` or `PORT`, so the process still listens on 8080. With `S3_BUCKET` unset, the entrypoint runs the app directly. Set `S3_BUCKET` and the AWS credentials to restore from Litestream and replicate while the container runs.
