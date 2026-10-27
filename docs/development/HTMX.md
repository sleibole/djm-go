# HTMX development guide

A practical guide for building multi-page, server-rendered applications with htmx. It is written for people and for coding agents. Copy it into another Go project and delete the last section.

The recommendations are opinions. The sentences that describe what htmx does were checked against htmx 2.0.11 and the htmx 2.x documentation on htmx.org. Where those two disagree, this guide follows the 2.0.11 build and says so. Re-check the installed file before relying on a behavior that a later 2.x release might have changed.

## How to read this

htmx is a small library that issues HTTP requests from HTML attributes and swaps the HTML response into the page. The server still owns application state and rendering. The browser still owns navigation, history, focus, and forms, except where an attribute deliberately takes over.

Prefer that shape:

- Render HTML on the server. Return a document or a fragment, not JSON that the client turns into markup.
- Use htmx to enhance a traditional application. A link that works as a link and a form that works as a form are the baseline.
- Keep behavior in attributes on the element that needs it. Add JavaScript when attributes and the browser cannot express the interaction.
- Choose the boring version. An explicit target and a small handler are easier to debug than a convention that only holds if every future change remembers it.

Two labels are used when the distinction matters. **htmx** means the library does this. **Prefer** means a recommendation, and a reasonable exception follows when there is one.

## Contents

