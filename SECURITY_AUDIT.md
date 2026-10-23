# Security audit

Date: 2026-10-23. Scope: this repository (Go server, templates, static assets, auth, catalogs, shows, audience flow, admin, deploy notes). Method: source review of request handling, cookies, headers, templates, SQL, uploads, and `govulncheck` against Go 1.27.0. No production traffic was probed.

## CSP and CSRF

**Content-Security-Policy is set, and it only stops framing.** There is no `<meta http-equiv="Content-Security-Policy">` tag. Every response gets one header from `securityHeaders` in `middleware.go`:

```
Content-Security-Policy: frame-ancestors 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: same-origin
```

`frame-ancestors 'none'` is clickjacking protection for current browsers. The policy does not restrict scripts, styles, images, connections, frames, form targets, or `base-uri`. Inline scripts in `templates/layout.html` and the Turnstile loader (`https://challenges.cloudflare.com/turnstile/v0/api.js` from `templates/partials/turnstile.html`) would need nonces and an explicit `script-src` before a stricter policy can be turned on. `TestSecurityHeaders` locks the current value in.

Also missing from the app: `Strict-Transport-Security`, `Permissions-Policy`, and `X-Frame-Options` (the last is only a fallback for clients that ignore CSP).

**Forms do not use CSRF tokens.** No hidden field, cookie, or `csrf` middleware token is rendered. State-changing routes are `POST`, and `routes()` wraps the mux with Go's `http.CrossOriginProtection` (`main.go`). That middleware rejects unsafe methods when `Sec-Fetch-Site` is anything other than `same-origin` or `none`. If that header is absent, it compares the `Origin` host to the `Host` header. `TestCrossOriginPostRejected` covers a cross-site `Sec-Fetch-Site`.

That is real CSRF defense for browsers since 2023, and it is stricter than `SameSite` alone: a sibling subdomain (`Sec-Fetch-Site: same-site`) is rejected because the origin host does not match. It is not a synchronizer token. Go's own implementation fails open when both `Sec-Fetch-Site` and `Origin` are missing (old browsers and non-browser clients), and when `Origin`'s host matches `Host` it does not check the scheme. The standard library comment says sites should pair that with HSTS. This app does not set HSTS.

Session and guest cookies are `SameSite=Lax`, `HttpOnly`, and `Secure` when `ENV` is not `dev`. Current browsers do not send `Lax` cookies on cross-site `POST`, so a classic forged form does not carry the session even if the origin check were skipped. Destructive actions (delete account, delete catalog, change role, impersonate) rely on that pair of controls. Typing the account email to confirm deletion stops accidents; it is not a CSRF secret.

No handler changes state on `GET`, `HEAD`, or `OPTIONS`, aside from sliding session expiry and `last_seen_at`. Post-login redirects go through `safeNext`, which allows only a single-slash local path.

## Findings

