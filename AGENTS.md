# DJMagic — Agent instructions

Product and implementation notes are in `PLAN.md`.

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

Existing tests stay as they are. Do not modify a test because it fails, weaken an assertion, skip a test, delete a test, or rewrite what it expects. Investigate the failure, report it, and separate a failure that already existed from a regression when that is visible. Add tests for new behavior. If a requirement change means an existing test has to change, explain the change and wait for permission before editing the test.

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
