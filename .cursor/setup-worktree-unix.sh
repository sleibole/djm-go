#!/bin/bash
# Prepare a Cursor Git worktree for this repository.
# Downloads Go modules and creates tmp/. Does not start a server, copy
# secrets, create a database, or modify another checkout. Safe to run again.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "setup-worktree: $1 is required" >&2
    exit 1
  fi
}

require_cmd git
require_cmd go
require_cmd lsof
require_cmd curl
require_cmd pgrep

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "setup-worktree: $ROOT is not a git checkout" >&2
  exit 1
fi

# Same identity the dev server stores on a port claim: wt- plus the first
# 12 hex characters of the SHA-256 of this worktree's absolute path.
worktree_id() {
  local sum
  if command -v shasum >/dev/null 2>&1; then
    sum=$(printf '%s' "$ROOT" | shasum -a 256 | awk '{print $1}')
  elif command -v sha256sum >/dev/null 2>&1; then
    sum=$(printf '%s' "$ROOT" | sha256sum | awk '{print $1}')
  else
    echo "setup-worktree: shasum or sha256sum is required" >&2
    exit 1
  fi
  printf 'wt-%s\n' "${sum:0:12}"
}

primary=""
if [ -n "${ROOT_WORKTREE_PATH:-}" ]; then
  if [ ! -d "$ROOT_WORKTREE_PATH" ]; then
    echo "setup-worktree: ROOT_WORKTREE_PATH is not a directory" >&2
    exit 1
  fi
  primary=$(cd "$ROOT_WORKTREE_PATH" && pwd)
else
  # sed reads the whole list, so a short pipeline does not die with SIGPIPE.
  primary=$(git worktree list --porcelain | sed -n 's/^worktree //p' | sed -n '1p')
fi

# .env is gitignored and may hold SMTP or cloud credentials. This repo has
# no separate development-only secrets file. A missing .env is the normal
# local configuration: the app uses its defaults, and make dev-agent sets
# the listen address and storage paths itself.
if [ -f "$ROOT/.env" ]; then
  env_line=".env already present; left it unchanged"
else
  env_line="no .env in this worktree. The app uses its defaults. A missing .env is normal. Secrets are not copied from another checkout."
fi

echo "setup-worktree: downloading Go modules"
go mod download

mkdir -p "$ROOT/tmp"

id=$(worktree_id)
branch=$(git rev-parse --abbrev-ref HEAD)
goversion=$(go env GOVERSION)

echo "setup-worktree: ready"
echo "  worktree: $ROOT"
echo "  worktree_id: $id"
echo "  branch: $branch"
echo "  primary: ${primary:-unknown}"
echo "  go: $goversion"
echo "  env: $env_line"
echo "  server: not started"
echo "  start with: make dev-agent NAME=<unique-name>"
echo "  ports: 8300-8399; port 8200 is the personal server"
