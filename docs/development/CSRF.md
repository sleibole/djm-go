# CSRF

Unsafe HTTP methods (`POST`, `PUT`, `PATCH`, `DELETE`, and anything else that is not `GET`, `HEAD`, `OPTIONS`, or `TRACE`) must carry this browser's CSRF token. `csrfProtect` in `csrf.go` checks that before the handler runs. Handlers do not check the token themselves.

`http.CrossOriginProtection` still runs first. It rejects unsafe requests whose `Sec-Fetch-Site` is not `same-origin` or `none`, and when that header is missing it compares the `Origin` host to `Host`. Go allows the request when both headers are missing. The token covers that gap. `SameSite=Lax` on the cookies is another layer, not a replacement.

There is no route allowlist. Public, anonymous, and `/api/` paths are not exempt. This app has no bearer-token API and no inbound webhook.

## Token lifecycle

The token is a 32-byte value from `crypto/rand`, encoded with base64url. It is stored on the server and compared with SHA-256 and `subtle.ConstantTimeCompare`. It is not written to the log.

Logged-in browsers use the `csrf_token` column on their `sessions` row. The session cookie is the binding. Logging in, redeeming a magic link, impersonating, and returning to admin all call `startSession`, which creates a new session and a new token. The previous anonymous token is deleted. Logging out deletes the session, which deletes its token. Other sessions ended by a password change lose their tokens with those rows. The current session keeps its token. A session created before this column existed gets a token on its next request.

Logged-out browsers get a row in `csrf_sessions` and an `HttpOnly` cookie named `csrf` (development) or `__Host-csrf` (everywhere else). The cookie holds a random id. The database stores the hash of that id and the separate form token. The cookie uses `Path=/`, no `Domain`, `SameSite=Lax`, and `Secure` outside development, same as the session cookie. It lasts 30 days and slides forward when less than half of that remains. Expired rows are deleted by the cleanup job.

The token is reused across tabs, reloads, history restores, and repeated submits. It is not burned after one POST. A second tab with the same cookies can submit with the same token.

## Adding a form

In any template, including a partial rendered with its own data:

```html
<form method="post" action="/feedback">
  {{csrfField}}
  <!-- fields -->
  <button type="submit">Send</button>
</form>
```

`{{csrfField}}` expands to:

```html
<input type="hidden" name="csrf_token" value="...">
```

The value is escaped. Do not build that input by hand, and do not put the token in a URL.

On a multipart form the hidden field has to be the first control. The middleware reads that part and leaves the rest of the body, including the file, for the handler. The read is capped at the catalog upload limit, so a huge first part cannot be buffered without bound. A token in `X-CSRF-Token` skips that read, which is what htmx does.

`render` and `renderBlock` load or create the token and pass it to the template. Handlers do not add it to the page data.

Full HTML responses include this in `<body>`, ahead of the page:

```html
<meta name="csrf-token" content="...">
```

A boosted navigation swaps the body, so the meta tag updates when the token rotates. A fragment swap does not include the layout. The token is stable for the session, and the fragment's forms include `{{csrfField}}`.

## HTMX

`static/app.js` listens for `htmx:configRequest` on `document` and sets `X-CSRF-Token` on unsafe same-origin requests. The listener is registered once. Controls do not set `hx-headers` or `hx-vals` for CSRF.

A rejected token returns 403 with `X-CSRF-Rejected: 1`. htmx is configured to swap other 4xx responses, so `htmx:beforeSwap` cancels that swap and shows "Reload the page and try again." A browser without JavaScript gets the same message and a Reload button. The client does not retry the POST.

## fetch

`static/reorder.js` is the only `fetch` that changes state. It sends the header from `djmCSRF.headers(url, headers)` and `credentials: "same-origin"`. Use that helper for a new same-origin request. It leaves the headers alone when `url` is another origin, so the token is not sent off-site. A 403 restores the queue and shows the message. It does not retry.

```javascript
const url = "/shows/1/requests";
fetch(url, {
  method: "POST",
  credentials: "same-origin",
  headers: djmCSRF.headers(url, { "Content-Type": "application/x-www-form-urlencoded" }),
  body: new URLSearchParams({ song_title: title }),
});
```

