#!/bin/bash
# Named Air dev servers for concurrent agents.
# make dev (port 8200, tmp/main, data/djm.db, data/blobs, data/catalogs)
# is not managed here.
#
# Each instance gets its own port, Air build directory, SQLite file, blob
# directory, and catalog cache. Shutdown only signals processes whose
# command line still matches this instance. A reused pid or an unrelated
# listener is left alone. Ports 8080, 8100-8199, and 8200 are never used.
#
# The port registry is shared by every worktree of this repository. It lives
# under the Git common directory unless DEV_SERVER_PORT_REGISTRY is set.
# A port is claimed with mkdir, which is atomic. A claim whose owner is still
# starting, or whose supervisor is still this script, is left alone. A claim
# with no live owner is reusable only after DEV_SERVER_PORT_CLAIM_GRACE
# seconds (default 30). A normal exit removes the claim immediately. SIGKILL
# can leave it until the grace period passes.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

# Tests override the state directory, port range, ready timeout, and the
# command that is launched. DEV_SERVER_RELOAD_DIR, when set to an absolute
# path, adds an Air include_dir so reload can be checked without editing
# project files. Agents leave that unset.
STATE_DIR=${DEV_SERVER_STATE_DIR:-$ROOT/tmp/dev-agents}
PORT_MIN=${DEV_SERVER_PORT_MIN:-8300}
PORT_MAX=${DEV_SERVER_PORT_MAX:-8399}
READY_TIMEOUT=${DEV_SERVER_READY_TIMEOUT:-60}
CLAIM_GRACE=${DEV_SERVER_PORT_CLAIM_GRACE:-30}
AIR_PKG=github.com/air-verse/air@v1.67.4
LOCK_DIR=""
LOCK_HELD=0
REGISTRY=""
WORKTREE_ID=""
INSTANCE_ID=""
CLAIM_PORT=""

NAME=""
APP_PORT=""
URL=""
INSTANCE_DIR=""
INSTANCE_FILE=""
INSTANCE_CONFIG=""
INSTANCE_BINARY=""
LOG_FILE=""
CHILD_PID=""
SERVER_PID=""
TAIL_PID=""
CLEANED=0
OWN_INSTANCE=0

usage() {
  echo "usage: dev-server.sh start NAME | stop NAME | list | stop-all" >&2
  exit 1
}

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "dev-server: $1 is required" >&2
    exit 1
  fi
}

require_number() {
  local label=$1 value=$2
  case "$value" in
    ''|*[!0-9]*)
      echo "dev-server: $label must be a number, got ${value:-empty}" >&2
      exit 1
      ;;
  esac
}

validate_name() {
  local name=$1
  if [ -z "$name" ]; then
    echo "dev-server: NAME is required" >&2
    echo "usage: make dev-agent NAME=<unique-name>" >&2
    exit 1
  fi
  case "$name" in
    *[!A-Za-z0-9_-]*)
      echo "dev-server: NAME must contain only letters, digits, hyphens, and underscores" >&2
      exit 1
      ;;
  esac
  case "$name" in
    [A-Za-z0-9]*) ;;
    *)
      echo "dev-server: NAME must start with a letter or digit" >&2
      exit 1
      ;;
  esac
  if [ "${#name}" -gt 32 ]; then
    echo "dev-server: NAME must be 32 characters or fewer" >&2
    exit 1
  fi
}

