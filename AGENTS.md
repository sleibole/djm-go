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

These servers share the working tree. Each one has its own port (from 8300 through 8399), Air build directory, SQLite file, blob directory, and catalog cache. Do not point an agent at `data/djm.db`, `data/blobs`, or `data/catalogs`, and do not share those writable directories between instances.
