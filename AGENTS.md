# DJMagic — Agent instructions

Product and implementation notes are in `PLAN.md`.

## CSRF

State-changing browser requests use the shared CSRF protection in `docs/development/CSRF.md`. HTML that includes the token is sent with `Cache-Control: private, no-store`. The anonymous `csrf` cookie is not a guest identity.

- Use `{{csrfField}}` in every HTML form that changes state. Do not hand-write the hidden input.
- HTMX picks up the token from the shared `htmx:configRequest` handler. Do not add per-control CSRF attributes.
- Same-origin `fetch` calls use `djmCSRF.headers`. Do not send the token to another origin.
- Do not disable or skip the check to make an endpoint work, and do not add a second token scheme.
- A route exemption has to be narrow, explicit, and documented in `docs/development/CSRF.md`. There are none today.

## Mail

Development uses the console mail backend unless `DEV_ALLOW_SMTP` is the exact string `true` and SMTP settings are present. `SMTP_HOST` alone does not send. Startup fails if development is pointed at SMTP without that flag. `MAIL_BACKEND=console` stays on the console even when SMTP settings and the flag are present.

`make dev-agent` sets `MAIL_BACKEND=console`, `DEV_ALLOW_SMTP=false`, and empty `SMTP_HOST`, `SMTP_USER`, and `SMTP_PASS` in the server process. A `.env` file or inherited shell variables cannot override that. Do not point an agent server at a real relay. Console lines from that process stay in its own `air.log`.

Console mail logs the recipient and subject only. The check-your-email page shows a sign-in link only when `ENV=dev`, `SMTP_HOST` is empty, and `DEV_SHOW_MAGIC_LINK=true`. Production keeps its SMTP configuration and does not log message bodies.

## Development servers

`make dev` is the personal server on port 8200. Do not start it, stop it, restart it, or kill whatever is listening there.

Run `make dev-list` before starting a server. If your name is already listed as running, use that URL. Do not start another server.

If you have no running server, start one from the repo root:

```sh
make dev-agent NAME=<unique-name>
```

Pick one name and keep using it. If that command says the name is already running, use the URL it prints.

The command stays in the foreground. It prints `dev-agent: ready` with the URL when the server is accepting requests. Use that URL for browser checks.

Stop only the server you started, and stop it when testing is done:

```sh
make dev-stop NAME=<unique-name>
```

Do not kill processes by port. Do not run `pkill`, `killall`, or any other broad match against `air`, `go`, or `main`. `make dev-stop-all` stops every named agent server and does not stop the personal server; use it only for names you started.

In one checkout, named servers share that checkout's source files. Each one has its own port (from 8300 through 8399), Air build directory, SQLite file, blob directory, and catalog cache. Do not point an agent at `data/djm.db`, `data/blobs`, or `data/catalogs`, and do not share those writable directories between instances. Ports are claimed in a registry shared by every worktree of this repository, so two checkouts do not take the same port.

## Parallel Agent Development and Git Worktrees

The workflow is documented in `docs/development/WORKTREES.md`. Reading this file does not create a worktree. Use Cursor's worktree workflow (`/worktree` in the IDE, or a worktree agent in the Agents Window). `.cursor/worktrees.json` runs the setup script when Cursor creates the worktree.

Work in the assigned worktree. Keep source changes there. Leave every other worktree, including the primary checkout, untouched.

When several agents work at the same time, each one uses its own Git worktree. `make dev-agent` still supplies the server: its own port, SQLite file, blobs, catalog cache, and Air build directory under that worktree's `tmp/dev-agents/<name>/`.

Without explicit authorization, do not do any of the following:

- Destructive Git operations, hard resets, or discarding uncommitted changes
- Deleting branches or worktrees
- Force pushes
- Rebasing another agent's branch
- Cherry-picking or merging into the main branch
- Pushing to a remote

Report conflicts and other destructive choices. Leave them unresolved rather than discarding either side.

Existing tests stay as they are. Do not modify a test because it fails, weaken an assertion, skip a test, delete a test, or rewrite what it expects. Investigate the failure, report it, and separate a failure that already existed from a regression when that is visible. Add tests for new behavior. If a requirement change means an existing test has to change, explain which test, what it asserts today, and the smallest edit, then wait for permission before editing the test.

Permission to change an existing test has to name that test, or those tests, and the edit. Approving an implementation plan, a design, or the feature itself is not that permission, even when the plan says the test will change. Do not treat plan approval, "go ahead," or starting implementation as a yes. Ask, and wait for an explicit yes, before editing the test.

Use `make dev-agent` for a server. Leave port 8200 and `data/` for the personal server. Stop only the name you started, and only from this worktree. Leave other agents' processes, port claims, and runtime files alone, including servers in other worktrees. `make dev-stop-all` stops named servers in this worktree and does not stop the personal server or another worktree.

When the task is finished, report:

- Worktree path
- Current branch
- Files changed
- Tests run and their results
- Whether a development server was started, and its port
- Processes stopped
- Known issues
- Whether the changes are ready to integrate

Leave the worktree and any uncommitted changes in place.
