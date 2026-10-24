# Deploying DJMagic to the homelab with `git push dokku main`

This is the plan for phase 6 of `PLAN.md`: running this app on the homelab Dokku server (`box`) the same way `../track-anything` runs, and making it public through the shared Fly.io ingress in `../homelab-ingress`.

## How track-anything does it

- The repo has a second git remote: `dokku  dokku@box:trackanything`. `git push dokku main` sends the code to Dokku over SSH, Dokku builds the `Dockerfile`, runs `app.json`'s `/healthz` startup check, and swaps the container.
- `box` is the server's LAN name (`box.localdomain` from the router's DNS), so the remote keeps working when its DHCP address changes.
- SQLite lives on a host directory mounted at `/data` with `dokku storage:mount`.
- **Backups run on the host, not in the container.** Litestream 0.5.14 is installed on box as a systemd service (`litestream.service`, enabled, runs as root). `/etc/litestream.yml` (root only) lists `trackanything.db` in the Dokku storage directory and replicates it to Tigris. The app image has no Litestream in it.
- Public traffic never uses the LAN address. Fly.io terminates TLS, a small nginx app on Fly (`../homelab-ingress`, Fly app name `trackanything`) forwards every request over WireGuard to box's Fly private address `fdaa:6:5dee:a7b:910f:53be:b775:e002` on port 8080, keeping the `Host` header. Dokku's nginx on box picks the app by `Host`. Because box dials out to Fly, a changing home IP doesn't matter there either.

```
Browser ──HTTPS──▶ Fly Proxy (app "trackanything", sets Fly-Client-IP)
                     ▶ Fly nginx (homelab-ingress)
                       ──HTTP over WireGuard──▶ box [fdaa:…:e002]:8080 (Dokku nginx, routes by Host)
                                                  ▶ djm container (:8080)

box: litestream.service ── reads /var/lib/dokku/data/storage/{trackanything,djm}-data/*.db ──▶ Tigris
```

DJMagic follows the same pattern: a multi-stage `Dockerfile`, `app.json` with the `/healthz` check on port 8080, and the database backed up by the host Litestream service.

## Backups: host Litestream, not in the container

One Litestream process on box replicates every homelab app's SQLite file. Why that and not Litestream inside the djm container:

- **Exactly one replicator, whatever the containers do.** Dokku's zero-downtime deploys run the new container next to the old one for up to a minute (`wait-to-retire` is 60 seconds). With Litestream inside the image, that's two replicators on one database, which Litestream doesn't support. Outside, deploys don't touch it.
- **So djm keeps zero-downtime deploys**, as track-anything does. Two app processes sharing the SQLite file for that minute is safe: WAL locking works across containers on one machine, because the bind mount is a local directory.
- **One config and one service for all apps.** `systemctl is-active litestream` is already part of the reboot checklist in the ingress README.

What it costs:

- The config and Tigris keys live on box (`/etc/litestream.yml`), not in a repo. The entry for djm is recorded below so it can be rebuilt.
- Restore is a manual step (see "Restore"), not automatic on container start. With a persistent disk on box, a restore only happens after a disaster.

What stays in the image: `entrypoint.sh` and `litestream.yml` still run Litestream inside the container when `S3_BUCKET` is set. **Never set `S3_BUCKET` on the Dokku app.** Without it, the entrypoint runs `/djm` directly. The in-container path is for a later move to Fly.io, where there is no host to run Litestream on. Check that path against Litestream 0.5 before using it (the `?endpoint=` query string in `litestream.yml`, and `restore -if-db-not-exists -if-replica-exists`).

Running as root is fine. Litestream 0.5 creates its `.djm.db-litestream` state directory with the database file's owner, and SQLite running as root gives new `-wal` and `-shm` files the database's owner too. Track-anything's data directory confirms it: every file, including Litestream's, is owned by 65532. The container (uid 65532) can always write its database.

