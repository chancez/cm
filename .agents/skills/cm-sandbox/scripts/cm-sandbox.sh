#!/bin/sh
# Run cm in an isolated environment that cannot touch the developer's own sessions.
#
# Every subcommand operates on a named sandbox under $TMPDIR, with its own runtime dir, state dir, and
# config. Nothing here can reach the real server: the isolation is by directory, and cm resolves its
# socket from CM_RUNTIME_DIR.
#
# Usage: cm-sandbox.sh <command> [name] [args...]
set -eu

root="${TMPDIR:-/tmp}/cm-sandbox"

usage() {
  cat >&2 <<'USAGE'
usage: cm-sandbox.sh <command> [name] [args...]

  new NAME            create a sandbox and start its server
  run NAME ARGS...    run cm in the sandbox (e.g. run t1 list)
  env NAME            print the exports, for eval in your own shell
  ps NAME             show the sandbox's server and shim processes
  check NAME          prove the sandbox is isolated
  rm NAME             kill its sessions, stop its server, delete its directories
  ls                  list sandboxes
  rm-all              remove every sandbox this script created
USAGE
  exit 2
}

# cm_bin prefers the repo build, so a sandbox tests the working tree rather than the installed binary.
#
# Built if missing rather than assumed: testing an installed cm while editing the source is a false pass,
# and it is not obvious from the output which one ran.
cm_bin() {
  if [ -n "${CM_SANDBOX_BIN:-}" ]; then
    printf '%s\n' "$CM_SANDBOX_BIN"
    return
  fi
  repo=$(git rev-parse --show-toplevel 2>/dev/null || echo "")
  if [ -n "$repo" ] && [ -f "$repo/go.mod" ]; then
    ( cd "$repo" && go build -o bin/cm ./cmd/cm >/dev/null )
    printf '%s\n' "$repo/bin/cm"
    return
  fi
  command -v cm
}

dir_for() {
  [ -n "${1:-}" ] || usage
  # A short path on purpose. sockaddr_un caps a socket path at 104 bytes on darwin and the failure is a
  # bare EINVAL, so the sandbox root stays shallow and the name should too.
  printf '%s/%s\n' "$root" "$1"
}

# sandbox_env prints the three variables that isolate cm.
#
# CM_CONFIG points at a file that does not exist rather than being empty, and the difference is not
# cosmetic: an empty value means *unset*, so cm falls through to XDG_CONFIG_HOME and then to the real
# config file. An entire e2e suite silently read the developer's detach_key that way. Verified: with
# CM_CONFIG= the sandbox reports the developer's key, and with this it reports the default.
sandbox_env() {
  d=$(dir_for "$1")
  printf 'export CM_RUNTIME_DIR=%s/r\n' "$d"
  printf 'export CM_STATE_DIR=%s/s\n' "$d"
  printf 'export CM_CONFIG=%s/absent.toml\n' "$d"
}

cmd=${1:-}; [ -n "$cmd" ] || usage
shift || true

case "$cmd" in
  new)
    name=${1:-}; [ -n "$name" ] || usage
    d=$(dir_for "$name")
    mkdir -p "$d/r" "$d/s" "$d/xdg"
    # 0700, since a session log holds everything typed at a prompt.
    chmod 700 "$d" "$d/r" "$d/s"
    bin=$(cm_bin)
    # Start the server up front. Every cm command auto-starts one, and two issued close together each
    # start one; the loser exits after the winner binds the socket, and a command that raced it sees an
    # empty session list.
    CM_RUNTIME_DIR="$d/r" CM_STATE_DIR="$d/s" CM_CONFIG="$d/absent.toml" XDG_CONFIG_HOME="$d/xdg" \
      "$bin" list >/dev/null
    printf 'sandbox %s ready\n' "$name"
    printf 'binary: %s\n' "$bin"
    printf 'eval "$(%s env %s)" to use it directly\n' "$0" "$name"
    ;;

  run)
    name=${1:-}; [ -n "$name" ] || usage
    shift
    d=$(dir_for "$name")
    [ -d "$d" ] || { printf 'no sandbox %s; run `new %s` first\n' "$name" "$name" >&2; exit 1; }
    bin=$(cm_bin)
    exec env CM_RUNTIME_DIR="$d/r" CM_STATE_DIR="$d/s" CM_CONFIG="$d/absent.toml" \
      XDG_CONFIG_HOME="$d/xdg" "$bin" "$@"
    ;;

  env)
    sandbox_env "${1:-}"
    ;;

  check)
    name=${1:-}; [ -n "$name" ] || usage
    d=$(dir_for "$name")
    bin=$(cm_bin)
    printf 'binary:   %s\n' "$bin"
    # cm config reports where each value came from, which is what makes this a proof rather than a hope.
    env CM_RUNTIME_DIR="$d/r" CM_STATE_DIR="$d/s" CM_CONFIG="$d/absent.toml" XDG_CONFIG_HOME="$d/xdg" \
      "$bin" config | grep -E 'runtime_dir|state_dir|config|detach_key' || true
    printf '\nif runtime_dir and state_dir are not under %s, the sandbox is NOT isolated\n' "$d"
    ;;

  ps)
    name=${1:-}; [ -n "$name" ] || usage
    d=$(dir_for "$name")
    # Matched on this sandbox's own runtime dir, never on the string "cm": a bare pattern would match the
    # developer's real server and any other sandbox.
    pgrep -fl "runtime-dir $d/r" || printf 'no processes for sandbox %s\n' "$name"
    ;;

  rm)
    name=${1:-}; [ -n "$name" ] || usage
    d=$(dir_for "$name")
    [ -d "$d" ] || { printf 'no sandbox %s\n' "$name" >&2; exit 0; }
    bin=$(cm_bin)
    # Sessions first, then the server. A shim outlives its server and holds a pty; macOS caps ptys at 511
    # system-wide, and exhaustion surfaces as "device not configured" somewhere unrelated.
    env CM_RUNTIME_DIR="$d/r" CM_STATE_DIR="$d/s" CM_CONFIG="$d/absent.toml" \
      "$bin" kill --all >/dev/null 2>&1 || true
    env CM_RUNTIME_DIR="$d/r" CM_STATE_DIR="$d/s" CM_CONFIG="$d/absent.toml" \
      "$bin" server stop >/dev/null 2>&1 || true
    # Anything still alive belongs to this sandbox by construction, since the pattern is its own directory.
    sleep 1
    pkill -f "runtime-dir $d/r" 2>/dev/null || true
    rm -rf "$d"
    printf 'removed sandbox %s\n' "$name"
    ;;

  ls)
    [ -d "$root" ] || { printf 'no sandboxes\n'; exit 0; }
    ls -1 "$root" 2>/dev/null || printf 'no sandboxes\n'
    ;;

  rm-all)
    [ -d "$root" ] || { printf 'no sandboxes\n'; exit 0; }
    for d in "$root"/*; do
      [ -d "$d" ] || continue
      "$0" rm "$(basename "$d")"
    done
    ;;

  *)
    usage
    ;;
esac