| Severity | Location | Finding |
| --- | --- | --- |
| High | `go.mod` (`go 1.27.0`), `Dockerfile` | Standard library is Go 1.27.0. `govulncheck ./...` reports 13 reachable stdlib issues fixed in 1.27.2 or 1.27.3. The ones that match this app's behavior: HTML template context bugs (`GO-2026-6599`, `GO-2026-6600`) on `template.ExecuteTemplate`; MIME header limits (`GO-2026-6608`) on catalog uploads and form parsing; unbounded `Range` parsing (`GO-2026-6609`) on `/static/` and the favicon; HTTP/2 server crash and memory issues if a client speaks HTTP/2 to the process; outbound TLS trust (`GO-2026-6809`) on Turnstile and SMTP. Public HTTP/2 server bugs are less exposed today because Fly terminates TLS and Dokku proxies HTTP/1, but the template, MIME, Range, and outbound TLS issues are on paths this binary uses. Upgrade the toolchain and the image to Go 1.27.3 or newer. `golang.org/x/crypto/openpgp` is required indirectly and is not called. |
| High | `mail.go` `consoleMailer.Send`, `main.go` `run`, `config.go` `validate` | With `SMTP_HOST` empty, magic-link emails are logged in full, including the raw token. Production startup only warns. `DEPLOY.md` says the Dokku app does not have `SMTP_HOST` yet, so sign-in links for `next.djmagic.io` land in `dokku logs`. The token is a 10-minute bearer credential. App request logs redact `/auth/{token}`; this mailer path does not. Require SMTP in production, or log only the recipient and subject. |
| Medium | `account.go` `handleAccountPasswordRemove` | Setting a password revokes every other session. Removing a password only clears `password_hash`. A session created with a stolen password keeps working. Revoke other sessions here the same way `handleAccountPassword` does. |
| Medium | `ratelimit.go` `clientIP`, `config.go` | `TRUSTED_IP_HEADER` (production: `Fly-Client-IP`) is trusted on every connection. The header is safe only when every path to the process is a proxy that overwrites it. Dokku also serves `djm.box`. Anyone who can reach the origin can pick the address used for login, magic-link, upload, feedback, and song-request limits. Ignore the header unless the peer is the proxy, or do not publish a vhost that bypasses Fly. |
| Medium | `middleware.go` `securityHeaders` | CSP does not constrain script execution. Pages include inline scripts and load Turnstile from Cloudflare. htmx 2.0.11 (`static/htmx.min.js`) ships with `allowScriptTags` and `allowEval` enabled and swaps HTML with `innerHTML`. Templates go through `html/template`, so user text is escaped today. A template bug or a raw `template.HTML` mistake would run as script, and this header would not stop it. After the Go upgrade, add a nonce-based `script-src` (self plus `https://challenges.cloudflare.com`), `object-src 'none'`, `base-uri 'self'`, and `form-action 'self'`. |
| Low | `auth.go` `handlePasswordLogin` | Ten failures set `locked_at`. The locked response is different from "email or password is incorrect", so a locked address can be distinguished. The same ten tries are an account-lock denial of service until the owner uses a magic link. The IP limiter is 10 attempts per minute, which is enough to lock one account. |
| Low | `audience.go` `placeAudienceRequest` | Song requests are unauthenticated and have no Turnstile check. The cap is 10 requests per 10 minutes per client address per show. A pool of addresses can fill a queue. Login and magic links are the only Turnstile gates. |
| Low | `render.go` `renderBlock` | HTML responses do not set `Cache-Control`. Account, admin, and queue pages are personalized. A shared cache that stores `200` HTML without `Vary: Cookie` could replay one viewer's page to another. `Cache-Control: no-store` on those responses avoids that. Audience search already sets `private` when the viewer is signed out. |
| Low | `auth.go` `setSessionCookie`, `audience.go` `setGuestCookie` | Cookies are host-only, `HttpOnly`, `Secure` outside dev, and `SameSite=Lax`. They are not `__Host-` prefixed. A cookie set with `Domain=.djmagic.io` from another subdomain (the apex still points at the Rails apps) can be sent to this host under the name `session` or `guest`. Prefixing with `__Host-` rejects a `Domain` attribute. `Secure` follows `ENV`, not the request's TLS state, which is correct behind Fly as long as production is never started with `ENV=dev`. |
| Low | `auth.go` `sendMagicLink`, `middleware.go` `logPath` | The sign-in secret is the URL path `/auth/{token}`. `GET` deliberately does not consume it, so a mail scanner cannot burn it, and neither does an access log. Nginx and Fly logs of that `GET` hold a still-valid token until it expires or is posted. The app's own request log hides the path. `Referrer-Policy: same-origin` keeps the token out of the Reddit link's `Referer`. Redact `/auth/` in the proxy logs. |
| Info | `shows.go` `newShowSlug`, `audience.go` `handleAudienceQueue`, `handleDisplay` | A show's public URL is `/dj/{handle}/shows/{slug}`. The slug is derived from the name. Queue and TV display are world-readable and include singer names and song titles. Pending and rejected rows are limited to the visitor's own participant. That matches a room display. It is not an access check. |
| Info | `auth.go` `handleAccountPassword` | Changing a password does not ask for the current one. A stolen session can set a new password and drop every other session. Recovery is a magic link. That is the designed tradeoff. |

## What is in good shape

Authentication material is handled carefully. Session ids, magic links, and guest cookies are 32 bytes from `crypto/rand`, and the database stores only SHA-256. Magic links last 10 minutes, one click consumes the row with a conditional update, and older links for that user are deleted first. `GET /auth/{token}` renders a confirm page so a scanner that only fetches the URL does not sign the user in. Password checks use bcrypt at the default cost, cap input at 72 bytes, and compare a dummy hash when the account has no password. New accounts and existing accounts get the same "check your email" page. Signup cannot overwrite an existing role. `safeNext` rejects scheme-relative URLs, backslashes, and control characters. Email links use `BASE_URL`, not the request `Host`.

Authorization is per owner. Catalog and show loads require `user_id` and answer 404 otherwise. Request actions load the show through that check. Audience cancel requires the request to belong to the visitor's participant. Saved songs and performances are scoped to the audience profile. Admin routes 404 for non-admins. An admin cannot change their own role, remove the last admin, or impersonate another admin. Impersonation sessions last one hour and do not slide. Stopping impersonation checks that the admin still has the admin role. Role changes and impersonation start/stop are logged.

Data handling is parameterized. GORM and `database/sql` use placeholders. Admin search escapes `LIKE` wildcards. Catalog FTS turns the query into bareword prefix terms and drops `AND` / `OR` / `NOT` / `NEAR`, so punctuation never reaches the FTS parser. `ORDER BY` is chosen from a fixed map. Uploads are capped at 100 MB, limited to 5 per user per hour, written to a temp file, and stored under keys the app generates (`fs.ValidPath` on blob keys). CSV rows are capped. Static files are served only when the name is in the embedded asset map, so `..` does not escape into templates. The container runs as uid 65532. `.env` is gitignored. Turnstile secrets are not logged. Production `validate` requires `https` `BASE_URL` and both Turnstile keys. Server errors returned to the client are a generic 500; the detail goes to the log. Foreign keys are on, and sessions and magic tokens cascade with the user.

## Suggested order

1. Build and deploy with Go 1.27.3 or newer.
2. Set `SMTP_HOST` before the site is used for real sign-in, and stop logging magic-link bodies.
3. Revoke other sessions when a password is removed.
4. Keep `Fly-Client-IP` trustworthy: no direct vhost that forwards a client-supplied copy of that header.
5. Add HSTS at the TLS edge (or in the app, understanding the app sees HTTP behind Fly) and tighten CSP once inline scripts have nonces.