## Anonymous visitors and sign-in

The token does not require an account. The feedback form, a song request, and the sign-in form all use the anonymous cookie. The page that renders the form sets the cookie, so the POST does not need an extra round trip. That cookie is not a guest identity and does not create a song request. Reloading the page reuses the same `csrf_sessions` row. Expired rows are deleted by the hourly cleanup. An unknown or expired cookie does not match any token.

`GET /auth/{token}` does not sign the user in. The confirm page has a normal POST form. Redeeming the link, signing in with a password, and impersonating replace the session and the token. The old anonymous token no longer matches.

## What a failed check returns

Missing, wrong, expired, and cross-session tokens get **403** and this text:

```html
<!doctype html><meta charset="utf-8"><title>Forbidden</title><p role="alert">Invalid or missing CSRF token. Reload the page and try again.</p>
```

The handler does not run. `Cache-Control` is `private, no-store`. The log line is `csrf rejected` with the method and path. The token and the cookie value are not logged.

Accepted places for the token:

- `X-CSRF-Token` header. If this is present, the body is not read for a token.
- `csrf_token` in an `application/x-www-form-urlencoded` body.
- `csrf_token` as the first part of a `multipart/form-data` body.

A query string, a JSON body, and any other content type are not accepted. Sending the header and a matching cookie from another browser fails.

## CSRF is not idempotency

The token says the request came from a page this app rendered for this browser. It does not say the operation should happen only once. Two tabs can both submit. A double click can both submit.

Use a database constraint, a conditional update, or an operation-specific one-time value when once is the requirement. Magic links already do that. Do not clear the CSRF token to get that behavior.

## GET

CSRF checks do not make a state-changing GET safe. Keep changes on unsafe methods. Today, GET still only slides session expiry and `last_seen_at`, and it clears a session or guest cookie the server does not recognize. `GET /auth/{token}` does not consume the link.

## Caching

`render` sets `Cache-Control: private, no-store` when the HTML contains the token (`csrf-token` or `csrf_token`). That covers full pages, including the home page, docs, feedback, sign-in, and anonymous show pages, and any fragment that includes `{{csrfField}}`. `private` tells a shared cache not to store it. `no-store` tells the browser not to reuse the document after login or logout replaces the token. `Vary: Cookie` is still set and is not the control.

A signed-out catalog search fragment has no token. When it is for the current catalog version it keeps `Cache-Control: private, max-age=300`. A stale version sends no cache header. Save buttons add the token, and those results are `private, no-store`.

Static files keep their own policy: a hashed URL is `public, max-age=31536000, immutable`, and a stale hash is `no-cache`. `robots.txt`, `sitemap.xml`, and `/healthz` do not start a CSRF session.

There is no service worker and no offline HTML cache. The install path is a home-screen dialog only. htmx history snapshots live in `sessionStorage` and are not controlled by `Cache-Control`. A full browser Back on a document may skip the back-forward cache because of `no-store` and load the page again, which is what supplies the current token.

## More than one tab

Tabs share cookies. They can all submit with the token from the latest page this browser loaded. Login, logout, and impersonation replace that token. A tab that still has the previous page gets a 403 on its next unsafe request until it loads a fresh document. That is fail-closed. The app does not retry the POST.

## Tests

`csrf_test.go` covers acceptance, rejection, cross-session use, safe methods, other unsafe methods, anonymous and logged-in submits, login, logout, expiry, cleanup, impersonation, session replacement, templates, htmx, multipart uploads, the multipart size cap, the origin check, and log redaction.

The test browser sends `X-CSRF-Token` on unsafe requests. Set `browser.skipCSRF` to omit it. A header you set yourself is left as-is.

```sh
go test -count=1 -run 'CSRF|CrossOrigin' .
```

## Mistakes

- Writing `<input type="hidden" name="csrf_token">` instead of `{{csrfField}}`.
- Putting the token in a link or a query parameter.
- Skipping the check because the route is public, or because the caller sent `HX-Request`.
- Adding a second token for one feature.
- Treating a 403 as a cue to retry a POST automatically.
- Putting `{{csrfField}}` after the file input on a multipart form that will be submitted without the header.
- Exempting a route without writing the reason down here.