prepare_state() {
  require_number PORT_MIN "$PORT_MIN"
  require_number PORT_MAX "$PORT_MAX"
  require_number READY_TIMEOUT "$READY_TIMEOUT"
  require_number CLAIM_GRACE "$CLAIM_GRACE"
  if [ "$PORT_MIN" -gt "$PORT_MAX" ]; then
    echo "dev-server: PORT_MIN ($PORT_MIN) is greater than PORT_MAX ($PORT_MAX)" >&2
    exit 1
  fi
  if [ "$READY_TIMEOUT" -lt 1 ]; then
    echo "dev-server: READY_TIMEOUT must be at least 1" >&2
    exit 1
  fi
  # Agents must not keep their state inside the personal data directory.
  # Check before mkdir so a refusal does not create that directory.
  case "$STATE_DIR" in
    "$ROOT/data"|"$ROOT/data"/*|data|data/*)
      echo "dev-server: state directory must not be inside the personal data directory ($STATE_DIR)" >&2
      exit 1
      ;;
  esac
  mkdir -p "$STATE_DIR"
  STATE_DIR=$(cd "$STATE_DIR" && pwd)
  case "$STATE_DIR" in
    "$ROOT/data"|"$ROOT/data"/*)
      echo "dev-server: state directory must not be inside the personal data directory ($STATE_DIR)" >&2
      exit 1
      ;;
  esac
  LOCK_DIR=$STATE_DIR/.lock
  compute_worktree_id
  resolve_registry
}

field() {
  local file=$1 key=$2 line
  [ -f "$file" ] || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      "$key"=*)
        printf '%s\n' "${line#"$key"=}"
        return 0
        ;;
    esac
  done < "$file"
  return 1
}

write_instance() {
  local tmp
  tmp=$(mktemp "${INSTANCE_FILE}.tmp.XXXXXX")
  cat > "$tmp"
  mv "$tmp" "$INSTANCE_FILE"
}

publish_instance() {
  local status=$1
  write_instance <<EOF
name=$NAME
port=$APP_PORT
url=$URL
status=$status
supervisor_pid=$$
go_pid=${CHILD_PID:-}
server_pid=${SERVER_PID:-}
config=$INSTANCE_CONFIG
binary=$INSTANCE_BINARY
log=$LOG_FILE
instance_id=$INSTANCE_ID
worktree=$ROOT
worktree_id=$WORKTREE_ID
EOF
}

command_of() {
  ps -ww -p "$1" -o command= 2>/dev/null || true
}

command_contains() {
  local pid=$1 needle=$2 cmd
  [ -n "$pid" ] && [ -n "$needle" ] || return 1
  cmd=$(command_of "$pid")
  case "$cmd" in
    *"$needle"*) return 0 ;;
  esac
  return 1
}

command_starts_with() {
  local pid=$1 prefix=$2 cmd
  [ -n "$pid" ] && [ -n "$prefix" ] || return 1
  cmd=$(command_of "$pid")
  case "$cmd" in
    "$prefix"|"$prefix "*) return 0 ;;
  esac
  return 1
}

owns_supervisor() {
  local pid=$1 name=$2 cmd
  cmd=$(command_of "$pid")
  case "$cmd" in
    *dev-server.sh\ start\ "$name"|\
    *dev-server.sh\ start\ "$name"\ *)
      return 0
      ;;
  esac
  return 1
}

descendant_pids() {
  local pid=$1 child
  for child in $(pgrep -P "$pid" 2>/dev/null || true); do
    printf '%s\n' "$child"
    descendant_pids "$child"
  done
}

is_descendant() {
  local pid=$1 ancestor=$2 hops=0 ppid
  while [ -n "$pid" ] && [ "$hops" -lt 30 ]; do
    [ "$pid" = "$ancestor" ] && return 0
    [ "$pid" = "1" ] || [ "$pid" = "0" ] && return 1
    ppid=$(ps -p "$pid" -o ppid= 2>/dev/null | tr -d '[:space:]' || true)
    [ -n "$ppid" ] && [ "$ppid" != "$pid" ] || return 1
    pid=$ppid
    hops=$((hops + 1))
  done
  return 1
}

listening_pids() {
  lsof -nP -iTCP:"$1" -sTCP:LISTEN -t 2>/dev/null || true
}

port_listening() {
  lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}

# A live supervisor that still looks like this script is holding the port.
port_reserved() {
  local port=$1 dir inst other sup name
  for dir in "$STATE_DIR"/*; do
    [ -d "$dir" ] || continue
    inst=$dir/instance.env
    [ -f "$inst" ] || continue
    other=$(field "$inst" port || true)
    [ "$other" = "$port" ] || continue
    name=$(basename "$dir")
    [ "$name" = "$NAME" ] && continue
    sup=$(field "$inst" supervisor_pid || true)
    if [ -n "$sup" ] && kill -0 "$sup" 2>/dev/null && owns_supervisor "$sup" "$name"; then
      return 0
    fi
  done
  return 1
}

# track-anything owns 8080 and 8100-8199. This repo's personal server owns 8200.
reserved_host_port() {
  local port=$1
  if [ "$port" -eq 8080 ] || [ "$port" -eq 8200 ]; then
    return 0
  fi
  if [ "$port" -ge 8100 ] && [ "$port" -le 8199 ]; then
    return 0
  fi
  return 1
}

compute_worktree_id() {
  local sum
  if command -v shasum >/dev/null 2>&1; then
    sum=$(printf '%s' "$ROOT" | shasum -a 256 | awk '{print $1}')
  elif command -v sha256sum >/dev/null 2>&1; then
    sum=$(printf '%s' "$ROOT" | sha256sum | awk '{print $1}')
  else
    echo "dev-server: shasum or sha256sum is required" >&2
    exit 1
  fi
  WORKTREE_ID="wt-${sum:0:12}"
}

new_instance_id() {
  od -An -N16 -tx1 /dev/urandom | tr -d '[:space:]'
}

dir_mtime() {
  stat -f %m "$1" 2>/dev/null || stat -c %Y "$1" 2>/dev/null || echo 0
}

resolve_registry() {
  local common
  if [ -n "${DEV_SERVER_PORT_REGISTRY:-}" ]; then
    REGISTRY=$DEV_SERVER_PORT_REGISTRY
  else
    if ! git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
      echo "dev-server: $ROOT is not a git checkout; set DEV_SERVER_PORT_REGISTRY" >&2
      exit 1
    fi
    if ! common=$(git -C "$ROOT" rev-parse --path-format=absolute --git-common-dir 2>/dev/null); then
      common=$(git -C "$ROOT" rev-parse --git-common-dir)
      case "$common" in
        /*) ;;
        *) common=$ROOT/$common ;;
      esac
      common=$(cd "$common" && pwd)
    fi
    REGISTRY=$common/djm-dev-ports
  fi
  case "$REGISTRY" in
    "$ROOT/data"|"$ROOT/data"/*|data|data/*)
      echo "dev-server: port registry must not be inside the personal data directory ($REGISTRY)" >&2
      exit 1
      ;;
  esac
  mkdir -p "$REGISTRY"
  REGISTRY=$(cd "$REGISTRY" && pwd)
  case "$REGISTRY" in
    "$ROOT/data"|"$ROOT/data"/*)
      echo "dev-server: port registry must not be inside the personal data directory ($REGISTRY)" >&2
      exit 1
      ;;
  esac
}

# A claim is live while its owner is still writing it, while its supervisor
# is still this script, or while a dead owner's grace period has not elapsed.
# A live pid whose command line is something else has been reused.
# An unreadable command line is treated as live.
claim_is_live() {
  local dir=$1
  local owner now age sup name cmd created
  owner=$dir/owner
  [ -d "$dir" ] || return 1
  now=$(date +%s)
  if [ ! -f "$owner" ]; then
    age=$((now - $(dir_mtime "$dir")))
    if [ "$age" -lt "$CLAIM_GRACE" ]; then
      return 0
    fi
    return 1
  fi
  sup=$(field "$owner" supervisor_pid || true)
  name=$(field "$owner" name || true)
  created=$(field "$owner" created_at || true)
  if [ -z "$sup" ]; then
    if [ -n "$created" ]; then
      age=$((now - created))
    else
      age=$((now - $(dir_mtime "$dir")))
    fi
    if [ "$age" -lt "$CLAIM_GRACE" ]; then
      return 0
    fi
    return 1
  fi
  if kill -0 "$sup" 2>/dev/null; then
    cmd=$(command_of "$sup")
    if [ -z "$cmd" ]; then
      return 0
    fi
    if owns_supervisor "$sup" "$name"; then
      return 0
    fi
    return 1
  fi
  if [ -n "$created" ]; then
    age=$((now - created))
  else
    age=999999
  fi
  if [ "$age" -lt "$CLAIM_GRACE" ]; then
    return 0
  fi
  return 1
}

write_owner() {
  local dir=$1 port=$2 tmp now
  now=$(date +%s)
  tmp=$(mktemp "$dir/owner.tmp.XXXXXX")
  cat > "$tmp" <<EOF
worktree=$ROOT
worktree_id=$WORKTREE_ID
name=$NAME
instance_id=$INSTANCE_ID
supervisor_pid=$$
created_at=$now
status=starting
url=http://localhost:$port
EOF
  mv "$tmp" "$dir/owner"
}

mark_claim_status() {
  local status=$1 dir owner id created tmp
  [ -n "${CLAIM_PORT:-}" ] || return 0
  dir=$REGISTRY/$CLAIM_PORT
  owner=$dir/owner
  [ -f "$owner" ] || return 0
  id=$(field "$owner" instance_id || true)
  [ "$id" = "$INSTANCE_ID" ] || return 0
  created=$(field "$owner" created_at || true)
  tmp=$(mktemp "$dir/owner.tmp.XXXXXX")
  cat > "$tmp" <<EOF
worktree=$ROOT
worktree_id=$WORKTREE_ID
name=$NAME
instance_id=$INSTANCE_ID
supervisor_pid=$$
created_at=$created
status=$status
url=$URL
EOF
  mv "$tmp" "$owner"
}

claim_dir_ok() {
  local port=$1
  local dir
  case "$port" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ -n "$REGISTRY" ] || return 1
  dir=$REGISTRY/$port
  if [ "$(dirname "$dir")" = "$REGISTRY" ]; then
    return 0
  fi
  return 1
}

# Remove a claim this process wrote. The supervisor pid must still be us.
release_matching_claim() {
  local port=$1 want_id=$2 want_pid=$3 dir owner id pid
  claim_dir_ok "$port" || return 0
  [ -n "$want_id" ] && [ -n "$want_pid" ] || return 0
  dir=$REGISTRY/$port
  owner=$dir/owner
  if [ ! -f "$owner" ]; then
    rmdir "$dir" 2>/dev/null || true
    return 0
  fi
  id=$(field "$owner" instance_id || true)
  pid=$(field "$owner" supervisor_pid || true)
  [ "$id" = "$want_id" ] && [ "$pid" = "$want_pid" ] || return 0
  rm -f "$owner"
  if [ ! -f "$owner" ]; then
    rm -rf "$dir"
  fi
}

# Remove a recorded instance's claim when its supervisor is not still this
# script. Used by stop and by a restart of the same name. A live supervisor
# is left in place, including during its grace period.
release_instance_claim() {
  local port=$1 want_id=$2 dir owner id sup name
  claim_dir_ok "$port" || return 0
  [ -n "$want_id" ] || return 0
  dir=$REGISTRY/$port
  [ -d "$dir" ] || return 0
  owner=$dir/owner
  [ -f "$owner" ] || return 0
  id=$(field "$owner" instance_id || true)
  [ "$id" = "$want_id" ] || return 0
  sup=$(field "$owner" supervisor_pid || true)
  name=$(field "$owner" name || true)
  if [ -n "$sup" ] && kill -0 "$sup" 2>/dev/null && owns_supervisor "$sup" "$name"; then
    return 0
  fi
  rm -f "$owner"
  if [ ! -f "$dir/owner" ]; then
    rm -rf "$dir"
  fi
}

# Take over an abandoned claim. The recover directory is itself created with
# mkdir so two agents cannot delete the same claim. A live claim, or one that
# changes while we look, is put back if we moved it.
reclaim_port() {
  local port=$1
  local dir recover token now age holder still moved
  dir=$REGISTRY/$port
  recover=$REGISTRY/.recover-$port
  [ -d "$dir" ] || return 0
  token=$(field "$dir/owner" instance_id || true)
  if ! mkdir "$recover" 2>/dev/null; then
    holder=$(cat "$recover/pid" 2>/dev/null || true)
    now=$(date +%s)
    age=$((now - $(dir_mtime "$recover")))
    if [ -n "$holder" ] && kill -0 "$holder" 2>/dev/null; then
      return 1
    fi
    if [ "$age" -lt "$CLAIM_GRACE" ]; then
      return 1
    fi
    rm -rf "$recover"
    mkdir "$recover" 2>/dev/null || return 1
  fi
  echo $$ > "$recover/pid"
  if [ ! -d "$dir" ]; then
    rm -rf "$recover"
    return 0
  fi
  still=$(field "$dir/owner" instance_id || true)
  if [ "$still" != "$token" ]; then
    rm -rf "$recover"
    return 1
  fi
  if claim_is_live "$dir"; then
    rm -rf "$recover"
    return 1
  fi
  if ! mv "$dir" "$recover/port" 2>/dev/null; then
    rm -rf "$recover"
    return 1
  fi
  moved=$(field "$recover/port/owner" instance_id || true)
  if [ "$moved" != "$token" ]; then
    mv "$recover/port" "$dir" 2>/dev/null || true
    rm -f "$recover/pid"
    rmdir "$recover" 2>/dev/null || true
    echo "dev-server: port $port claim changed during recovery; left it in place" >&2
    return 1
  fi
  rm -rf "$recover"
  return 0
}

try_claim() {
  local port=$1 dir
  case "$port" in
    ''|*[!0-9]*) return 1 ;;
  esac
  if [ "$port" -lt "$PORT_MIN" ] || [ "$port" -gt "$PORT_MAX" ]; then
    return 1
  fi
  if reserved_host_port "$port"; then
    return 1
  fi
  if port_reserved "$port"; then
    return 1
  fi
  dir=$REGISTRY/$port
  if mkdir "$dir" 2>/dev/null; then
    if ! write_owner "$dir" "$port"; then
      rm -rf "$dir"
      return 1
    fi
    CLAIM_PORT=$port
    if port_listening "$port"; then
      echo "dev-server: port $port is in use by another process; released this claim" >&2
      release_matching_claim "$port" "$INSTANCE_ID" "$$"
      CLAIM_PORT=""
      return 1
    fi
    return 0
  fi
  if claim_is_live "$dir"; then
    return 1
  fi
  if ! reclaim_port "$port"; then
    return 1
  fi
  if mkdir "$dir" 2>/dev/null; then
    if ! write_owner "$dir" "$port"; then
      rm -rf "$dir"
      return 1
    fi
    CLAIM_PORT=$port
    if port_listening "$port"; then
      echo "dev-server: port $port is in use by another process; released this claim" >&2
      release_matching_claim "$port" "$INSTANCE_ID" "$$"
      CLAIM_PORT=""
      return 1
    fi
    return 0
  fi
  return 1
}

# Writable agent paths must stay out of data/, which is the personal server.
refuse_personal_data() {
  local label=$1 path=$2
  case "$path" in
    "$ROOT/data"|"$ROOT/data"/*|data|data/*)
      echo "dev-server: $label must not use the personal data directory ($path)" >&2
      exit 1
      ;;
  esac
}

choose_port() {
  local preferred="" port pass
  CLAIM_PORT=""
  APP_PORT=""
  if [ -f "$INSTANCE_DIR/port" ]; then
    preferred=$(tr -d '[:space:]' < "$INSTANCE_DIR/port" || true)
  fi
  case "$preferred" in
    ''|*[!0-9]*) preferred="" ;;
  esac
  for pass in 1 2; do
    if [ -n "$preferred" ] && try_claim "$preferred"; then
      APP_PORT=$preferred
      return 0
    fi
    port=$PORT_MIN
    while [ "$port" -le "$PORT_MAX" ]; do
      if [ -n "$preferred" ] && [ "$port" = "$preferred" ]; then
        port=$((port + 1))
        continue
      fi
      if try_claim "$port"; then
        APP_PORT=$port
        return 0
      fi
      port=$((port + 1))
    done
  done
  return 1
}

acquire_lock() {
  local i=0 holder
  while true; do
    if mkdir "$LOCK_DIR" 2>/dev/null; then
      echo $$ > "$LOCK_DIR/pid"
      LOCK_HELD=1
      return 0
    fi
    holder=$(cat "$LOCK_DIR/pid" 2>/dev/null || true)
    if [ -z "$holder" ]; then
      sleep 0.1
    elif ! kill -0 "$holder" 2>/dev/null; then
      rm -rf "$LOCK_DIR"
      continue
    fi
    i=$((i + 1))
    if [ "$i" -ge 100 ]; then
      echo "dev-server: timed out waiting for $LOCK_DIR" >&2
      exit 1
    fi
    sleep 0.1
  done
}

release_lock() {
  local holder
  [ "$LOCK_HELD" = 1 ] || return 0
  holder=$(cat "$LOCK_DIR/pid" 2>/dev/null || true)
  if [ "$holder" = "$$" ]; then
    rm -rf "$LOCK_DIR"
  fi
  LOCK_HELD=0
}

# Signal only the recorded tree. Before SIGKILL, require the command line
# to be unchanged so a pid reused during the wait is not killed.
stop_recorded() {
  local go_pid=$1 server_pid=$2 config=$3 binary=$4
  local pid cmd snap pid_list="" i alive saved
  [ -n "$config$binary" ] || return 0

  if [ -n "$go_pid" ] && command_contains "$go_pid" "$config"; then
    pid_list="$go_pid $(descendant_pids "$go_pid" | tr '\n' ' ')"
  fi
  if [ -n "$server_pid" ] && command_starts_with "$server_pid" "$binary"; then
    case " $pid_list " in
      *" $server_pid "*) ;;
      *) pid_list="$pid_list $server_pid $(descendant_pids "$server_pid" | tr '\n' ' ')" ;;
    esac
  fi

  if [ -z "$(printf '%s' "$pid_list" | tr -d '[:space:]')" ]; then
    return 0
  fi

  snap=$(mktemp -d "${TMPDIR:-/tmp}/dev-server-snap.XXXXXX")
  for pid in $pid_list; do
    [ -n "$pid" ] || continue
    cmd=$(command_of "$pid")
    [ -n "$cmd" ] || continue
    printf '%s' "$cmd" > "$snap/$pid"
    kill -TERM "$pid" 2>/dev/null || true
  done

  i=0
  while [ "$i" -lt 5 ]; do
    alive=0
    for pid in $pid_list; do
      [ -f "$snap/$pid" ] || continue
      if kill -0 "$pid" 2>/dev/null; then
        alive=1
      fi
    done
    [ "$alive" = 0 ] && break
    sleep 1
    i=$((i + 1))
  done

  for pid in $pid_list; do
    [ -f "$snap/$pid" ] || continue
    if ! kill -0 "$pid" 2>/dev/null; then
      continue
    fi
    cmd=$(command_of "$pid")
    saved=$(cat "$snap/$pid")
    if [ "$cmd" = "$saved" ]; then
      kill -KILL "$pid" 2>/dev/null || true
    fi
  done
  rm -rf "$snap"
  return 0
}

kill_if_child() {
  local pid=$1 ppid
  [ -n "$pid" ] || return 0
  ppid=$(ps -p "$pid" -o ppid= 2>/dev/null | tr -d '[:space:]' || true)
  if [ "$ppid" = "$$" ]; then
    kill "$pid" 2>/dev/null || true
  fi
}

show_log() {
  echo "dev-agent: log ($LOG_FILE):" >&2
  if [ -f "$LOG_FILE" ] && [ -s "$LOG_FILE" ]; then
    tail -n 50 "$LOG_FILE" >&2 || true
  else
    echo "(no output)" >&2
  fi
}

cleanup() {
  [ "$CLEANED" = 1 ] && return 0
  CLEANED=1
  release_lock
  kill_if_child "${TAIL_PID:-}"
  # A second start of a name that is already running exits through this
  # trap and must not delete the live instance. Release the port only after
  # this process has stopped listening, and only when the claim is ours.
  if [ "$OWN_INSTANCE" = 1 ]; then
    stop_recorded "${CHILD_PID:-}" "${SERVER_PID:-}" "${INSTANCE_CONFIG:-}" "${INSTANCE_BINARY:-}" || true
    if [ -n "${INSTANCE_FILE:-}" ]; then
      rm -f "$INSTANCE_FILE" || true
    fi
    if [ -n "${INSTANCE_DIR:-}" ]; then
      rm -rf "$INSTANCE_DIR/air" || true
    fi
  fi
  release_matching_claim "${CLAIM_PORT:-}" "${INSTANCE_ID:-}" "$$" || true
}

render_air_config() {
  local dest=$1 binary=$2 tmp_dir=$3
  local line found_tmp=0 found_cmd=0 found_ep=0 reload=""
  if [ ! -f "$ROOT/.air.toml" ]; then
    echo "dev-server: missing $ROOT/.air.toml" >&2
    return 1
  fi
  if [ -n "${DEV_SERVER_RELOAD_DIR:-}" ]; then
    case "$DEV_SERVER_RELOAD_DIR" in
      /*) ;;
      *)
        echo "dev-server: DEV_SERVER_RELOAD_DIR must be an absolute path" >&2
        return 1
        ;;
    esac
    case "$DEV_SERVER_RELOAD_DIR" in
      *\"*|*\\*)
        echo "dev-server: DEV_SERVER_RELOAD_DIR must not contain quotes or backslashes" >&2
        return 1
        ;;
    esac
    reload=$DEV_SERVER_RELOAD_DIR
    mkdir -p "$reload"
  fi
  : > "$dest"
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      'tmp_dir = "'*)
        printf 'tmp_dir = "%s"\n' "$tmp_dir" >> "$dest"
        found_tmp=1
        ;;
      'cmd = "'*)
        printf '%s\n' "$(printf '%s\n' "$line" | sed "s|-o [^ ]*|-o '$binary'|")" >> "$dest"
        found_cmd=1
        ;;
      'entrypoint = ['*)
        printf 'entrypoint = ["%s"]\n' "$binary" >> "$dest"
        found_ep=1
        ;;
      '[build]')
        printf '%s\n' "$line" >> "$dest"
        if [ -n "$reload" ]; then
          printf 'include_dir = ["%s"]\n' "$reload" >> "$dest"
        fi
        ;;
      *)
        printf '%s\n' "$line" >> "$dest"
        ;;
    esac
  done < "$ROOT/.air.toml"
  if [ "$found_tmp" != 1 ] || [ "$found_cmd" != 1 ] || [ "$found_ep" != 1 ]; then
    echo "dev-server: .air.toml is missing tmp_dir, cmd, or entrypoint" >&2
    return 1
  fi
}

foreign_listener() {
  local port=$1 ancestor=$2 pid
  for pid in $(listening_pids "$port"); do
    [ -n "$pid" ] || continue
    if ! is_descendant "$pid" "$ancestor"; then
      printf '%s\n' "$pid"
      return 0
    fi
  done
  return 1
}

health_ok() {
  curl -fsS --max-time 1 "http://127.0.0.1:$1/healthz" >/dev/null 2>&1
}

first_owned_listener() {
  local port=$1 ancestor=$2 pid
  for pid in $(listening_pids "$port"); do
    [ -n "$pid" ] || continue
    if is_descendant "$pid" "$ancestor"; then
      printf '%s\n' "$pid"
      return 0
    fi
  done
  return 1
}

recover_stale() {
  local inst=$1 go_pid server_pid config binary sup port instance_id
  sup=$(field "$inst" supervisor_pid || true)
  if [ -n "$sup" ] && kill -0 "$sup" 2>/dev/null && owns_supervisor "$sup" "$NAME"; then
    URL=$(field "$inst" url)
    APP_PORT=$(field "$inst" port)
    echo "dev-agent: $NAME is already running at $URL"
    return 1
  fi
  port=$(field "$inst" port || true)
  instance_id=$(field "$inst" instance_id || true)
  go_pid=$(field "$inst" go_pid || true)
  server_pid=$(field "$inst" server_pid || true)
  config=$(field "$inst" config || true)
  binary=$(field "$inst" binary || true)
  stop_recorded "$go_pid" "$server_pid" "$config" "$binary"
  # The same name is restarting and its supervisor is gone. Drop that claim
  # now, including during the grace period. Another worktree still waits.
  release_instance_claim "$port" "$instance_id"
  rm -f "$inst"
  return 0
}

wait_until_ready() {
  local deadline foreign listener code
  deadline=$((SECONDS + READY_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if ! kill -0 "$CHILD_PID" 2>/dev/null; then
      set +e
      wait "$CHILD_PID"
      code=$?
      set -e
      echo "dev-agent: $NAME exited before it was ready (status $code)" >&2
      show_log
      exit 1
    fi
    foreign=$(foreign_listener "$APP_PORT" "$CHILD_PID" || true)
    if [ -n "$foreign" ]; then
      echo "dev-agent: port $APP_PORT is in use by pid $foreign; left that process running" >&2
      exit 1
    fi
    if health_ok "$APP_PORT"; then
      listener=$(first_owned_listener "$APP_PORT" "$CHILD_PID" || true)
      if [ -n "$listener" ]; then
        SERVER_PID=$listener
        mark_claim_status running || true
        publish_instance running
        return 0
      fi
    fi
    sleep 0.2
  done
  echo "dev-agent: $NAME did not become ready within ${READY_TIMEOUT}s" >&2
  echo "dev-agent: the process is still running; stopping it" >&2
  show_log
  exit 1
}

cmd_start() {
  NAME=$1
  validate_name "$NAME"
  require_cmd lsof
  require_cmd curl
  require_cmd pgrep
  prepare_state

  INSTANCE_DIR=$STATE_DIR/$NAME
  INSTANCE_FILE=$INSTANCE_DIR/instance.env
  mkdir -p "$INSTANCE_DIR"

  trap cleanup EXIT
  trap 'exit 0' INT TERM HUP

  acquire_lock
  if [ -f "$INSTANCE_FILE" ]; then
    if ! recover_stale "$INSTANCE_FILE"; then
      release_lock
      exit 0
    fi
  fi

  INSTANCE_ID=$(new_instance_id)
  if [ -z "$INSTANCE_ID" ]; then
    echo "dev-server: could not create an instance id" >&2
    release_lock
    exit 1
  fi
  if ! choose_port; then
    echo "dev-server: no free port in $PORT_MIN-$PORT_MAX" >&2
    release_lock
    exit 1
  fi
  if reserved_host_port "$APP_PORT"; then
    echo "dev-server: port $APP_PORT is reserved for another server" >&2
    release_lock
    exit 1
  fi
  URL="http://localhost:$APP_PORT"
  INSTANCE_BINARY=$INSTANCE_DIR/air/main
  INSTANCE_CONFIG=$INSTANCE_DIR/air.toml
  LOG_FILE=$INSTANCE_DIR/air.log
  mkdir -p "$INSTANCE_DIR/air" "$INSTANCE_DIR/data"
  if ! render_air_config "$INSTANCE_CONFIG" "$INSTANCE_BINARY" "$INSTANCE_DIR/air"; then
    release_lock
    exit 1
  fi
  printf '%s\n' "$APP_PORT" > "$INSTANCE_DIR/port"
  OWN_INSTANCE=1
  publish_instance starting
  release_lock

  echo "dev-agent: starting name=$NAME port=$APP_PORT url=$URL"
  : > "$LOG_FILE"
  tail -n +1 -f "$LOG_FILE" &
  TAIL_PID=$!

  export ADDR=":$APP_PORT"
  export PORT="$APP_PORT"
  export DB_PATH="$INSTANCE_DIR/data/djm.db"
  export BASE_URL="$URL"
  export BLOB_DIR="$INSTANCE_DIR/blobs"
  export CATALOG_CACHE_DIR="$INSTANCE_DIR/catalogs"
  refuse_personal_data DB_PATH "$DB_PATH"
  refuse_personal_data BLOB_DIR "$BLOB_DIR"
  refuse_personal_data CATALOG_CACHE_DIR "$CATALOG_CACHE_DIR"
  mkdir -p "$INSTANCE_DIR/data" "$INSTANCE_DIR/blobs" "$INSTANCE_DIR/catalogs"

  # The config path is on the command line so stop can recognize the process.
  if [ -n "${DEV_SERVER_COMMAND:-}" ]; then
    "$DEV_SERVER_COMMAND" "$INSTANCE_CONFIG" >>"$LOG_FILE" 2>&1 &
  else
    go run "$AIR_PKG" -c "$INSTANCE_CONFIG" >>"$LOG_FILE" 2>&1 &
  fi
  CHILD_PID=$!
  publish_instance starting

  wait_until_ready
  echo "dev-agent: ready name=$NAME port=$APP_PORT url=$URL"

  set +e
  wait "$CHILD_PID"
  status=$?
  set -e
  exit "$status"
}

stop_one() {
  local name=$1 dir inst sup go_pid server_pid config binary i port instance_id
  validate_name "$name"
  prepare_state
  dir=$STATE_DIR/$name
  inst=$dir/instance.env
  if [ ! -f "$inst" ]; then
    echo "dev-server: $name is not running" >&2
    return 1
  fi
  sup=$(field "$inst" supervisor_pid || true)
  go_pid=$(field "$inst" go_pid || true)
  server_pid=$(field "$inst" server_pid || true)
  config=$(field "$inst" config || true)
  binary=$(field "$inst" binary || true)
  port=$(field "$inst" port || true)
  instance_id=$(field "$inst" instance_id || true)

  if [ -n "$sup" ] && kill -0 "$sup" 2>/dev/null; then
    if owns_supervisor "$sup" "$name"; then
      kill -TERM "$sup" 2>/dev/null || true
      i=0
      while [ "$i" -lt 24 ]; do
        if ! kill -0 "$sup" 2>/dev/null; then
          break
        fi
        if ! owns_supervisor "$sup" "$name"; then
          break
        fi
        sleep 0.25
        i=$((i + 1))
      done
      if kill -0 "$sup" 2>/dev/null && owns_supervisor "$sup" "$name"; then
        kill -KILL "$sup" 2>/dev/null || true
      fi
    else
      echo "dev-server: $name pid $sup is not its dev server; not killing it" >&2
    fi
  fi

  stop_recorded "$go_pid" "$server_pid" "$config" "$binary"
  release_instance_claim "$port" "$instance_id"
  rm -f "$inst"
  rm -rf "$dir/air"
  return 0
}

cmd_stop() {
  stop_one "$1"
}

cmd_list() {
  local dir inst name port url sup status found=0 owner wt
  prepare_state
  for dir in "$STATE_DIR"/*; do
    [ -d "$dir" ] || continue
    inst=$dir/instance.env
    [ -f "$inst" ] || continue
    found=1
    name=$(field "$inst" name || basename "$dir")
    port=$(field "$inst" port || true)
    url=$(field "$inst" url || true)
    sup=$(field "$inst" supervisor_pid || true)
    status=$(field "$inst" status || true)
    if [ -n "$sup" ] && kill -0 "$sup" 2>/dev/null && owns_supervisor "$sup" "$name"; then
      [ "$status" = "running" ] || status=starting
    else
      status=stale
    fi
    printf '%s %s %s %s\n' "$name" "$port" "$url" "$status"
  done
  # Claims from other worktrees are visible here and are not local servers.
  # stop and stop-all do not signal them.
  for dir in "$REGISTRY"/*; do
    [ -d "$dir" ] || continue
    owner=$dir/owner
    [ -f "$owner" ] || continue
    wt=$(field "$owner" worktree || true)
    [ "$wt" = "$ROOT" ] && continue
    found=1
    name=$(field "$owner" name || true)
    port=$(basename "$dir")
    url=$(field "$owner" url || true)
    [ -n "$url" ] || url="http://localhost:$port"
    if claim_is_live "$dir"; then
      status=external
    else
      status=external-stale
    fi
    printf '%s %s %s %s %s\n' "$name" "$port" "$url" "$status" "$wt"
  done
  if [ "$found" = 0 ]; then
    echo "no agent dev servers"
  fi
}

cmd_stop_all() {
  local dir name found=0
  prepare_state
  for dir in "$STATE_DIR"/*; do
    [ -d "$dir" ] || continue
    [ -f "$dir/instance.env" ] || continue
    found=1
    name=$(basename "$dir")
    stop_one "$name" || true
  done
  if [ "$found" = 0 ]; then
    echo "no agent dev servers"
  fi
}

main() {
  local cmd=${1:-}
  case "$cmd" in
    start)
      [ $# -eq 2 ] || usage
      cmd_start "$2"
      ;;
    stop)
      [ $# -eq 2 ] || usage
      cmd_stop "$2"
      ;;
    list)
      [ $# -eq 1 ] || usage
      cmd_list
      ;;
    stop-all)
      [ $# -eq 1 ] || usage
      cmd_stop_all
      ;;
    *)
      usage
      ;;
  esac
}

main "$@"
