# Git worktrees for parallel agents

A Git worktree is a second checkout of this repository. It has its own files and its own branch, and it shares history with the primary checkout. Cursor can create one for each agent. Two agents can then edit, build, and run the app at the same time. The primary checkout, including `make dev` on port 8200 and `data/`, stays as it is.

Separate checkouts keep simultaneous edits from overwriting each other. They do not merge the results. Integrating two branches can still produce conflicts where both sides changed the same lines.

## How Cursor creates a worktree

In the Agents Window, start an agent in a worktree or move an existing agent into one. Cursor creates a checkout and the agent continues there. In the IDE, `/worktree` runs the rest of that chat in a new checkout. `/best-of-n` gives each model its own worktree and does not merge a winner back. `/apply-worktree` brings a worktree's changes into the main checkout. `/delete-worktree` removes one checkout when you are done with it.

`git worktree list` shows every checkout, including ones Cursor did not create.

Cursor looks for `.cursor/worktrees.json` in the new worktree, then in the primary checkout. This repository sets `setup-worktree-unix` to `.cursor/setup-worktree-unix.sh`. On macOS and Linux that script is the setup. There is no Windows script. If setup misbehaves, open the Output panel and choose **Worktrees Setup**.

Cursor 3.5 can delete old worktrees to limit disk use. The check interval is `cursor.worktreeCleanupIntervalHours` (the documented example is 6 hours). `cursor.worktreeMaxCount` defaults to 25 worktrees per machine, across every workspace. Cleanup can include worktrees created with `/worktree` or `git worktree add`. A worktree is not kept forever. Stop its dev server before it disappears, and do not rely on the directory remaining after the task.

## What setup does

The script resolves this checkout, checks that `git`, `go`, `lsof`, `curl`, and `pgrep` exist, and checks that it is inside a Git worktree. It runs `go mod download`, creates `tmp/` when that directory is missing, and exits. It does not start a server, create a database, or run migrations. Migrations run later, when a server opens its own SQLite file.

Running it again is safe. It downloads modules that are already cached, leaves an existing `.env` byte for byte, and does not delete local data.

It prints a worktree id, `wt-` plus the first 12 hex characters of the SHA-256 of the absolute checkout path. The same path always produces the same id. The id is not the dev-server name. You still choose `NAME` when you start a server.

`.env` is not copied from the primary checkout. That file is gitignored and may contain SMTP or cloud credentials, and this repository has no separate development-only secrets file. A missing `.env` is the normal local configuration. The app uses its defaults. `make dev` and `make dev-agent` set the listen address themselves, and an agent server also sets its database and storage paths, which win over anything in `.env`.

To send mail through MailHog from the personal server, copy `.env.example` to `.env` in that worktree and set `SMTP_HOST=localhost`, `SMTP_PORT=1025`, and `DEV_ALLOW_SMTP=true`. Do not copy production credentials. Do not commit `.env`. Named agent servers do not use that file's mail settings.

The Go module cache is shared by every checkout. That cache is safe to share. Mutable directories are not: each worktree has its own `tmp/`, `data/`, and `tmp/dev-agents/`.

## Run a server

From the worktree:

```sh
make dev-list
make dev-agent NAME=<unique-name>
```

`NAME` is letters, digits, hyphens, and underscores, up to 32 characters. The command stays in the foreground. When the server accepts requests it prints `dev-agent: ready` with the URL. The port comes from 8300 through 8399. Open that URL in a browser. The personal server remains http://localhost:8200.

The process listens on that port and stores its SQLite database, blobs, and catalog cache under `tmp/dev-agents/<name>/` in this worktree. The hourly cleanup inside the process deletes rows and files in that database and blob directory only. Nothing in development consumes a shared queue. `make dev-agent` sets `MAIL_BACKEND=console` and `DEV_ALLOW_SMTP=false`, and clears `SMTP_HOST`, `SMTP_USER`, and `SMTP_PASS` in the process environment. A `.env` file or inherited shell variables cannot switch that server to a real relay. Console output for that process stays in `tmp/dev-agents/<name>/air.log`. MailHog is only for the personal server, and only with `DEV_ALLOW_SMTP=true`.

`make dev-list` prints each server started from this worktree as:

```text
<name> <port> <url> running
```

A server that is still coming up is `starting`. A record whose process is gone is `stale`. A claim owned by another worktree is `external`, followed by that worktree's path. `external-stale` is a claim that no longer has a live owner.

