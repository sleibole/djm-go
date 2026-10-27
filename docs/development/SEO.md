# Search indexing

How public pages get a title, description, and canonical URL, and which responses a crawler may index. The rules live in `seo.go`. Tags are rendered by `templates/partials/open_graph.html`. After a boosted navigation, `applyPageMeta` in `static/app.js` copies them into the document head.

`DEPLOY.md` says when to set the flag on the server. This note is what to change in the app.

## When indexing is on

Indexing is on only when both are true: `ENV=prod` and `ALLOW_INDEXING=true`. Development ignores the flag. Staging runs as `ENV=prod` with the flag unset, so `next.djmagic.io` stays out of search results.

Canonical URLs are `BASE_URL` plus the path. Set `ALLOW_INDEXING` only after `BASE_URL` is the origin that should appear in search. Turning the flag on while `BASE_URL` is still `https://next.djmagic.io` would publish that host.

While the flag is off:

- HTML responses send `noindex, nofollow` in a robots meta tag and in `X-Robots-Tag`.
- `/robots.txt` is `User-agent: *` and `Disallow: /`.
- `/sitemap.xml` is a 404.

## What may be indexed

Only a successful full document (`200` and the `layout` template) for:

- `GET /` when nobody is signed in
- `GET /docs/csv-format`

Those two paths are the list in `indexableDocument` and the list in `handleSitemap`. They have to stay the same list.

Everything else is `noindex, nofollow`, including the signed-in home, accounts, shows, DJ profiles, queues, the venue display, feedback, sign-in, admin, errors, redirects, and `/healthz`. A fragment (an HTMX request that is not a full document) is `noindex` even when its URL is one of the two pages above.

`/robots.txt`, when indexing is on, disallows only `/auth/`. That is not access control. The magic-link page is also `noindex`, and `Disallow` only asks crawlers not to fetch it. The sitemap lists the two public URLs and nothing else. Do not add private paths to `robots.txt` to "hide" them. A disallow entry advertises the path.

`/static/`, `/favicon.ico`, `/robots.txt`, and `/sitemap.xml` do not get an `X-Robots-Tag`. Static files are assets, not documents.

## Adding a page

Put the title and description in that page's template, next to the visible copy:

```
{{define "title"}}CSV Format Guide · DJMagic.io{{end}}
{{define "description"}}How to format a CSV song catalog for DJMagic.io. …{{end}}
```

The description in `seo_tags` is only the fallback for a template that forgot its own. Give every page its own description, including pages that stay `noindex`, so shares and the document title are right.

A description is public text. It must not include an email address, a magic-link token, a singer's name, or anything else that changes per visitor. The visible page can show those. The description cannot.

The canonical URL is the path only. Query strings are dropped, so a search or a `utm_` parameter does not create a second URL. A trailing slash redirects to the path without the slash (`/docs/csv-format/` becomes `/docs/csv-format`). `/static/` is a route prefix and stays a 404. Do not redirect it.

A new page is `noindex` until its path is added in both `indexableDocument` and `handleSitemap`. Add it only when the page is public, stable, and the same for every anonymous visitor. Show pages, profiles, and search results stay out: they multiply, they change, and they can name people. Extend `seo_test.go` when the list changes. `TestPublicPagesAreIndexableWhenEnabled` and `TestPrivateAndSharedPagesStayUnindexed` are the checks.

## Boosted navigation

Ordinary clicks swap the body and leave the head in place. `templates/layout.html` puts the current page's tags in `<template id="page-meta">`. `applyPageMeta` copies the title, description, robots, canonical link, Open Graph tags, and Twitter tags into `document.head` on `htmx:afterSettle` and `htmx:historyRestore`.

A new head tag that has to follow the page belongs in that template and in the selector in `applyPageMeta`. A tag left only in `<head>` stays stuck on the first page of the tab.

## Caching

HTML varies on `HX-Request`, `HX-Boosted`, `HX-History-Restore-Request`, `HX-Target`, and `Cookie`, so a fragment is not stored as the document and a signed-in response is not stored as the anonymous page.

HTML responses are `Cache-Control: private, no-store` because they carry a CSRF token. Hashed static files stay publicly cacheable. Magic-link URLs are included in the HTML policy. Indexing is unchanged: a crawler can still fetch a public page, and `X-Robots-Tag` decides whether it may index it.

## Turning indexing on

Do this after `djmagic.io` is the live origin, not as part of pointing DNS. Leave the flag unset on `next.djmagic.io`.

```sh
ssh dokku@box config:set djm ALLOW_INDEXING=true
```

`BASE_URL` must already be `https://djmagic.io`. Confirm:

- `https://djmagic.io/` has no `X-Robots-Tag` and no robots meta tag.
- `https://djmagic.io/robots.txt` disallows only `/auth/` and points its Sitemap line at `https://djmagic.io/sitemap.xml`.
- `https://djmagic.io/sitemap.xml` lists `/` and `/docs/csv-format` on that origin.
- A signed-in home page and a show page send `noindex, nofollow`.