One rule: **the database file must exist before djm is added to `/etc/litestream.yml`.** If the service started before `djm.db` existed, either the service would fail (taking track-anything's backups down with it), or the file would be created as root. Deploy first, then add the entry.

## Stages

1. **Prove the pipeline.** Dokku app `djm`, `git push dokku main` works, `next.djmagic.io/healthz` is green through Fly, host Litestream backs up `djm.db`. Uploaded catalogs are stored on box's disk (`/data/blobs`), since the Tigris blob store isn't written yet. They are not backed up. Fine for testing, not for real shows.
2. **Tigris blob store** (code). Then switch to it and re-upload test catalogs. The app's Tigris keys go in `dokku config:set` under their own names, not `S3_BUCKET`.
3. **Real show on `next.djmagic.io`**, restore test, then cut `djmagic.io` over (see `PLAN.md`, phase 6).

The rest of this file is stage 1.

## 1. Repo changes before the first push

- [ ] **Absolute data paths in the image.** `BLOB_DIR` and `CATALOG_CACHE_DIR` default to the relative `data/blobs` and `data/catalogs`. They land on the volume today only because the image has no `WORKDIR` (so the working directory is `/`). Make it explicit in the `Dockerfile`:

  ```dockerfile
  ENV ENV=prod DB_PATH=/data/djm.db BLOB_DIR=/data/blobs CATALOG_CACHE_DIR=/data/catalogs
  ```

- [x] **Catalog builds during a zero-downtime deploy.** Done. The new container used to delete the old one's in-progress temp files at startup and rebuild its `processing` catalog alongside it. Now only one process builds at a time (an exclusive `flock` on `/data/catalogs/.builder.lock`, released by the kernel when the old container exits). Temp files are removed only after 10 minutes untouched, and the builder checks for pending work every 30 seconds. A catalog uploaded during the overlap starts building once the old container is gone, up to about a minute later.

## 2. Create the Dokku app on box

**Done (Oct 20).** These ran from the laptop through `ssh dokku@box`, copying track-anything's setup (Dockerfile builder, storage at `/var/lib/dokku/data/storage/<app>-data`, port `http:8080:8080` detected from the `Dockerfile`'s `EXPOSE`, the global `box` domain, zero-downtime checks left on):

```sh
dokku apps:create djm
dokku storage:ensure-directory djm-data --chown false
dokku storage:mount djm /var/lib/dokku/data/storage/djm-data:/data
dokku domains:add djm next.djmagic.io            # vhosts are now djm.box and next.djmagic.io
dokku nginx:set djm client-max-body-size 101m    # Dokku's default is 1m
dokku nginx:set djm client-body-timeout 300s
dokku nginx:set djm proxy-read-timeout 300s
dokku nginx:set djm proxy-send-timeout 300s
dokku config:set --no-restart djm BASE_URL=https://next.djmagic.io TRUSTED_IP_HEADER=Fly-Client-IP \
  SMTP_PORT=587 'MAIL_FROM=DJMagic <noreply@djmagic.io>'
```

And in this repo: `git remote add dokku dokku@box:djm`.

## 3. Box setup that needs sudo

`administrator` on box needs a password for sudo, so these run from your terminal. The scripts are copied to `/tmp` on box.

**Step 1, before the first push.** Gives the storage directory to uid 65532 (the container's user; it's `dokku`-owned now, so the app couldn't create `djm.db`), and saves a copy of `/etc/litestream.yml` with the keys blanked, so the djm entry can be written to match track-anything's:

```sh
ssh -t administrator@box sudo sh /tmp/djm-setup-1.sh
```

Done Oct 21.

**Step 2, after the first push has created `djm.db`.** Backs up `/etc/litestream.yml`, appends djm's entry (keeping the file's comments), checks the YAML lists both databases (restoring the backup if not), restarts the service, and shows the recent log lines:

```sh
ssh -t administrator@box sudo sh /tmp/djm-setup-2.sh
```

Done Oct 21: the first snapshot was uploaded, both databases sync every 2 seconds, and Litestream's files (`.djm.db-litestream/`) are owned by 65532 like the rest. The previous config is `/etc/litestream.yml.bak-20261021-232550`.

`/etc/litestream.yml` has the Tigris keys at the top level, shared by every entry. djm uses track-anything's bucket under its own path:

```yaml
dbs:
  - path: /var/lib/dokku/data/storage/trackanything-data/trackanything.db
    replica:
      type: s3
      bucket: trackanything-backups
      path: trackanything
      endpoint: fly.storage.tigris.dev
      region: auto
  - path: /var/lib/dokku/data/storage/djm-data/djm.db
    replica:
      type: s3
      bucket: trackanything-backups
      path: djm
      endpoint: fly.storage.tigris.dev
      region: auto
```

## 4. Secrets

From the laptop:

