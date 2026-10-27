# Security audit

Date: 2026-10-23. Updated the same day after the Phase 1 fixes below. Scope: this repository (Go server, templates, static assets, auth, catalogs, shows, audience flow, admin, deploy notes). Method: source review of request handling, cookies, headers, templates, SQL, uploads, and `govulncheck`. No production traffic was probed.

## Status after Phase 1

Implemented in this repository:

- Go is pinned to 1.27.3 in `go.mod` and `Dockerfile`. `go test ./...` passes. `govulncheck ./...` on Go 1.27.3 reports no reachable vulnerabilities. `golang.org/x/crypto/openpgp` (`GO-2026-5932`) is still an unused indirect requirement and is not called.
- `consoleMailer` logs the recipient and subject only. Production `validate` fails when `SMTP_HOST` is empty, and also when `DEV_SHOW_MAGIC_LINK` or `DEV_ALLOW_SMTP` is set. SMTP user and password stay optional. Development does not send SMTP mail unless `DEV_ALLOW_SMTP` is the exact string `true`. `MAIL_BACKEND=console` keeps the console backend even when SMTP settings are present. Named agent servers set that and clear SMTP credentials in the process environment. The check-your-email page includes the URL only when `ENV` is `dev`, `SMTP_HOST` is empty, and `DEV_SHOW_MAGIC_LINK` is the exact string `true`. The handler also refuses when `ENV` is not `dev`.
- Removing a password deletes every other session for that user and keeps the current one.
- HTML rendered for a browser sends `Cache-Control: private, no-store`. That includes public pages, search fragments, and signed-in pages, because each response carries a CSRF token. Hashed static files stay `public, max-age=31536000, immutable`.
- When `ENV` is not `dev`, the cookies are `__Host-session` and `__Host-guest` (Secure, Path=/, no Domain). Development keeps `session` and `guest`. Production does not read the unprefixed names. The first production deploy signs everyone out and detaches in-progress guest browsers.

Not implemented, on purpose: CSP enforcement, HSTS, nginx log redaction, and any change to `Fly-Client-IP` trust. Those are written up below. `Fly-Client-IP` stays a separate infrastructure hardening pass. CSRF tokens are now implemented; see `docs/development/CSRF.md`.

### Browser check

Unit tests are not the browser check. A local process was started with `ENV=prod` (so the cookies are the `__Host-` names and `Secure`), `BASE_URL=https://127.0.0.1:18080`, Cloudflare's always-pass Turnstile test keys, and `SMTP_HOST` set so production `validate` accepts the process. The browser used `http://127.0.0.1:18080`. Chrome treats that address as a secure context, so it will store a `Secure` `__Host-` cookie there. This was not `https://next.djmagic.io`.

That browser did all of the following:

1. Password sign-in for a seeded DJ completed and landed on the home page, signed in.
2. Opening `/account`, then loading `/account` again, still showed "Signed in as smoke@example.com."
3. Creating a live show (`Friday Smoke`) while signed in succeeded.
4. Log out returned to the public home page. The next request for `/account` redirected to `/signin?next=%2Faccount`.
5. Signed out, a request for "Hello" / "Alice" on `/dj/smoke-dj/shows/friday-smoke` redirected to the audience queue and showed "You're up next! You are number 1 of 1."
6. Loading that queue URL again, without the `requested` query, still showed the same place in line and "Cancel your request for Hello."

`https://next.djmagic.io` was not used. After SMTP is set and this image is deployed, repeat this on the real host:

1. Sign in with a password or a magic link. DevTools should show `Set-Cookie` for `__Host-session` with `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, and no `Domain`. There should be no cookie named `session`.
2. Open Account, reload the tab, and confirm the account email is still shown.
3. Log out. Reload Account. The browser should be on the sign-in page, and `__Host-session` should be gone.
4. On a phone or a private window, open a live show, submit a song request, and land on the queue. DevTools should show `__Host-guest` with the same attributes, and not a cookie named `guest`.
5. Reload the queue. The same request should still be marked as yours. A second browser that only has an old `guest` cookie should not be marked as that person.

## CSP and CSRF

**Content-Security-Policy is set, and it only stops framing.** There is no `<meta http-equiv="Content-Security-Policy">` tag. Every response gets one header from `securityHeaders` in `middleware.go`:

```
Content-Security-Policy: frame-ancestors 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: same-origin
```

`frame-ancestors 'none'` is clickjacking protection for current browsers. The policy does not restrict scripts, styles, images, connections, frames, form targets, or `base-uri`. Inline scripts in `templates/layout.html` and the Turnstile loader (`https://challenges.cloudflare.com/turnstile/v0/api.js` from `templates/partials/turnstile.html`) would need nonces and an explicit `script-src` before a stricter policy can be turned on. `TestSecurityHeaders` locks the current value in.

