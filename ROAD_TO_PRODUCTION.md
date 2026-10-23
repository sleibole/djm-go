# Road to production: moving djmagic.io to box

`next.djmagic.io` runs the Go app on box (see `DEPLOY.md`): Fly ingress, WireGuard, Dokku, SES email, Turnstile, and Litestream backups all work end to end. This is what's left before `djmagic.io` itself moves from the Rails apps on Fly to box, in rough order of importance.

## Before the switch

### 1. Decide what happens to existing users

`PLAN.md` decided there is no data migration: the Go app starts with an empty database. Anyone using `djmagic.io` today would have to sign up again, DJs would re-upload their catalogs, and singers would lose saved songs and performance history.

- [ ] Check whether anyone is actively using the Rails app (recent sign-ins, shows, uploads).
- [ ] If they are: tell them ahead of time, or reconsider migrating at least accounts and handles.

What carries over: the audience URLs match Rails (`/dj/{handle}`, `/dj/{handle}/shows/{slug}`, `.../queue`, `.../display`). A printed QR code for a DJ's profile keeps working if that DJ claims the same handle again. Old magic links and sessions do not.

### 2. Back up uploaded catalogs

Litestream backs up `djm.db` only. Uploaded CSVs and built catalogs are on box's disk (`/var/lib/dokku/data/storage/djm-data/blobs`) with no copy anywhere else. If that disk dies, every DJ's catalog is gone.

- [ ] **Real fix:** stage 2 in `DEPLOY.md`, the Tigris blob store, so CSVs and built catalogs live in Tigris and the local copies are only a cache.
- [ ] **Stopgap until then:** a nightly job on box that copies `djm-data/blobs` to Tigris (for example `rclone sync` from a root cron job or systemd timer, using the Litestream bucket's keys), and a check that a file can be copied back.

### 3. Rehearse a restore

The backup uploads every 2 seconds, but nobody has restored from it yet. Restore to a scratch file, without touching production. On box:

```sh
sudo litestream restore -config /etc/litestream.yml -o /tmp/djm-restore.db \
  /var/lib/dokku/data/storage/djm-data/djm.db
python3 -c "import sqlite3; print(sqlite3.connect('/tmp/djm-restore.db').execute('select email, role from users').fetchall())"
sudo rm /tmp/djm-restore.db*
```

- [ ] The restored file opens and has the expected users and shows.
- [ ] Repeat after any change to `/etc/litestream.yml`.

The full in-place restore runbook is under "Restore" in `DEPLOY.md`.

### 4. Accept (or reduce) the single point of failure

The Rails apps run on Fly in three regions. On box, a power cut or ISP outage during a show takes the queue down until it comes back. Make this a deliberate choice.

- [ ] Decide it's acceptable for the current number of shows.
- [ ] Optional: a UPS for box and the router, so short power blips don't matter.
- [ ] Optional: an uptime check on `https://next.djmagic.io/healthz` that alerts you (the `../uptime` project, or a free external monitor).

### 5. Run a real show on next.djmagic.io

This covers what's still untested in section 8 of `DEPLOY.md`.

- [ ] Make yourself admin (`UPDATE users SET role='admin' WHERE email='…'`, run on box as uid 65532 against `djm-data/djm.db`).
- [ ] Upload a large catalog (~50 MB) and search it.
- [ ] Start a show; request songs from phones on cellular via the QR code.
- [ ] Rate limits see each visitor's own IP (two phones don't share a limit).
- [ ] Run the queue for a full night from the host page; the TV display keeps up.
- [ ] A `git push dokku main` between songs doesn't disrupt anything (zero-downtime deploy).
- [ ] Reboot box and check that the site, WireGuard, and Litestream all come back with no SSH session.

## The switch

Keep the Rails apps running on Fly, so going back is a DNS change.

1. A day ahead, lower the TTL on the `djmagic.io` DNS records in Cloudflare (for example to 1–5 minutes), so both the switch and a rollback take effect quickly.
2. Add the certificate on the Fly ingress: `fly certs add djmagic.io -a trackanything` (and `www.djmagic.io` if it's used).
3. Add the domain to the Dokku app and change the base URL:
   ```sh
   ssh dokku@box domains:add djm djmagic.io
   ssh dokku@box config:set djm BASE_URL=https://djmagic.io
   ```
4. Add `djmagic.io` to the Turnstile widget's hostnames.
5. Point the apex at Fly in Cloudflare, **DNS only (grey cloud)**: a CNAME to the target `fly certs setup djmagic.io -a trackanything` gives (Cloudflare flattens it at the apex), or `A 66.241.125.120` and `AAAA 2a09:8280:1::19f:8a98:0`.
6. Wait for `fly certs check djmagic.io -a trackanything` to show the certificate issued, then check `https://djmagic.io/healthz`, sign in by email, and load a DJ profile.
7. Optional: keep `next.djmagic.io` as an alias, or redirect it to `djmagic.io`.

## Rollback

Point the `djmagic.io` records back at the Rails apps on Fly. With the lowered TTL, this takes minutes. Anything created on the Go app in the meantime stays in its database (and its backup) but isn't visible on Rails.

## After the switch

- [ ] Keep the Rails apps on standby for about a week, then shut them down.
- [ ] Revoke the Rails SES SMTP credentials if the Go app uses new ones.
- [ ] Optional: set `djmagic-app` as the default SES configuration set on the `djmagic.io` identity, so bounce and complaint events are still recorded (the Go mailer doesn't send the configuration-set header Rails did).
- [ ] Raise the DNS TTL back up.