```sh
ssh dokku@box config:set --no-restart djm \
  SMTP_HOST=email-smtp.<region>.amazonaws.com SMTP_USER=<ses-smtp-user> SMTP_PASS=<ses-smtp-pass> \
  TURNSTILE_SITE_KEY=<site-key> TURNSTILE_SECRET_KEY=<secret>
```

Production refuses to start without the Turnstile keys and without `SMTP_HOST`. `SMTP_USER` and `SMTP_PASS` may be empty. Set the mail and Turnstile values before deploying the image that enforces them. No `S3_BUCKET` (see "Backups").

Do not set `DEV_SHOW_MAGIC_LINK` here. Production rejects that variable even when SMTP is configured, and `ENV=dev` must not be used on a host that anyone else can reach.

- Turnstile: add `next.djmagic.io` to the djmagic.io widget's hostnames in Cloudflare (or create a new widget).
- SES: `djmagic.io` is already verified, so `noreply@djmagic.io` sends as-is. Use the SES SMTP region the Rails app uses.

## 5. First push

Done Oct 21 (`7760da2`): built in about 50 seconds, both health checks passed, `/healthz` returns 200 on `djm.box` and over WireGuard as `next.djmagic.io`, and `djm.db` is owned by 65532. The Turnstile keys were set for that push. `SMTP_HOST` was not, and that build logged sign-in links. The current app will not boot in production without `SMTP_HOST`, and it does not log message bodies. Set SMTP before the next push (section 4).

From this repo:

```sh
git push dokku main
```

Check on box:

```sh
curl -H 'Host: djm.box' http://localhost:8080/healthz                                       # ok
curl -H 'Host: next.djmagic.io' 'http://[fdaa:6:5dee:a7b:910f:53be:b775:e002]:8080/healthz' # ok, through WireGuard
ls -la /var/lib/dokku/data/storage/djm-data                                                 # djm.db owned by 65532
```

Then run step 2 from section 3, and check:

```sh
journalctl -u litestream -f      # "replica sync" lines for both trackanything.db and djm.db
```

## 6. Fly ingress (`../homelab-ingress`)

Done Oct 21 (`homelab-ingress` commit `9e6696d`, Fly release deployed the same day). A 5 MB POST with `Host: next.djmagic.io` to `https://trackanything.fly.dev` reaches the djm app, and track-anything's health checks still pass. The certificate and DNS below are still to do.

Two changes, both harmless for track-anything:

- [ ] **Upload size.** nginx's default `client_max_body_size` is 1 MB, so catalog uploads would fail at Fly before reaching box. In `nginx.conf`, inside `server`:

  ```nginx
  client_max_body_size 101m;
  proxy_request_buffering off;
  proxy_read_timeout 300s;
  proxy_send_timeout 300s;
  ```

- [ ] **Remove the temporary debug headers** from the last commit ("Temporary debug headers for ip addresses"), if they're still there.

Then:

```sh
cd ../homelab-ingress
fly deploy
fly certs add next.djmagic.io -a trackanything
fly certs show next.djmagic.io -a trackanything   # shows the DNS records to create
```

## 7. DNS

Where `djmagic.io`'s DNS lives (Cloudflare per the ingress README):

**Done Oct 21.** `next` is a CNAME to `2plmnoj.trackanything.fly.dev` (the custom-domain target `fly certs setup next.djmagic.io -a trackanything` gives), **DNS only (grey cloud)**. Let's Encrypt issued the certificate within a minute. `https://next.djmagic.io/healthz` returns 200, and plain HTTP redirects to HTTPS.

- A CNAME follows Fly if the app's addresses change. The alternative is `A 66.241.125.120` plus `AAAA 2a09:8280:1::19f:8a98:0`, as `trackanything.io` uses. The apex `djmagic.io` will need those (or Cloudflare's CNAME flattening) at cutover, since an apex can't be a plain CNAME.
- Keep it grey. With the orange cloud on, Fly can't validate the certificate, and `Fly-Client-IP` would be a Cloudflare address, so rate limits would lump every visitor together. Switching to proxied later means `TRUSTED_IP_HEADER=CF-Connecting-IP` and stripping that header at Fly.
- No `_acme-challenge` or `_fly-ownership` record is needed: the app has a dedicated IPv6 address and traffic points straight at Fly.

Check with `fly certs check next.djmagic.io -a trackanything`.

Leave the apex `djmagic.io` pointing at the Rails apps until cutover.

## 8. Verify