Also missing from the app: `Strict-Transport-Security`, `Permissions-Policy`, and `X-Frame-Options` (the last is only a fallback for clients that ignore CSP).

**Forms use a session-bound CSRF token.** `{{csrfField}}` renders the hidden input, and `csrfProtect` checks it on every unsafe method. `http.CrossOriginProtection` still wraps the mux and rejects `Sec-Fetch-Site` values other than `same-origin` or `none`. The token covers the case where both `Sec-Fetch-Site` and `Origin` are missing, which is where Go's origin check fails open. See `docs/development/CSRF.md`.

Session, guest, and anonymous CSRF cookies are `SameSite=Lax`, `HttpOnly`, and `Secure` when `ENV` is not `dev`. Outside development the names are `__Host-session`, `__Host-guest`, and `__Host-csrf`. `SameSite=Lax` is defense in depth, not the only control. Typing the account email to confirm deletion stops accidents; it is not a CSRF secret.

No handler changes state on `GET`, `HEAD`, or `OPTIONS`, aside from sliding session expiry and `last_seen_at`. Post-login redirects go through `safeNext`, which allows only a single-slash local path.

## Findings

| Severity | Location | Finding |
| --- | --- | --- |
| Fixed | `go.mod`, `Dockerfile` | Go is 1.27.3. The reachable stdlib findings from the 1.27.0 scan (`GO-2026-6599`, `GO-2026-6600`, `GO-2026-6608`, `GO-2026-6609`, `GO-2026-6809`, and the other stdlib items fixed in 1.27.2 or 1.27.3) are gone. `govulncheck` still reports `GO-2026-5932` for `golang.org/x/crypto/openpgp`, which this module requires indirectly and does not call. |
| Fixed | `mail.go` `consoleMailer.Send`, `config.go` `validate` | The console mailer no longer logs the body. Production refuses to start without `SMTP_HOST`, and refuses `DEV_SHOW_MAGIC_LINK`. The sign-in page shows the URL only for an explicit local opt-in. Deploy still has to set `SMTP_HOST` before this image will boot. See `DEPLOY.md`. |
| Fixed | `account.go` `handleAccountPasswordRemove` | Removing a password now deletes every other session, same as a password change. The current session stays. |
| Medium | `ratelimit.go` `clientIP`, `config.go` | `TRUSTED_IP_HEADER` (production: `Fly-Client-IP`) is trusted on every connection. The header is safe only when every path to the process is a proxy that overwrites it. Dokku also serves `djm.box`. Anyone who can reach the origin can pick the address used for login, magic-link, upload, feedback, and song-request limits. Ignore the header unless the peer is the proxy, or do not publish a vhost that bypasses Fly. |
| Medium | `middleware.go` `securityHeaders` | CSP does not constrain script execution. Pages include inline scripts and load Turnstile from Cloudflare. htmx 2.0.11 (`static/htmx.min.js`) ships with `allowScriptTags` and `allowEval` enabled and swaps HTML with `innerHTML`. Templates go through `html/template`, so user text is escaped today. A template bug or a raw `template.HTML` mistake would run as script, and this header would not stop it. After the Go upgrade, add a nonce-based `script-src` (self plus `https://challenges.cloudflare.com`), `object-src 'none'`, `base-uri 'self'`, and `form-action 'self'`. |
| Low | `auth.go` `handlePasswordLogin` | Ten failures set `locked_at`. The locked response is different from "email or password is incorrect", so a locked address can be distinguished. The same ten tries are an account-lock denial of service until the owner uses a magic link. The IP limiter is 10 attempts per minute, which is enough to lock one account. |
| Low | `audience.go` `placeAudienceRequest` | Song requests are unauthenticated and have no Turnstile check. The cap is 10 requests per 10 minutes per client address per show. A pool of addresses can fill a queue. Login and magic links are the only Turnstile gates. |
| Fixed | `render.go` `renderBlock` | HTML responses send `Cache-Control: private, no-store`, including pages that used to be publicly cacheable and audience search that used `private, max-age=300`. Hashed static files are unchanged. |
| Fixed | `auth.go`, `audience.go` | Production cookies are `__Host-session` and `__Host-guest`. The unprefixed names are not accepted. Development on HTTP keeps `session` and `guest`. |
| Low | `auth.go` `sendMagicLink`, `middleware.go` `logPath` | The sign-in secret is the URL path `/auth/{token}`. `GET` deliberately does not consume it, so a mail scanner cannot burn it, and neither does an access log. Nginx and Fly logs of that `GET` hold a still-valid token until it expires or is posted. The app's own request log hides the path. `Referrer-Policy: same-origin` keeps the token out of the Reddit link's `Referer`. Redact `/auth/` in the proxy logs. |
| Info | `shows.go` `newShowSlug`, `audience.go` `handleAudienceQueue`, `handleDisplay` | A show's public URL is `/dj/{handle}/shows/{slug}`. The slug is derived from the name. Queue and TV display are world-readable and include singer names and song titles. Pending and rejected rows are limited to the visitor's own participant. That matches a room display. It is not an access check. |
| Info | `auth.go` `handleAccountPassword` | Changing a password does not ask for the current one. A stolen session can set a new password and drop every other session. Recovery is a magic link. That is the designed tradeoff. |

