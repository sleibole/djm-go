# HTMX in this application

This note records how this application uses htmx. The reusable rules are in [HTMX.md](HTMX.md).

The sections below are the application note from 2026-10-24. Two details were completed from the handlers and from `static/htmx.min.js` (htmx 2.0.11) while moving it:

- Ctrl-click, Cmd-click, and middle-click on a boosted link stay with the browser. Shift-click is boosted in this build. `target="_blank"` and other hosts are not boosted. The Ctrl/Cmd exception does not apply to an anchor that has its own `hx-get`, including Load more results.
- When a poll stops swapping into itself and retargets `#content`, the handler also sends `HX-Reswap`. `hx-swap="none"` would otherwise discard the new HTML.

## Answer

Ordinary links and form posts are boosted. `<body>` in `templates/layout.html` sets `hx-boost="true"` and `hx-target="body"`. htmx requests the URL, the server returns a full document, and htmx swaps that document's body and title. The window does not load a new document. Head scripts, including `app.js` and `reorder.js`, keep running.

An element with its own `hx-get` or `hx-post` is not boosted. Those requests still swap a fragment. `render` in `render.go` returns the `content` block for a fragment request, and the full `layout` when any of these is true:

- `HX-Boosted` is true
- `HX-History-Restore-Request` is true, even if `HX-Request` is also set
- `HX-Target` is `body`
- none of the HTMX headers are present

`target="_blank"` and other hosts stay with the browser, as do Ctrl-click, Cmd-click, and middle-click. Shift-click is boosted in htmx 2.0.11. The venue Display link and the Reddit link open a new tab. Every form keeps `method` and `action`, and the CSV upload keeps `enctype="multipart/form-data"`, so a browser without JavaScript still navigates and submits.

## What counts as a window reload

A boosted click or submit replaces the body. The address bar follows the response, including a `303` that XMLHttpRequest follows inside the request. A fragment swap replaces one element and leaves the address bar alone unless the response sets `HX-Push-Url`.

A real document load still happens for a new tab, an external site, a Ctrl-click or Cmd-click or middle-click on a boosted link, and a browser with JavaScript off. Nothing in the app sets `HX-Redirect` or `HX-Refresh`. Those headers assign `location.href` or call `location.reload()` in htmx 2.0.11.

## History

The layout's `htmx-config` sets `historyRestoreAsHxRequest` to false. The library default is true, which would mark a cache-miss GET as `HX-Request` and, without the `render` rule above, return only the content block. `historyCacheSize` stays at 10. `refreshOnHistoryMiss` stays false: in this build, true means `location.reload(true)` on a miss.

A cache hit restores the saved body from `sessionStorage` (`htmx-history-cache`). A miss is an XHR GET with `HX-History-Restore-Request: true` and no `HX-Request`. The response is a full document. htmx takes its body and title and swaps them in. Back and Forward do not reload the window.

`hx-history="false"` on any element skips the snapshot of the page being left. It does not turn history off. The next Back to that URL is a cache miss and still swaps through htmx. It is set on the content of:

- `templates/signin.html` — email value, `next` URL, and a password field
- `templates/signin_sent.html` — the address the link was sent to, and in dev the raw magic URL
- `templates/magic_link.html` — the address being signed in
- `templates/account.html` — the signed-in email is in the page text
- `templates/admin_users.html`, `templates/admin_user.html`, and `templates/admin_feedback_item.html` — other people's emails and feedback text

Shows, catalogs, queues, saved songs, and performances stay cached. Stage names and song titles are the working set of the app, not credentials. The feedback form stays cached so Back can restore a draft. While an admin is impersonating, the banner is part of the body, so snapshots of ordinary pages include that email. That is limited to the admin's tab.

`hx-history="false"` does not stop htmx from writing the current path to `htmx-current-path-for-history`. A magic-link token in `/auth/{token}` can still appear there, and the browser's own session history already has that URL. The token is single-use and expires in 10 minutes. History is not disabled app-wide to hide that one key.

## Fragment requests

These leave the document in place. `hx-target` and `hx-swap` come from the nearest ancestor that sets them. `this` means the element that owns the attribute.

| Control | Where | Target and swap | Server returns |
| --- | --- | --- | --- |
| Host queue poll | `#show-live` in `templates/show.html`, every 5s while `djmShouldPoll()` | itself, `outerHTML` | `show_live` fragment, while the show is active |
| Show no longer live | same poll | response sets `HX-Retarget: #content`, `HX-Reswap: innerHTML show:top`, and `HX-Push-Url` | the show page's content block. The header stays |
| Approve, reject, done, skip, start, remove | forms inside `#show-live` | inherit `#show-live` / `outerHTML` | the live fragment, including `409` with the error text |
| Drag reorder | `static/reorder.js` uses `fetch`, then `htmx.trigger(live, "refresh")` | the poll target above | `204`, or `409` which restores the old order |
| Participant suggestions | name field on the host add form | `#participant-suggestions` | `<option>` fragment |
| Host, catalog, and audience search | the search form, including Enter | the results element | the results fragment. The form's `hx-get` beats boost |
| Load more results | the audience anchor, which has its own `hx-get` | `#audience-results-body`, `beforeend` | extra rows. Middle-click and Open in New Tab use `href` and load a document. In htmx 2.0.11, Ctrl-click and Cmd-click on this anchor issue the `hx-get`, because the Ctrl/Cmd exception is only on boosted anchors |
| Audience queue poll | `#audience-live` | itself, `outerHTML` | `audience_live` when `HX-Target` is `audience-live` |
| Cancel my request | form inside that region | `#content` | `303` followed inside the request; the content block swaps into main |
| Venue display poll | `#display-live` | itself, `outerHTML` | `display_live` when `HX-Target` is `display-live` |
| Save / unsave heart | `templates/partials/memory.html` | the form itself, `outerHTML` | one heart form |
| Remove a saved song | `templates/saved_songs.html` | `#content`, explicit | the saved-songs content block |
| Catalog build poll | "Processing…" on the catalog page | `hx-target="this"` and `hx-swap="none"` while the build runs | `204` |
| Catalog build finished | same poll | `HX-Retarget: #content`, `HX-Reswap: innerHTML show:top`, `HX-Push-Url` to `?built=1` | the catalog content block, with the ready message |

The CSV upload form is boosted, so the `303` after a successful upload swaps the catalog page into the body. It sets `hx-encoding="multipart/form-data"` because a file input alone does not make htmx send `FormData`. `enctype="multipart/form-data"` stays for browsers without JavaScript.

`static/app.js` looks up the mobile nav, the password toggle, and magic-link auto-submit at event time, and runs them again from `htmx:afterSettle` and `htmx:historyRestore`. A swapped sign-in page loads the Turnstile script if it is not already on the document. `reorder.js` is in the layout head, so drag handles work after a boosted arrival on a show.