- [x] `curl https://next.djmagic.io/healthz` returns `ok`.
- [x] Sign in by email (real SES mail, Turnstile passes), open the dashboard.
- [ ] After the `__Host-` cookie deploy, repeat the browser check in `docs/development/SECURITY_AUDIT.md`: sign in, refresh, log out, send a guest request, refresh the audience queue. Existing browsers are signed out by that deploy.
- [ ] Upload a large catalog (~50 MB) to prove the body size and timeouts on both nginx hops. Search it.
- [ ] Start a show and request from a phone over cellular, via the QR code.
- [ ] Rate limits see the phone's IP: temporarily log the client IP, or check that two devices don't share a limit.
- [ ] A second `git push dokku main` deploys cleanly, the data survives, and `journalctl -u litestream` shows djm still syncing afterwards.
- [ ] Restore test (below), at least once before the first real show.
- [ ] Reboot box and check that `next.djmagic.io` comes back and Litestream syncs both databases, with no SSH session.

## Restore

On box, as `administrator`. Given a database path, `litestream restore` finds the replica in `/etc/litestream.yml`.

```sh
dokku ps:stop djm
cd /var/lib/dokku/data/storage/djm-data
sudo mkdir -p ../djm-data-old && sudo mv djm.db* .djm.db-litestream ../djm-data-old/
sudo litestream restore -config /etc/litestream.yml /var/lib/dokku/data/storage/djm-data/djm.db
sudo chown 65532:65532 djm.db
sudo systemctl restart litestream
dokku ps:start djm
```

For a rehearsal that doesn't touch production, restore to a scratch path instead (`-o /tmp/djm-restore.db`) and open it with `sqlite3` to check the users and shows are there.

## About the changing LAN address

Nothing here depends on box's `192.168.1.x` address. The git remote uses the name `box`, and public traffic goes over WireGuard to box's fixed Fly private address. If `box` ever stops resolving on the LAN, a DHCP reservation for box on the router fixes it for good, and `ssh dokku@<ip>` works in the meantime.

## Cookie names

Outside development the session and guest cookies are `__Host-session` and `__Host-guest` (Secure, Path=/, no Domain). Development on plain HTTP keeps `session` and `guest`. Production does not accept the old names.

The first deploy of this change signs everyone out and detaches in-progress guest browsers from their queue identity. They sign in again, or send a new request, and continue. No data migration.

## Security follow-ups

These are documented, not changed, in this pass. Detail and the manual browser check are in `SECURITY_AUDIT.md`.

**Client IP, separate infrastructure pass.** The app trusts `Fly-Client-IP` on every connection because `TRUSTED_IP_HEADER=Fly-Client-IP`. WireGuard does not authenticate the Fly ingress app. The home server's only peer is the Fly 6PN gateway (`sjc2.gateway.6pn.dev`), with `AllowedIPs = fdaa:6:5dee::/48`. Any Fly machine in the organization can reach Dokku on box's `fdaa:` address. Limiting port 8080 to the WireGuard interface would drop the LAN and would not stop another Fly app from connecting and sending its own `Fly-Client-IP`. Do not blank that header in Go: every visitor would then share Dokku's address. Do not switch the app to `X-Forwarded-For`. The Fly nginx config appends that header (`$proxy_add_x_forwarded_for`), so a caller can still supply the first address.

**HSTS.** `homelab-ingress` `fly.toml` has `force_https = true` and no `[http_service.http_options.response.headers]` entry. That redirect is not HSTS. `next.djmagic.io` is grey-cloud DNS, so Cloudflare does not add the header either. Add it at the Fly TLS edge, for example:

```toml
[http_service.http_options.response.headers]
  Strict-Transport-Security = "max-age=604800"
```

No `includeSubDomains` and no `preload`: the Rails app is still on the apex. One week is long enough to notice a mistake. Do not emit HSTS from this Go process unless Fly cannot.

**Magic-link URLs in proxy logs.** The app redacts `/auth/{token}` in its own request log. A GET does not consume the token, so a logged URL stays valid for up to 10 minutes. Fly nginx and Dokku's default combined log record the raw request URI. Redact `/auth/` in the nginx `log_format` with a `map` before relying on email sign-in. Fly edge logs may still contain the path. This pass does not change nginx.

## Open questions

- Turnstile: reuse the djmagic.io widget or create one for `next`?
- Backups: share track-anything's Tigris bucket (simplest, the step 2 default) or give djm its own bucket and keys?