## What is in good shape

Authentication material is handled carefully. Session ids, magic links, and guest cookies are 32 bytes from `crypto/rand`, and the database stores only SHA-256. Magic links last 10 minutes, one click consumes the row with a conditional update, and older links for that user are deleted first. `GET /auth/{token}` renders a confirm page so a scanner that only fetches the URL does not sign the user in. Password checks use bcrypt at the default cost, cap input at 72 bytes, and compare a dummy hash when the account has no password. New accounts and existing accounts get the same "check your email" page. Signup cannot overwrite an existing role. `safeNext` rejects scheme-relative URLs, backslashes, and control characters. Email links use `BASE_URL`, not the request `Host`.

Authorization is per owner. Catalog and show loads require `user_id` and answer 404 otherwise. Request actions load the show through that check. Audience cancel requires the request to belong to the visitor's participant. Saved songs and performances are scoped to the audience profile. Admin routes 404 for non-admins. An admin cannot change their own role, remove the last admin, or impersonate another admin. Impersonation sessions last one hour and do not slide. Stopping impersonation checks that the admin still has the admin role. Role changes and impersonation start/stop are logged.

Data handling is parameterized. GORM and `database/sql` use placeholders. Admin search escapes `LIKE` wildcards. Catalog FTS turns the query into bareword prefix terms and drops `AND` / `OR` / `NOT` / `NEAR`, so punctuation never reaches the FTS parser. `ORDER BY` is chosen from a fixed map. Uploads are capped at 100 MB, limited to 5 per user per hour, written to a temp file, and stored under keys the app generates (`fs.ValidPath` on blob keys). CSV rows are capped. Static files are served only when the name is in the embedded asset map, so `..` does not escape into templates. The container runs as uid 65532. `.env` is gitignored. Turnstile secrets are not logged. Production `validate` requires `https` `BASE_URL`, both Turnstile keys, and `SMTP_HOST`, and it rejects `DEV_SHOW_MAGIC_LINK`. Server errors returned to the client are a generic 500; the detail goes to the log. Foreign keys are on, and sessions and magic tokens cascade with the user.

## Client IP (separate infrastructure pass)

`TRUSTED_IP_HEADER=Fly-Client-IP` is trusted on every connection to this process. Rate limits for login, magic links, uploads, feedback, and song requests use that address.

WireGuard does not authenticate the Fly ingress app. The home server has one peer, and that peer is the Fly 6PN gateway (`sjc2.gateway.6pn.dev`), not an application machine. `AllowedIPs` is `fdaa:6:5dee::/48`. The ingress README says any authorized Fly machine in the organization can reach box on its `fdaa:` address, and the nginx machines are meant to be disposable. Dokku also answers on the host (`curl -H 'Host: djm.box' http://localhost:8080/healthz` in `DEPLOY.md`).

Restricting port 8080 to the WireGuard interface would drop LAN clients. It would not stop another Fly app in the same organization from opening a connection and sending its own `Fly-Client-IP`. Do not make that firewall change as a fix for this. Do not clear the header in Go: every visitor would then share Dokku's `RemoteAddr`, and every limit would be one bucket. Do not switch the app to `X-Forwarded-For`. The Fly nginx config sets that header with `$proxy_add_x_forwarded_for`, which appends to a client-supplied value, so the first address is not the one Fly observed.

This stays open until there is a real check that the TCP peer is the ingress, or the origin is unreachable except through that ingress.