Stop only the server you started:

```sh
make dev-stop NAME=<unique-name>
```

`make dev-stop-all` stops named servers recorded in this worktree. It does not stop the personal server and it does not stop a server in another worktree.

## Tests

From the worktree:

```sh
make test
```

`go test` does not use the dev-server port registry unless a test sets `DEV_SERVER_PORT_REGISTRY`. The dev-server tests point that variable at a temporary directory.

## Review and integrate

In the worktree, `git status` and `git diff` show what the agent changed. Commit there when you want a commit. Push and open a pull request when you want the work on the remote. `/apply-worktree` copies the changes into the main checkout instead.

Merging two agent branches uses the same Git merge as any other branches. When both sides edited the same lines, Git stops with a conflict. Keep both sides' intent, edit the file, and continue the merge. Do not clear the conflict by discarding one side unless that is the decision you mean to make.

To remove a checkout after its server is stopped:

```sh
git worktree remove <path>
```

`/delete-worktree` does this for a Cursor worktree. Remove only a worktree you are finished with. The primary checkout is not one of those.

## Ports

Every worktree of this repository claims ports in one directory:

```text
$(git rev-parse --path-format=absolute --git-common-dir)/djm-dev-ports/<port>/
```

Claiming a port is creating that directory. The `owner` file records the worktree path, worktree id, server name, instance id, supervisor pid, and start time. The dev server then checks that nothing else is already listening. Ports 8080, 8100–8199, and 8200 are never claimed. The personal server does not register itself; agent servers avoid its port and also notice if something is already listening.

`DEV_SERVER_PORT_REGISTRY` overrides the directory. Tests use that so they do not write into `.git`. Production and Docker do not read it.

A normal exit, `make dev-stop`, or a failed start deletes that server's claim. Two agents cannot delete each other's claims: removal checks the instance id, and a supervisor that is still `dev-server.sh start <name>` is left alone.

A claim with no live owner is left in place until it is older than `DEV_SERVER_PORT_CLAIM_GRACE` seconds (default 30). That covers a server that has created the directory and is still writing the owner file. Age alone never removes a claim whose supervisor is still this script. A pid that now belongs to some other program is not treated as the owner.

`SIGKILL` skips the exit cleanup, so the claim can sit until the grace period ends. Starting the same `NAME` in the same worktree drops a dead claim immediately, because that command still has the instance record. Another name, or another worktree, waits out the grace period. If a claim cannot be identified safely, the new server reports it and chooses a different port, or exits with `no free port` when the range is exhausted. It does not delete the claim.

`make dev-list` is the first place to look. `lsof -nP -iTCP:<port> -sTCP:LISTEN` shows a listener the registry does not know about. The owner file is under the registry path above.

## Troubleshooting

**Setup fails.** Read **Worktrees Setup** in the Output panel. Run `.cursor/setup-worktree-unix.sh` from the worktree and read the error. Install the missing tool it names. `go mod download` needs a network path to the module proxy when the module is not cached.

**No free port.** Another worktree is using the range, or a claim is still inside its grace period. `make dev-list` shows `external` rows. Stop the server that owns the port from the worktree that started it.

**The server was interrupted.** Start the same name again. If the supervisor is gone, that start clears the claim and the instance record. If the supervisor is still alive, the command prints the existing URL. Stop it with `make dev-stop NAME=...` from that worktree before starting a replacement.

**No `.env`.** Leave it missing, or create one from `.env.example`. The app already runs. Agent servers do not use `data/djm.db`.

## Two agents at once

Agent A gets a worktree and adds notes on the queue page. Agent B gets another worktree and adds a clock to the TV display.

```text
/worktree Add a short note field to the host queue page
```

```text
/worktree Show a clock on the TV display
```

Each agent works in its own checkout. When a server is useful, A runs `make dev-agent NAME=queue-notes` and B runs `make dev-agent NAME=display-clock`. The ready lines name two ports, for example http://localhost:8300 and http://localhost:8301. Each SQLite file lives under that worktree's `tmp/dev-agents/<name>/data/djm.db`. `make dev` on port 8200 is still the personal server.

A stops with `make dev-stop NAME=queue-notes`. B's server keeps running. Review each checkout with `git diff`, then merge or open pull requests when the work is ready. If both agents edited the same lines of a shared template, the merge stops for a conflict. Resolve it in the file. Neither worktree is deleted unless you ask for that.
