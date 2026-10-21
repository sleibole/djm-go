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

**Step 2, after the first push has created `djm.db`.** Adds djm to `/etc/litestream.yml` (same Tigris bucket and keys as track-anything, its own path), restarts the service, and checks both databases sync. The exact entry gets written here once step 1 has shown the config's shape.

```sh
ssh -t administrator@box sudo sh /tmp/djm-setup-2.sh
```

## 4. Secrets

From the laptop:

```sh
ssh dokku@box config:set --no-restart djm \
  SMTP_HOST=email-smtp.<region>.amazonaws.com SMTP_USER=<ses-smtp-user> SMTP_PASS=<ses-smtp-pass> \
  TURNSTILE_SITE_KEY=<site-key> TURNSTILE_SECRET_KEY=<secret>
```

Production refuses to start without the Turnstile keys, so set at least those before the first push. No `S3_BUCKET` (see "Backups").

- Turnstile: add `next.djmagic.io` to the djmagic.io widget's hostnames in Cloudflare (or create a new widget).
- SES: `djmagic.io` is already verified, so `noreply@djmagic.io` sends as-is. Use the SES SMTP region the Rails app uses.

## 5. First push

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

- `next` CNAME `trackanything.fly.dev`, **DNS only (grey cloud)**. With the orange cloud on, `Fly-Client-IP` would be a Cloudflare address, so rate limits would lump every visitor together. Switching to proxied later means `TRUSTED_IP_HEADER=CF-Connecting-IP` and stripping that header at Fly.
- Any `_acme-challenge` record `fly certs show` asks for.

Leave the apex `djmagic.io` pointing at the Rails apps until cutover.

## 8. Verify

- [ ] `curl https://next.djmagic.io/healthz` returns `ok`.
- [ ] Sign in by email (real SES mail, Turnstile passes), open the dashboard.
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

## Open questions

- Turnstile: reuse the djmagic.io widget or create one for `next`?
- Backups: share track-anything's Tigris bucket (simplest, the step 2 default) or give djm its own bucket and keys?