## HSTS

`homelab-ingress` `fly.toml` sets `force_https = true` and has no `[http_service.http_options.response.headers]` entry. That is a redirect, not HSTS. `next.djmagic.io` is a grey-cloud CNAME (`DEPLOY.md`), so Cloudflare does not add `Strict-Transport-Security` either. Fly's current config key for a response header is `http_service.http_options.response.headers`. A first value, without `includeSubDomains` and without `preload` (the Rails app is still on the apex):

```toml
[http_service.http_options.response.headers]
  Strict-Transport-Security = "max-age=604800"
```

One week is enough to notice a bad deploy. Do not emit this header from the Go process unless Fly cannot. The process sees plain HTTP behind the proxy.

## Proxy logs

The app redacts `/auth/{token}` in its own request log (`logPath`). `GET /auth/{token}` does not consume the token, so a proxy log line stays a valid sign-in link for up to 10 minutes. Fly nginx (`homelab-ingress/nginx.conf`) and Dokku's default combined log record the raw URI. This pass does not change nginx. A `map` in front of the log format is the change to make before email sign-in is the normal path:

```nginx
map $request_uri $log_uri {
    ~^/auth/  /auth/{token};
    default   $request_uri;
}
```

Use `$log_uri` in the `log_format` instead of `$request_uri`. Fly's own edge logs can still contain the path. That is outside this repo.

## CSP (not enforced)

Leave the current header as `frame-ancestors 'none'` until the policy below has been loaded in a browser with the Turnstile widget on the sign-in page. `templates/layout.html` has inline scripts and a background style. htmx injects an indicator style unless `inlineStyleNonce` is set, and its defaults leave `allowEval` and `allowScriptTags` on. The templates do not use `hx-vals` or `hx-on`. `template.HTML` is limited to icons, the QR SVG, and time elements.

Proposed policy, with a per-response nonce on the inline script and style, and the same nonce passed to htmx:

```
default-src 'self';
script-src 'self' 'nonce-...' https://challenges.cloudflare.com;
style-src 'self' 'nonce-...';
frame-src https://challenges.cloudflare.com;
connect-src 'self' https://challenges.cloudflare.com;
img-src 'self';
object-src 'none';
base-uri 'self';
form-action 'self';
frame-ancestors 'none'
```

Set `allowEval` and `allowScriptTags` to false in the existing `htmx-config` meta when the nonce lands. Do not add `unsafe-inline` or `unsafe-eval`. Turnstile has to be checked in a browser before this is enforced. A broken widget would block sign-in.

## CSRF

Synchronizer tokens are required on unsafe methods, in addition to `http.CrossOriginProtection` and `SameSite=Lax`. The implementation, the form helper, and the rules for new endpoints are in `docs/development/CSRF.md`. Do not add a second token scheme, and do not remove the check because the origin middleware is also present. `GET` still only slides session expiry and `last_seen_at`.

## Left as designed

- Lockout after 10 failures uses a distinct message. It can confirm that an account exists and is locked, and the per-IP limit is high enough to lock one account. Impact is low: the owner signs in with a magic link, which clears the lock. Leave it.
- Audience requests are capped at 10 per 10 minutes per address per show, with no Turnstile check. That is the no-account tradeoff. A future cap belongs on the show, not on a captcha in front of a room request.
- The public queue and the display show singer names and song titles. That is the room screen.
- Changing a password does not ask for the current password. A stolen session can set a new one and sign out the other sessions. Recovery is a magic link. That is the intended tradeoff.

## Decisions

- Set `SMTP_HOST` (and the SES user and password) on the Dokku app before deploying this image. The process will exit without it.
- Accept the cookie cutover: every current session and guest browser on `next.djmagic.io` is signed out or detached.
- HSTS at Fly, with the one-week header above, is recommended and not applied.
- The client-IP trust issue needs an infrastructure design. No Go or firewall change in this pass is a fix.

## Next, by the risk that is left

1. Set SMTP and deploy. Until that image is running, production can still be on the build that logs magic-link bodies.
2. Run the production browser checklist above once, on `https://next.djmagic.io`.
3. Design the client-IP check so a Fly machine that is not the ingress cannot supply `Fly-Client-IP`. That is the remaining medium issue with a real path.
4. Add HSTS at Fly with `max-age=604800` and neither `includeSubDomains` nor `preload`.
5. Redact `/auth/` in the Fly nginx and Dokku logs.
6. Try the CSP above in a browser, including Turnstile, before enforcing it.
