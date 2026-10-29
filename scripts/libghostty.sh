#!/bin/sh
# Build libghostty-vt at GHOSTTY_REF into GHOSTTY_PREFIX, unless that prefix already holds this build.
#
# The one place the build is written down. mise's libghostty task, Dockerfile.test, and both workflows run
# this rather than their own copy: they used to each carry one, the copies drifted, and the task's copy had
# no coverage anywhere, which is how it shipped broken twice.
#
# Environment:
#   GHOSTTY_REF      required, the full commit SHA. `git fetch` resolves complete object names only, so an
#                    abbreviation fails with "couldn't find remote ref".
#   GHOSTTY_PREFIX   required, where the library and its pkg-config files are installed. The .pc files
#                    record this path, so the result must be used where it was built.
#   GHOSTTY_DIR      a git repository to fetch into, cloned there if absent. Its working tree is never
#                    touched: the build runs from an exported copy of the pinned commit.
#   GHOSTTY_TARGET   optional zig target, e.g. x86_64-linux-gnu.2.34. Unset builds for this machine.
#   ZIG              optional, the zig to run. Defaults to the one on PATH.
set -eu

: "${GHOSTTY_REF:?GHOSTTY_REF is not set}"
: "${GHOSTTY_PREFIX:?GHOSTTY_PREFIX is not set}"
: "${GHOSTTY_DIR:?GHOSTTY_DIR is not set}"
zig="${ZIG:-zig}"
target="${GHOSTTY_TARGET:-}"

case "$GHOSTTY_REF" in
  *[!0-9a-f]* | "") echo "GHOSTTY_REF must be a full commit SHA, got '$GHOSTTY_REF'" >&2; exit 1 ;;
esac
if [ "${#GHOSTTY_REF}" -ne 40 ]; then
  echo "GHOSTTY_REF must be a full 40-character SHA, got '$GHOSTTY_REF'" >&2
  exit 1
fi

# -Doptimize is not a tuning knob, and omitting it is a bug.
#
# Zig defaults to Debug, and ghostty derives `slow_runtime_safety` from the optimize mode. In Debug that turns
# on integrity verification that walks a whole page, and `insertLines` calls it once per row it shifts. A
# reverse index with the cursor on the top row goes through insertLines, so it cost 14ms at 50x120 against
# 10us anywhere else, and `less` paging up, which emits one per line, was visibly delayed.
#
# ReleaseSafe rather than ReleaseFast: 15us against 12us for the same half page, not worth giving up bounds
# and overflow checks in a parser whose input is whatever a program decides to print.
#
# emit-xcframework defaults to "xcodebuild on PATH", which pulls in the iOS SDK and fails without full
# Xcode, so disabling it is not redundant.
set -- -Demit-lib-vt=true -Demit-xcframework=false -Demit-macos-app=false -Doptimize=ReleaseSafe
if [ -n "$target" ]; then
  set -- "$@" "-Dtarget=$target"
fi

# What a finished build was made from. Written last, so a build that failed partway is rebuilt rather than
# mistaken for complete, and compared whole, so changing a flag above rebuilds too.
stamp="$GHOSTTY_PREFIX/.cm-build"
want="ref=$GHOSTTY_REF flags=$*"
if [ -f "$stamp" ] && [ "$(cat "$stamp")" = "$want" ]; then
  exit 0
fi

if ! git -C "$GHOSTTY_DIR" rev-parse --git-dir >/dev/null 2>&1; then
  mkdir -p "$(dirname "$GHOSTTY_DIR")"
  git clone --filter=blob:none --no-checkout https://github.com/ghostty-org/ghostty.git "$GHOSTTY_DIR"
fi
if ! git -C "$GHOSTTY_DIR" cat-file -e "$GHOSTTY_REF^{commit}" 2>/dev/null; then
  # Shallow only where the repository already is. --depth on a complete clone would make it shallow, which
  # is a change to someone's repository that nothing here needs.
  if [ "$(git -C "$GHOSTTY_DIR" rev-parse --is-shallow-repository)" = true ]; then
    git -C "$GHOSTTY_DIR" fetch --depth 1 origin "$GHOSTTY_REF"
  else
    git -C "$GHOSTTY_DIR" fetch origin "$GHOSTTY_REF"
  fi
fi

src="$(mktemp -d "${TMPDIR:-/tmp}/libghostty.XXXXXX")"
trap 'rm -rf "$src"' EXIT
git -C "$GHOSTTY_DIR" archive "$GHOSTTY_REF" | tar -x -C "$src"

echo "building libghostty-vt $GHOSTTY_REF into $GHOSTTY_PREFIX" >&2
rm -rf "$GHOSTTY_PREFIX"
(cd "$src" && "$zig" build "$@" --prefix "$GHOSTTY_PREFIX")
printf '%s\n' "$want" >"$stamp"