1. [Request and response architecture](#1-request-and-response-architecture)
2. [Navigation and page swaps](#2-navigation-and-page-swaps)
3. [Forms and progressive enhancement](#3-forms-and-progressive-enhancement)
4. [Targeted updates and partials](#4-targeted-updates-and-partials)
5. [JavaScript integration](#5-javascript-integration)
6. [History, caching, and security](#6-history-caching-and-security)
7. [Accessibility and browser behavior](#7-accessibility-and-browser-behavior)
8. [Performance and reliability](#8-performance-and-reliability)
9. [Testing and debugging](#9-testing-and-debugging)
10. [Common anti-patterns](#10-common-anti-patterns)
11. [Implementation checklist](#11-implementation-checklist)
12. [Version notes](#12-version-notes)

## 1. Request and response architecture

Every interaction is one of four HTTP exchanges. The response body has to match the exchange. Mixing them up is the defect behind most "the page shell disappeared" bugs.

| Exchange | What the browser does | What the server returns |
| --- | --- | --- |
| Ordinary navigation | A full document load. No htmx headers. | A complete HTML document. |
| Boosted navigation | htmx requests the URL and swaps the response into `<body>`. The window stays. | A complete HTML document. htmx keeps the body and the title, and drops `<head>`. |
| Fragment request | An element with `hx-get`, `hx-post`, or another verb swaps one target. | The HTML for that target, and nothing around it. |
| History miss | Back or Forward found no snapshot. htmx GETs the URL and swaps it into the history element, which is `<body>` unless `hx-history-elt` says otherwise. | A complete HTML document. |

Background polling and other timed updates are fragment requests. They are called out because they run without a click, so a wrong response is especially visible: a poll that returns a full document will paint a page inside a widget, or replace the widget with a layout, every few seconds.

### Request headers

htmx sends these on its own requests. Ordinary document loads send none of them.

| Header | Set when |
| --- | --- |
| `HX-Request: true` | Almost every htmx request. Absent on a history miss when `historyRestoreAsHxRequest` is false. The library default is true, so a miss then sends this header too. |
| `HX-Boosted: true` | The element was boosted. A fragment trigger does not send it. |
| `HX-Target` | The `id` of the resolved target element. It is the id, not the `hx-target` selector. An element with no id sends nothing useful. |
| `HX-History-Restore-Request: true` | The request is a history-cache miss. |
| `HX-Current-URL` | The URL currently shown. Useful in logs. Do not authorize from it. |
| `HX-Trigger` / `HX-Trigger-Name` | Id and name of the element that fired the request, when those attributes exist. |

`HX-Request` alone does not mean "send a fragment." Boosted navigation and, under the library default, history restoration are htmx requests that need a full document.

### Choosing the response

Treat "full document" as the default. Return a fragment only when this request is an htmx fragment swap.

```go
// fullDocument reports whether the client needs a complete HTML page.
// A normal browser navigation sends no HX-Request. Boosted navigation and
// history restoration need the shell as well, even when HX-Request is set.
func fullDocument(r *http.Request) bool {
    if r.Header.Get("HX-Request") != "true" {
        return true
    }
    if r.Header.Get("HX-Boosted") == "true" {
        return true
    }
    if r.Header.Get("HX-History-Restore-Request") == "true" {
        return true
    }
    return false
}
```

Set `historyRestoreAsHxRequest` to false when the server uses `HX-Request` to decide. The htmx docs recommend that, and the library default does the opposite. Keep the history-restore check anyway, so a deploy that forgets the config still returns a document.

```html
<meta name="htmx-config" content='{"historyRestoreAsHxRequest":false}'>
```

`HX-Target` is a weak signal. It is missing when the target has no id, and it is inherited when an ancestor set `hx-target`. Branching on `HX-Target == "body"` is reasonable only after the body element has `id="body"` and the team knows that value means "this request aimed at the body." Prefer the three checks above.

Render into a buffer and set the status after the template succeeds, so a template error cannot emit a half-written fragment with a 200.

The same URL often has two representations. If any HTTP cache might store the response, send `Vary: HX-Request` (and vary on any other request header that changes the body). A cache that ignores `Vary` will serve a fragment as the document, or a document as the fragment. Personalized HTML should usually send `Cache-Control: no-store` or `private` instead of being shared. See [History, caching, and security](#6-history-caching-and-security).

### Status codes

htmx 2 decides whether to swap from `htmx.config.responseHandling`. The first matching rule wins. In 2.0.11 the default is:

- `204` — do not swap, and do not treat it as an error. Use this when the DOM should stay as it is.
- `200`–`399` — swap.
- `400`–`599` — do not swap, and fire `htmx:responseError`.

A validation response that uses `422` is ignored by the default, and the user sees nothing. Two honest options:

- Return `200` with the form rendered again, errors included. Works with the default.
- Return `422` and configure a rule that swaps it. Put that rule before the `[45]..` rule, because the first match wins.

```html
<meta name="htmx-config" content='{
  "responseHandling":[
    {"code":"204","swap":false},
    {"code":"422","swap":true},
    {"code":"[23]..","swap":true},
    {"code":"[45]..","swap":false,"error":true}
  ]
}'>
```

Prefer opting in to the specific codes whose bodies are HTML you mean to show, such as a `409` that re-renders one region with a conflict message. A blanket "swap every 4xx and 5xx" will dump an error page into a small target. `htmx:beforeSwap` can set `shouldSwap` for one response. That is a page-level hook, not a place to hide a missing server decision.

### Redirects

`XMLHttpRequest` follows a `301`, `302`, or `303` inside the same call. htmx sees the final response. Headers on the redirect itself, including `HX-Redirect`, `HX-Push-Url`, and `HX-Retarget`, are not applied.

A boosted request pushes the final URL (path and query) into the address bar. A `303` after a POST is the right way to move to another page, for a normal submit and for a boosted one. The destination must be able to render a full document for a plain GET. Do not depend on the browser repeating `HX-*` headers on the followed request. Browsers differ, and the safe handler does not care: a normal GET of that URL is already a full document.

```go
func (a *app) createItem(w http.ResponseWriter, r *http.Request) {
    item, problems, err := a.saveItem(r)
    if err != nil {
        http.Error(w, "internal error", http.StatusInternalServerError)
        return
    }
    if len(problems) > 0 {
        // 200 so the default response handling swaps the form.
        a.renderFragment(w, r, http.StatusOK, item, problems)
        return
    }
    http.Redirect(w, r, "/items/"+item.ID, http.StatusSeeOther)
}
```

Use a fragment response when the user stays on this screen. Use a redirect when the next URL is a different resource. `HX-Redirect` is a separate tool: it assigns `location.href` and loads a new document. See [Targeted updates](#response-headers-that-change-the-swap).

## 2. Navigation and page swaps

### When to boost

`hx-boost="true"` makes same-host links and forms inside that element into htmx requests. The default target is `<body>`, the default swap is `innerHTML`, and the URL is pushed. Without JavaScript the link or form still navigates. That is the feature.

Boosting is a tradeoff, not a default to apply to every `<body>`.

What you gain: the document, its head scripts, and in-memory state survive. Moving between pages feels faster because the browser does not parse a new document. Forms and links keep their native attributes.

What you give up:

- The new page's `<head>` is discarded. A script or stylesheet that exists only in that head does not load.
- JavaScript globals and listeners from earlier pages are still there.
- History snapshots start to matter, including authenticated HTML left in the tab.
- Attribute inheritance starts at the boosted ancestor. A target set there applies to every descendant request that does not override it.
- htmx's own team treats boost as the odd feature. Some of them prefer ordinary navigations because browsers are already fast. Both positions are reasonable.

Prefer boost on a region that is actually navigation: the site header, a layout wrapper around ordinary links, or one form that posts and then shows a new page. Prefer a real document load when the next page depends on a fresh JavaScript environment, a new CSP nonce, or a head asset the current document does not have. Prefer an explicit `hx-get` when the interaction only updates part of the page.

`hx-boost="false"` on a child turns boost off for that subtree. An element that already has `hx-get`, `hx-post`, or another verb is not boosted. The verb wins, which is what you want for a search form inside a boosted layout.

### Do not aim every request at the body

A boosted element with no `hx-target` already swaps `<body>`. Setting `hx-target="body"` on `<body>` as well does not change boost. It does change everything else. htmx inherits the nearest `hx-target`, so a poll, a typeahead, and a button that forgot its own target all aim at the body and send `HX-Target` for the body's id.

Prefer `hx-boost="true"` without an ancestor `hx-target`. Give every fragment request its own `hx-target` and `hx-swap`. Inheritance is useful one level up, on the widget that owns a group of buttons, where a reader can see the target by looking at the parent. It is a poor fit on `<body>` or `<html>`.

htmx always uses `innerHTML` when the target is `<body>`, including when the swap says `outerHTML`. A body swap cannot update attributes on the `<body>` element itself. Put page state on an element inside the body if a swap needs to change it.

### Target, swap, and select

- `hx-target="this"` is the element that has the attribute. A CSS selector is resolved from that element. `#id` is the usual choice. `closest form` and similar relative selectors are available.
- `innerHTML` replaces the children. The target element, its listeners, and its htmx attributes remain. The response must not include the target's own tag, or you nest it.
- `outerHTML` replaces the element. Use it when the response includes the element's tag, especially when a poll must update its own trigger attributes. The response's root should carry the same id.
- `beforeend` / `afterend` / `beforebegin` / `afterbegin` insert. `beforeend` is the usual "load more" swap.
- `delete` removes the target. `none` ignores the body. In 2.0.11, `none` returns before insertion, so a later `HX-Retarget` still inserts nothing unless the response also sends `HX-Reswap`.
- `hx-select` keeps matching nodes from a larger response. Prefer returning only the fragment. Use `hx-select` when one endpoint must serve both a full document and a small update and you have not split them yet.

If the selector matches nothing, htmx fires `htmx:targetError` and the swap throws. A missing id is a bug, not a silent no-op.

### URL and title

Boosted navigation pushes the final response URL and copies a `<title>` from a full document into the existing document. Fragment requests do not change the URL or the title unless you ask:

- `hx-push-url="true"` pushes the request URL. A value of a path pushes that path. `"false"` suppresses a push.
- `hx-replace-url` replaces the current history entry instead of adding one.
- The response may send `HX-Push-Url` or `HX-Replace-Url`. `"false"` suppresses.

Push a URL only when a person should be able to bookmark, share, or reload it, and the server can render a full document for that URL. A new tab has no htmx headers.

A fragment with no `<title>` leaves the current title alone. A full document with a title replaces it. `ignoreTitle` in config, or on a response-handling rule, skips that update. A title that never changes across boosted pages is a defect, because history and tabs then all show the first page's title.

### What stays a real navigation

htmx 2.0.11 boosts an anchor when all of these hold: the link has an href, the href does not start with `#`, `location.hostname` equals the link's hostname, and `target` is empty or `_self`. A `target="_blank"` link is left alone. A form with `method="dialog"` is left alone. Local `#fragment` links are left alone.

On a boosted anchor, Ctrl-click and Cmd-click return before htmx calls `preventDefault`, so the browser opens a new tab. Middle-click is an `auxclick`, which this build does not listen for, so the browser opens a new tab there too. Shift-click is not special-cased: htmx calls `preventDefault` and handles it as an in-place visit.

That Ctrl/Cmd exception is only on the boost path. In 2.0.11 the check requires the element to be boosted. An anchor with its own `hx-get` or `hx-post` still has a real `href`, so the click handler calls `preventDefault` and then issues the htmx request. Ctrl-click and Cmd-click on that anchor do not open a new tab. Middle-click and the browser's Open in New Tab command still use `href`, because they never hit that click listener. Check both paths again after upgrading. This is the kind of detail a patch release can change.

`selfRequestsOnly` defaults to true and then refuses a request whose origin differs, including the same hostname on another scheme or port. A link that passed the hostname check and then fails the origin check can swallow the click. Mark cross-origin links with a full URL the boost check will reject, or set `hx-boost="false"` or `target="_blank"` on them.

Use a real document load, `HX-Redirect`, or `HX-Refresh: true`, when the next step has to tear the window down: leaving for another site, completing a login flow that depends on a fresh document, or recovering from a response the current page cannot safely swap. `HX-Redirect` assigns `location.href`. `HX-Refresh: true` calls `location.reload()`. `HX-Location` does not. It starts another htmx request. Prefer an ordinary `303` for in-app "go to this page" and reserve `HX-Redirect` for a true load.

### Stale shells

A body swap replaces the shell, so the header, the navigation, and the signed-in control update together. A fragment swap does not. If a fragment request can change something the shell shows — signing in, signing out, an unread count, a banner — update the shell in the same response or navigate. Out-of-band swaps exist for this. A second request that "usually" refreshes the header will be the one that fails open.

Boost does not refresh `<head>`. Permanent CSS and JS belong in the layout that was loaded with the document. Page-specific behavior has to live in that already-loaded script, or in markup, not in a `<script>` in the arriving head.

## 3. Forms and progressive enhancement

A form keeps a native `method` and `action`. htmx attributes repeat them when they need to: `method="post"` with `hx-post`, `method="get"` with `hx-get`. Without JavaScript, the browser submits the form. With JavaScript, the same URL and method run. An endpoint that only exists as `hx-post`, with no `action`, is a script-only form. That is sometimes a fair trade for a widget that has no non-JS equivalent. It is a poor trade for sign-in, search, create, and delete.

### GET and POST

GET reads. The query string is the resource. Boosted and unboosted GET forms navigate to that URL, so the results are shareable and refresh does not surprise anyone.

POST changes state. After a successful POST that moves the user to another resource, respond with `303` and a `Location`. Refresh then repeats a GET. After a successful POST that stays on the screen — a toggle, an inline edit, a row action — respond with the updated fragment and do not redirect.

htmx 2 encodes `GET` and `DELETE` parameters in the URL, not the body (`methodsThatUseUrlParams`). A `DELETE` from a button sends its fields on the query string. The CSRF check does not accept `csrf_token` from the query string. Prefer a small POST form for destructive actions so the hidden field is in the body and the button works without JavaScript. An htmx `DELETE` still sends `X-CSRF-Token` when the shared listener is loaded. `DELETE` is fine when the action is idempotent and the URL already names the resource.

Disabled fields are not submitted. `hx-disabled-elt` disables after htmx has already collected the values, so the click still includes the submit button's name and value.

### Validation

The server validates again. Client checks are a courtesy.

htmx asks a form for `checkValidity()` before it sends, unless the form has `novalidate` or the submit control has `formnovalidate`. In 2.0.11 it does not call `reportValidity()` unless `reportValidityOfForms` is true, so the request is blocked and the browser may not show the message or move focus. The htmx docs say to turn that flag on. Do that.

```html
<meta name="htmx-config" content='{"reportValidityOfForms":true}'>
```

Non-form elements do not validate unless they set `hx-validate="true"`.

Render server errors next to the fields, in the fragment you swap back into the form. Put the summary in an element with `role="alert"` or `aria-describedby` on the inputs. A `200` with that HTML is the path that works without extra config. A `422` requires the response-handling rule from section 1, and a test that the client is actually configured to swap it.

### Search, Enter, and debounce

Enter submits the form. If the htmx attributes live only on the text field, Enter still submits the form, and a boosted form will swap `<body>`. Put `hx-get`, `hx-target`, `hx-swap`, and any `hx-push-url` on the `<form>` so Enter, the button, and no-JS submission are the same request.

```html
<form method="get" action="/items" hx-get="/items"
      hx-target="#results" hx-swap="innerHTML" hx-push-url="true"
      hx-trigger="input changed delay:300ms, submit"
      hx-sync="this:replace">
  <label for="q">Search</label>
  <input id="q" type="search" name="q">
  <button type="submit">Search</button>
</form>
<div id="results" aria-live="polite"></div>
```

`input` bubbles, so the form hears typing in its fields. `changed` skips no-op events. `delay:300ms` waits for a pause. `submit` covers Enter and the button. `hx-sync="this:replace"` aborts the request already in flight so a slow response cannot overwrite a newer query.

A non-form control does not include the surrounding form's fields on a GET. Use `hx-include="closest form"` when a button outside the typing flow must send those fields. A form trigger does not need it.

### Overlapping requests, busy controls, and uploads

`hx-sync` is the overlap control. `replace` aborts the previous request and is the right choice for search. `queue last` keeps the latest follow-up and is a reasonable default for a button the user might hit twice. `queue first` ignores the rest. `drop` ignores new requests while one is running. htmx already queues on the triggering element when you set nothing; say it anyway on any control where the choice matters.

`hx-disabled-elt="this"` or `hx-disabled-elt="find button[type=submit]"` disables those controls for the duration. Pair it with a visible pending state. `hx-indicator` adds the class `htmx-request` during the request. htmx injects CSS that reveals `.htmx-indicator` while that class is present. That CSS is visual (`opacity` and `visibility`). It does not set `aria-busy`. Set `aria-busy="true"` on the region for the duration, or include a `role="status"` message, when the wait is something the user must perceive.

`hx-disabled-elt` is not a substitute for a server that tolerates a double submit. The request can still be sent twice if the first one finished, or from another tab.

A file input does not switch the encoding by itself. Set `enctype="multipart/form-data"` on the form so a no-JS submit sends `multipart/form-data`. htmx 2.0.11 honors that `enctype` on a `<form>`, and also honors `hx-encoding="multipart/form-data"` (which is inherited). Set the `enctype`. Add `hx-encoding` when the requesting element is not the form, or when you want the encoding to be visible next to the other htmx attributes. Older htmx only looked at `hx-encoding`. If you upgrade or downgrade, confirm which one the file you ship implements.

### CSRF

Unsafe htmx requests get `X-CSRF-Token` from the document listener in `static/app.js`. The token is the `<meta name="csrf-token">` inside `<body>`, so a body swap picks up a new value after login or logout. POST forms also include `{{csrfField}}`, which is what a no-JS submit sends. Do not put the token in `hx-headers` on `<body>`: that element is not replaced when its inner HTML is swapped. Details and the rules for new endpoints are in `docs/development/CSRF.md`.

### Fragment or navigation

Update a fragment when the task is "this region of the current page changed" and the URL is still the right URL. Navigate when the task is "the user is now somewhere else," when the shell would be stale, or when refresh should show the new resource. A create form that lands on the new item is a redirect. A create form that appends a row to a list on the same page is a fragment.

## 4. Targeted updates and partials

### Make the target obvious

Every fragment request names its target on the element a reader is looking at, or on the closest parent that groups that widget. `this` is explicit. An inherited target from `<body>` is not.

The server response is the inside of an `innerHTML` swap, or the element itself for an `outerHTML` swap. Include the id in an `outerHTML` response. Omit the wrapper for an `innerHTML` response. Pick one and test the bytes.

A fragment should still make sense to someone reading the template: a list of rows, a form, a status paragraph. Avoid a snippet that is only valid because of an unstated parent, and avoid sharing a partial between a full page and a swap if the two need different wrappers. Two small templates are cheaper than a partial full of conditionals.

### Out-of-band swaps

An element in the response with `hx-swap-oob="true"` and an id replaces the element in the document that has that id. The default OOB swap is `outerHTML`. `hx-swap-oob="innerHTML:#cart-count"` selects both the strategy and the target. The OOB element is removed from the main response before the main swap.

Use OOB to update a sibling the trigger did not aim at: a count in the header, a "load more" control that lives outside the list. Prefer one main fragment when that fragment already contains everything that changed. OOB is a second write target, and a typo in the id fires `htmx:oobErrorNoTarget` and drops the element. In 2.0.11, nested OOB elements are processed (`allowNestedOobSwaps` defaults to true).

### Polling

`hx-trigger="every 5s"` polls while the element is in the document. Stop the poll by removing the element or by returning a replacement that does not have the trigger. A poll that runs for the life of a page, hitting an endpoint that almost always returns `204`, is a cost you should have to justify.

`204` with `hx-swap` left at its default leaves the DOM alone. `hx-swap="none"` also leaves it alone, including when you later retarget, unless you send `HX-Reswap`.

A filter such as `every 5s [document.visibilityState === 'visible']` uses `eval`. It is off when `allowEval` is false. Pausing while hidden is worth doing for a lively poll. Skipping one tick is fine. Catching up with a burst of immediate requests is not.

Do not poll a region so large that each tick destroys inputs, scroll position, or open disclosures. Poll the smallest element that actually changes, and preserve user state across that replacement (see [JavaScript](#5-javascript-integration) for focus and for widgets).

### Request synchronization

Say `hx-sync` on polls that can overlap a user action on the same element. A poll and a form that both target the same node will otherwise race. `hx-sync="#panel:queue last"` on the form, pointed at the polling element, serializes them. Aborting a poll is fine. Aborting the user's save is not, so choose the queue element deliberately.

### Loading more and incremental search

"Load more" is a link or a button with a real URL, `hx-get` of that URL, `hx-target` on the list, and `hx-swap="beforeend"`. The response is the next rows. Update or remove the control in the same response with an OOB swap, or include the next control after the rows and target a wrapper. A plain GET of `href` — JavaScript off, a middle-click, or Open in New Tab — must return a full document for that next page, not a bare row fragment. Branch that on the headers in section 1. In 2.0.11, Ctrl-click and Cmd-click on an anchor that carries `hx-get` issue the fragment request instead of following `href`.

Incremental search is the form in section 3. Push the URL when the query should survive Back and reload. Leave the URL alone for a disposable filter the user would not share.

### Response headers that change the swap

These headers are instructions to the client. They belong in a test next to the body, because the body has to fit the instruction.

| Header | Effect in 2.0.11 | Use it |
| --- | --- | --- |
| `HX-Retarget` | CSS selector. Replaces the target before the swap. | One endpoint serves two shapes, such as a poll that usually replaces itself and sometimes replaces the main region. The body must match the new target. |
| `HX-Reswap` | Replaces the swap style. | The trigger's `hx-swap` is wrong for this outcome. Required when the trigger said `none` and this response must insert HTML. |
| `HX-Push-Url` | Pushes a history entry. `false` suppresses one. | The fragment update changed the URL the user is "on." The URL must render a full document on its own. |
| `HX-Replace-Url` | Rewrites the current entry. | The same, when Back should not return to the pre-update URL. |
| `HX-Redirect` | `location.href = …`. Full document load. Does not swap. | The next page has to be a real navigation. |
| `HX-Refresh: true` | `location.reload()`. | The current URL must be loaded again from scratch. Rare. |
| `HX-Location` | Another htmx GET. Not a document load. | You want a client-side visit. Easy to confuse with `HX-Redirect`. Prefer a `303` from a boosted form unless you have a reason. |
| `HX-Reselect` | Overrides `hx-select` for this response. | The useful part of a larger body changed. Prefer a tighter body. |

`HX-Retarget` plus a mismatched body is worse than two endpoints. Use it when the trigger is fixed (a poll element already in the page) and the outcome changes. Do not use `HX-Redirect` to refresh a region you could have swapped.

### When to leave the page

Navigate when any of these are true: the primary content and the shell both change, the user would expect Back to return to a previous page, the result has its own URL, or the interaction is a POST that should not be refreshed in place. Stay on the page when the change is local and the current URL is still honest.

## 5. JavaScript integration

htmx's JavaScript API is intentionally small. A codebase that grows its own component lifecycle on top of it has rebuilt the problem htmx was meant to avoid. A handful of idempotent functions, called from events, is enough.

### Delegate, and look nodes up at event time

Listeners bound to nodes inside a swap target die with those nodes. Listen on `document` (or another node that stays) and resolve the control from the event.

```javascript
document.addEventListener("click", (event) => {
  const button = event.target.closest("[data-copy]");
  if (!button) return;
  const source = document.getElementById(button.dataset.copy);
  if (source) navigator.clipboard.writeText(source.textContent.trim());
});
```

Query at event time. A node captured at first load is the node from the first page, which a body swap has already removed.

### Lifecycle events

The events you actually need:

| Event | When |
| --- | --- |
| `htmx:beforeSwap` | The response is about to replace a target. Read state you need to reapply, or set `shouldSwap`. |
| `htmx:afterSwap` | New nodes are in the tree. External widgets may not be ready yet. |
| `htmx:afterSettle` | The swap has settled (about 20ms after, by default). A good place to init. |
| `htmx:load` | htmx finished processing new content. `htmx.onLoad(fn)` is sugar for this. The argument is the new content, not the whole document. |
| `htmx:historyRestore` | A snapshot or a history-miss response was restored. Cache hits do not go through your request handler. |
| `htmx:beforeCleanupElement` | An element htmx owns is about to be disabled or removed. Destroy widgets here. |
| `htmx:beforeHistorySave` | A snapshot is about to be taken. Undo DOM mutations a library made, or the snapshot restores the mutated DOM and the library initializes it a second time. |
| `htmx:configRequest` | Last chance to change parameters or headers. Prefer a hidden input when the value belongs to one form. |
| `htmx:responseError` / `htmx:sendError` / `htmx:timeout` | Bad status, network failure, timeout. |
| `htmx:oobErrorNoTarget` / `htmx:targetError` | The response and the document disagree. |

Bind these once, on `document`, at startup. Binding them inside `afterSettle` stacks a new listener on every swap.

History restoration and swaps both need initialization. Call the same idempotent function from `htmx:afterSettle` or `htmx:load`, and from `htmx:historyRestore`. Guard with a data attribute when a second call would create a second widget:

```javascript
function mount(root) {
  root.querySelectorAll("[data-widget]:not([data-mounted])").forEach((el) => {
    el.dataset.mounted = "1";
    startWidget(el);
  });
}
```

Clear that guard if you destroy the widget and the same node will be mounted again. `outerHTML` replacement gives you a new node, so the guard resets with the HTML, which is what you want.

### Third-party code and scripts that arrive later

Libraries that upgrade a node (rich selects, charts, maps) need `mount` after new HTML arrives, and `destroy` before the node is saved into history or removed. Listen for `htmx:beforeCleanupElement` and `htmx:beforeHistorySave`. If the library only offers a document-wide scan, keep the scan idempotent.

HTML inserted by your own code, not by htmx, is invisible to htmx until you call `htmx.process(node)`.

Scripts in a response are handled only in the body, and only when `allowScriptTags` is true (the 2.0.11 default). The parser strips `<head>` first, so a script in the head of a returned document never runs. Do not put behavior in a page template's head and expect a boosted visit to load it. Permanent code belongs in the script the layout loaded on the real document request.

When one page needs a library the layout does not load, detect the marker element in `mount` and insert the `<script>` once:

```javascript
function ensureLibrary(root) {
  if (!root.querySelector("[data-needs-maps]")) return;
  if (document.querySelector("script[data-maps]")) return;
  const script = document.createElement("script");
  script.src = "/static/maps.js";
  script.dataset.maps = "1";
  script.onload = () => mount(root);
  document.head.appendChild(script);
}
```

That is a special case, not a loader framework. If many pages need it, put it in the layout.

### Cleanup

A listener on `document` does not need cleanup. A `setInterval`, a socket, an observer, or a widget instance does. Start it in `mount`, and stop it on `htmx:beforeCleanupElement` or when `mount` notices the old node is gone. htmx removes its own listeners from elements it discards. It does not know about yours.

`hx-preserve` on an element with an id keeps that element across a swap. Use it for something expensive to rebuild, such as a media element the user is already playing. Preserved nodes keep stale state by design. Do not preserve a node to paper over a swap that was too large.

## 6. History, caching, and security

These are three different stores. Mixing them up leads to either "Back shows a signed-in page after logout" or "we turned history off and the site feels broken."

| Store | What it is | What clears it |
| --- | --- | --- |
| htmx history cache | Snapshots of the history element's `innerHTML`, kept so Back can restore without a request. In 2.0.11 this is `sessionStorage` under `htmx-history-cache`. The current htmx.org docs still say `localStorage`. Trust the build you ship. | Tab close, `historyCacheSize`, quota eviction, or deleting the key. Logout does not clear it. |
| Browser session history | The URL stack. Includes URLs htmx pushed. | The user, or the browser discarding the tab's history. |
| HTTP cache | Cached responses, controlled by `Cache-Control`, `ETag`, `Vary`, and friends. | Those headers, and the browser. Unrelated to `sessionStorage`. |
| Back-forward cache | The browser's snapshot of a real document, used when a full navigation goes Back. | A real load. It is not the htmx snapshot. `Cache-Control: no-store` influences it. It does not influence `htmx-history-cache`. |

`sessionStorage` is per tab and dies with the tab. Any script on the origin can read it. It survives logout for as long as the tab stays open. That is the threat: Back restores HTML that still shows private data and a signed-in header, even though the next click will be unauthorized.

### Snapshots, hits, and misses

Before htmx pushes a URL, it snapshots the history element (the first `[hx-history-elt]`, otherwise `<body>`). The snapshot stores inner HTML, title, and scroll. The default cache holds 10 entries. A cache hit restores that HTML locally, fires `htmx:historyRestore`, and does not hit the server. A miss, with the default `refreshOnHistoryMiss: false`, GETs the URL and swaps the response into the history element.

On that miss request, 2.0.11 always sends `HX-History-Restore-Request: true`. It sends `HX-Request: true` only when `historyRestoreAsHxRequest` is true, which is the library default. Set the flag to false. The server must still return a full document whenever the restore header is present, because the swap replaces the body.

`refreshOnHistoryMiss: true` calls `location.reload(true)` instead of an htmx GET. That is a real document load. The boolean is a legacy argument. Current browsers ignore it, so this is not a reliable "bypass the HTTP cache" switch. Use it when a miss should re-run head scripts. Leave it false when a miss should stay inside htmx and your server returns a full document.

`historyCacheSize: 0` disables snapshots. Back still works. Every Back is a miss and fetches a full document. That is the right switch when the application is full of private HTML and snapshotting is not worth the care. It does not disable the address bar, and it does not stop htmx from recording the current path.

### `hx-history="false"`

An element with `hx-history="false"` anywhere in the current document skips the snapshot of the page being left. History is still on. The next Back to that URL is a miss and requests it. The attribute does not stop htmx from writing the current path to `htmx-current-path-for-history` in `sessionStorage`, and it does not remove the URL from the browser's own history.

Put it on pages that contain secrets, credentials, one-time tokens, or other people's private data. Do not put it on every page to avoid thinking about one URL. A secret in a path is already in the browser history and may be in the path key. Keep secrets out of URLs. When a one-time token has to be in a path, make it short-lived and single-use, and do not pretend `hx-history="false"` erased it.

`hx-history-elt` changes which element is snapshotted. It has to exist on every page that participates, or restore swaps into the wrong place. The body is the right choice unless you have a measured reason.

### Authorization

The server authorizes every request, including fragment, boosted, poll, and history-miss requests. `HX-Request` is not an authentication factor. A restored snapshot is not a session. It is old HTML. The next action has to pass the real check.

Design the failure path. A `401` or `403` that does not swap (the default) leaves the user looking at a page that no longer works. For a boosted navigation, returning a full sign-in document is coherent. For a fragment, prefer a small signed-out state, or `HX-Redirect` to the sign-in document, and test it. Silent nothing is how people conclude the button is broken.

Logout is the sharp case. Pages snapshotted while signed in are still in the tab. `hx-history="false"` on the sensitive ones means those were never stored. The rest can still paint a signed-in shell. Pick a policy and test Back after logout:

- Mark every view that shows private data with `hx-history="false"`, and accept that harmless pages may restore a stale header until the next request. The next request must not trust that header.
- Or set `historyCacheSize` to 0 so Back always fetches.
- Or clear `sessionStorage`'s `htmx-history-cache` as part of a successful logout, knowing that is a client courtesy and the server check still matters.

A stale header that still offers "Delete" is acceptable only because Delete will fail closed. A stale header that shows someone else's email is not.

### HTTP caching

`Cache-Control` does not decide what htmx writes to `sessionStorage`. Set it for the HTTP caches and for the browser's back-forward cache.

- Public, identical-for-everyone HTML may be cached. Send `Vary` for every request header that changes the body, including `HX-Request` when both a document and a fragment exist.
- Personalized HTML should not be stored in a shared cache. `no-store` is the simple policy. A handler that set a more specific policy can keep it.
- History-miss responses are full documents. If `historyRestoreAsHxRequest` is left at the default, a cache that keys only on the URL can store a fragment under the same URL as the document. Turn the flag off, and send `Vary`.
- `ETag` has to differ when the body differs.

`getCacheBusterParam` appends a query parameter so htmx GETs and normal GETs do not share a cache slot. Prefer `Vary`. A cache-buster parameter becomes part of the URL the user might see.

### XSS

Swaps write HTML with `innerHTML`. In 2.0.11, `allowScriptTags` and `allowEval` default to true. A `<script>` in swapped body HTML runs. `hx-on:` attributes and `js:` values in `hx-vals` and `hx-headers` are evaluated. That is safe only if the server escaped the HTML.

Escape by default in the template engine. The few places that emit raw HTML should be a whitelist you can name (an icon sprite, a known-safe SVG), never user text. If raw user HTML is unavoidable, put it inside `hx-disable` so injected `hx-*` attributes are not processed, and still sanitize it. Attribute blacklists miss things. A content security policy that blocks inline scripts is a backstop, and it requires `allowEval` and `allowScriptTags` to be false, plus nonces for the scripts you do ship (`inlineScriptNonce`, `inlineStyleNonce`). htmx injects an indicator `<style>` unless you account for it.

Turning `allowEval` off disables event filters, `hx-on:`, and `js:` prefixes. Turn it off when you have a CSP that needs it, and move those behaviors into the external script. Do not turn it off casually and leave filters in the markup that then silently stop running.

## 7. Accessibility and browser behavior

A swap that avoids a full reload can still be a bad page. The HTML has to be usable with a keyboard and a screen reader, and the browser behaviors people rely on have to keep working.

- Use real elements. An `<a href>` for navigation, a `<button>` for an action, a `<form>` for a submit. htmx on a `<div>` with a click handler throws away keyboard behavior you then have to rebuild.
- Label every control. Tie errors to fields with `aria-describedby` or by rendering the message where the browser's constraint validation would have.
- htmx restores focus to an element with the same id if the focused element was replaced and had an id. Give the replacement the same id when the user was in that control. Do not move focus on a poll. `defaultFocusScroll` is false, so a focus restore does not scroll the focused control into view unless the swap says `focus-scroll:true`.
- Announce updates that happen away from the user's focus. `aria-live="polite"` on a results region, `role="status"` for a completion, `role="alert"` for an error. Replacing the live region node can cut off the announcement. Updating its contents, or swapping an inner element while the live region stays, is more reliable. Test with a screen reader when the update is the whole point of the control.
- The htmx indicator class is a visual fade. Set `aria-busy` on the region while a request that blocks the task is in flight, and clear it when the request ends. A poll that is already showing "Working" can set `aria-busy="true"` on that element in the HTML.
- Boosted navigation scrolls the target to the top (`scrollIntoViewOnBoost` defaults to true, and the target is the body). Fragment updates should not scroll unless the user needs to see a new summary, in which case `hx-swap="innerHTML show:top"` or `scroll:top` is explicit. In htmx 2 the default `scrollBehavior` is `instant`, not the smooth scroll of htmx 1.
- Preserve in-widget state that a swap would reset: the open `<details>`, the scroll offset inside a panel, text the user has started typing. Read it in `htmx:beforeSwap` and write it back after, or stop swapping that subtree.
- Leave native link behavior in place. `href`, `target="_blank"`, and middle-click are features. Ctrl-click and Cmd-click are features on a normal or boosted link. On an anchor with its own `hx-get`, htmx 2.0.11 handles those two as the fragment request. Do not replace a link with a button that only works through htmx unless the action is not navigation.

`reportValidityOfForms: true` is the accessibility setting for client-side constraint validation. Without it, htmx blocks the submit and the user may get no explanation.

## 8. Performance and reliability

Measure before adding machinery. A full document rendered on a boosted click is the normal cost of a server-rendered page, and it is usually cheaper to understand than a client cache of HTML fragments.

- Poll only while the result can change, at a human interval, on the smallest fragment that is honest. Stop when the work is done. Pause when the document is hidden if the poll is frequent.
- Debounce typing. Synchronize overlapping requests. Disable the submit control for the duration. These three prevent most accidental stampedes.
- Keep fragments small so the swap is cheap and state is easy to preserve. A swap of the whole main column on a timer is a page load without the browser's loading UI.
- The server pays for every representation. A handler that renders a full layout for a request that only needed one `<li>` is the thing to fix, by returning the fragment, not by adding a client-side diffing library. Morph-style swaps exist as an extension. Adopt one when a measured swap is destroying state you cannot preserve any other way.
- Let `4xx` and `5xx` keep the default "do not swap," except the codes you opted into because their body is useful HTML. A `500` page swapped into a list is how a blip destroys the screen.
- Network errors fire `htmx:sendError`. Timeouts fire `htmx:timeout` (`timeout` config is `0`, meaning no client timeout, until you set one). htmx does not retry. Retry by letting the user submit again, or by the next poll tick. An automatic retry loop needs backoff and a reason. Most pages do not have one.
- Cache static assets aggressively. Cache HTML only when it is public and varied correctly. Do not add `getCacheBusterParam`, service workers, or a second fragment cache because a page felt slow once.

## 9. Testing and debugging

Split tests by what can lie. HTTP tests prove the server's decision. A browser proves that htmx applied it. A green HTTP test does not prove the target, the history stack, or the focus.

### HTTP tests

Send the headers a real request would send, and assert status, content type, the headers you meant to set, and the shape of the body.

| Case | Request headers | Expect |
| --- | --- | --- |
| Address bar, refresh, no JavaScript | none | Full document. Layout markers present. |
| Boosted click | `HX-Request`, `HX-Boosted`, and `HX-Target` if the body has an id | Full document. |
| Fragment | `HX-Request`, and `HX-Target` set to the target id | Fragment only. No layout. No `<html>`. |
| History miss, config false | `HX-History-Restore-Request` and no `HX-Request` | Full document. |
| History miss, library default | both `HX-Request` and `HX-History-Restore-Request` | Full document, not the fragment branch. |
| Poll still working | fragment headers | `204`, or a small fragment, and no `HX-Redirect`. |
| Poll finished and retargeting | fragment headers | `HX-Retarget`, `HX-Reswap` if the trigger swap would not insert, the URL header if you push one, and a body that matches the new target. |

Assert a boosted POST's success as a `303` with `Location`. Assert the destination, requested with no htmx headers and again with `HX-Boosted`, as a full document. Do not assert `HX-*` headers on the `303` response. htmx will not see them.

For a file upload, build a real `multipart/form-data` body and parse it on the server the way production does. Checking that the template contains `enctype` does not prove the handler reads the part.

Validation tests should cover the status you chose. If the status is `422`, the test belongs next to a client config test or a browser test, because the server body is irrelevant when response handling drops it.

```go
func TestItemFragmentOmitsLayout(t *testing.T) {
    req := httptest.NewRequest(http.MethodGet, "/items/7/row", nil)
    req.Header.Set("HX-Request", "true")
    req.Header.Set("HX-Target", "item-7")
    rec := httptest.NewRecorder()
    app.ServeHTTP(rec, req)

    if rec.Code != http.StatusOK {
        t.Fatalf("status %d", rec.Code)
    }
    body := rec.Body.String()
    if strings.Contains(body, "<html") || strings.Contains(body, `id="site-header"`) {
        t.Fatalf("fragment included the layout:\n%s", body)
    }
    if !strings.Contains(body, `id="item-7"`) {
        t.Fatalf("fragment missing its root:\n%s", body)
    }
}
```

### Browser tests

Use a real browser for anything that depends on htmx applying a response.

- Back and Forward after a boosted navigation restore the previous body and title without a window load, or fetch a full document on a miss.
- A history miss (cache disabled, or more than `historyCacheSize` steps back) swaps a full document and leaves the shell intact.
- A widget initialized by JavaScript works after a boosted arrival and after Back. This is the test that catches a listener bound at `DOMContentLoaded` only.
- Enter in a search field updates the results region and does not navigate away. The same form with JavaScript disabled navigates to `action`.
- A file input submits as multipart with JavaScript on and off.
- Logout, then Back. The page either refetches into a signed-out document or, if a snapshot remains, does not expose data the policy said was uncached. The next privileged action fails.
- Sign-in and sign-out update the navigation the user can see, not only the fragment that submitted.
- On a boosted link, Ctrl-click, Cmd-click, and middle-click open a new tab. On a link with its own `hx-get`, middle-click opens a new tab and Ctrl-click or Cmd-click runs the fragment request, in htmx 2.0.11. `target="_blank"` and an external link open a new tab either way.

Detect an accidental document load with a sentinel in the layout `<head>`, which a body swap does not re-execute. Putting the sentinel in the body is useless when `allowScriptTags` is true, because the swap would run it again.

```html
<script>
  window.__documentLoads = (window.__documentLoads || 0) + 1;
</script>
```

Drive the flow, then read `window.__documentLoads`. It stays `1` across boosted navigation and fragment swaps. It becomes `2` after a real load, `HX-Redirect`, `HX-Refresh`, or a link that was not actually boosted. That single number distinguishes "we swapped" from "we reloaded and the test still saw the right text."

### Devtools

In the network panel, an htmx request is an XHR or fetch to your URL. Confirm the request headers (`HX-Request`, `HX-Boosted`, `HX-Target`, `HX-History-Restore-Request`) and the response body. A full document in a request you thought was a fragment is the bug. Response headers `HX-Retarget`, `HX-Reswap`, `HX-Push-Url`, and `HX-Redirect` are visible on the final response, not on a redirect hop.

`htmx.logAll()` prints every htmx event. It is noisy and belongs in a debugging session, not in the app. The console's `monitorEvents(document.getElementById("the-control"))` shows DOM events you expected to trigger a request. Breakpoints in `issueAjaxRequest` and `handleAjaxResponse` of the unminified build are the next step.

On a history bug, look at `sessionStorage['htmx-history-cache']` and `sessionStorage['htmx-current-path-for-history']`. The cache is the snapshots. The path key is not a snapshot and is easy to mistake for one.

## 10. Common anti-patterns

Each of these has a narrow exception. The exception is the thing you should have to explain in the change, not the default.

**Returning a fragment for a boosted navigation or a history restore.** The body swap then replaces the whole page with a partial, or a partial's worth of layout is missing. Return a full document whenever `fullDocument` is true. Exception: none, if the target is the body.

**`HX-Redirect` for an ordinary in-app update.** It throws away the document, the scroll position, and any in-memory state, and it hides the fact that a swap could have done the job. Use it when you truly need a new document. Prefer `303` for "this POST succeeded, go to the new resource" under boost.

**`location.href` in application code for an in-app transition.** Same cost as `HX-Redirect`, plus it splits navigation across JS and HTML. A link, a boosted form, or a fragment swap belongs in the markup. Assigning `location` is for leaving the origin or finishing a flow the markup cannot see.

**Relying on an inherited target.** It works until someone adds a button in that subtree. Set the target on the widget. Inheritance on a tight parent is fine.

**Binding JavaScript at first load only.** The first page works. The page after a boost does not, and Back may not either. Delegate, or init from `htmx:load` / `htmx:afterSettle` and `htmx:historyRestore`.

**Depending on scripts in a swapped `<head>`.** They are stripped before the swap. Ship behavior in the layout script or in the body, knowingly.

**Dropping native form behavior.** A form with only `hx-post` and no `action` fails closed when JavaScript fails. Keep `method` and `action` unless the interaction cannot exist without a script, and say why.

**Reimplementing htmx in JavaScript.** Hand-rolled `fetch` plus `innerHTML` skips history, titles, OOB, indicators, and the header contract. Use attributes. A small `fetch` is reasonable when htmx's request model does not fit (a binary upload with progress that `htmx:xhr:progress` cannot present the way you need, a drag library that already owns the gesture). After it succeeds, trigger the existing htmx refresh instead of maintaining a second renderer.

**Two sources of truth on the client.** A JS store, plus the DOM, plus htmx history, will drift. The server renders the DOM. The DOM is the state. Client variables are for UI mechanics (menu open, widget instance), not for a copy of the data the server just rendered.

**Ignoring history and authentication together.** Shipping boost without deciding what may be snapshotted, and without trying Back after logout, leaves private HTML in the tab. Make the call in section 6 and add the browser test.

**`hx-swap="none"` together with `HX-Retarget` and no `HX-Reswap`.** The retarget runs, and `none` then discards the HTML. Send `HX-Reswap` to a swap that inserts.

**A page-specific head asset as the way a boosted page gets its behavior.** It works on refresh and fails on boost. Load shared assets with the layout.

## 11. Implementation checklist

Use this before writing an htmx feature and again before calling it done.

1. Read the existing htmx in the project. Match its response selection, its boost boundary, its history config, and the way it initializes scripts. Do not introduce a second convention beside a working one.
2. Name the interaction. It is either a navigation (full document, URL changes, shell updates) or a fragment update (one target, URL unchanged unless you have a reason). Polls, typeahead, and row actions are fragments.
3. Write down the request headers and the response shape. Include the no-JS request, which has no htmx headers.
4. Set `hx-target` and `hx-swap` on the element or its immediate widget parent. Confirm you are not inheriting a target from the layout by accident.
5. Keep `href`, or `method` and `action`. Add htmx attributes that repeat them. Keep `enctype` on file forms.
6. Decide the success status. `303` to a document URL, `200` fragment, or `204` for no change. If the status is `4xx`, confirm response handling will swap it, or change the status.
7. If the response sends `HX-Retarget`, `HX-Reswap`, `HX-Push-Url`, or `HX-Redirect`, confirm the body matches and that a `303` is not the thing carrying those headers.
8. Decide history. Should this URL be snapshotted? Should it be pushed? Can a cold load of the pushed URL render a document? Does logout-then-Back expose data it should not?
9. If JavaScript is involved, bind it so a swap and a history restore still work, and destroy anything that holds resources. Do not add a helper framework for one widget.
10. Add HTTP tests for the document and fragment shapes, including boosted and history-restore headers when those paths share a handler.
11. Exercise the interaction in a browser: the click or keystroke, Back, the no-JS path when you claim one, and the sentinel that catches an accidental reload.
12. Do not add a dependency, an extension, or a client store unless the attributes cannot express the behavior. SSE, WebSockets, and morph swaps are extensions in htmx 2, not core, and they are not the first tool to reach for.

## 12. Version notes

Checked against the unminified behavior of htmx 2.0.11 (`htmx.version` in the distributed file) and the 2.x documentation and migration guide on htmx.org. The live reference sometimes documents attributes this build does not have. `hx-partial` is one of those: it appears in the current reference and is absent from 2.0.11. Do not use it against this generation.

- History snapshots are stored in `sessionStorage` (`htmx-history-cache`, and the path in `htmx-current-path-for-history`). Several htmx.org pages still say `localStorage`. The security property differs: this build's cache is per tab and ends when the tab closes. It still survives logout.
- `historyRestoreAsHxRequest` defaults to true. The docs say to set it false whenever `HX-Request` selects a fragment. This guide agrees with the docs.
- `refreshOnHistoryMiss: true` calls `location.reload(true)`. The argument is legacy and is not a portable cache bypass. The call is a real document load.
- On a boosted anchor, Ctrl-click and Cmd-click opt out. Shift-click does not, in 2.0.11. Middle-click is not a `click` event here, so the browser handles it. The Ctrl/Cmd opt-out is not applied to an anchor that has its own `hx-get` or `hx-post`. That click is prevented and the fragment request runs.
- Boost's "same page" check compares hostnames, not origins. `selfRequestsOnly` (default true) then rejects a different origin. A same-hostname link on another port or scheme can be captured and then cancelled.
- `204` does not swap. `400`–`599` do not swap unless `responseHandling` or `htmx:beforeSwap` says so. The first matching rule wins.
- Response headers on a `3xx` are not visible to htmx. The followed response is what counts. Boosted navigation pushes the final path and query.
- `HX-Redirect` assigns `location.href`. `HX-Refresh: true` calls `location.reload()`. `HX-Location` issues an htmx request.
- A target of `<body>` always uses `innerHTML`. `<head>` is stripped from responses. Body scripts run only while `allowScriptTags` is true, which is the default.
- A form's `enctype="multipart/form-data"` is honored by 2.0.11, as is `hx-encoding`. A file field alone changes nothing. Older writeups mention only `hx-encoding`.
- htmx 2 puts `DELETE` parameters in the query string. htmx 1 put them in the body. `scrollBehavior` defaults to `instant`. Extensions, including SSE and WebSockets, are not in the core file.
- `reportValidityOfForms` defaults to false. Forms are still checked. The browser UI is not shown until the flag is on.
- The docs say a blocking `<script>` in the head is the supported way to load htmx, and that `defer` is best-effort. If you defer it, confirm that nodes present before the script runs still get processed.
- `hx-on` without a colon is the htmx 1 spelling. htmx 2 uses `hx-on:event-name`. Attributes are case-insensitive, so listen for the kebab-case event name in markup.

## Project-specific notes

This section is not part of the portable guide. Delete it when copying the file.

Decisions for the application in this repository, including which pages are boosted, which responses are fragments, and which views set `hx-history="false"`, live in [HTMX-APP.md](HTMX-APP.md).
