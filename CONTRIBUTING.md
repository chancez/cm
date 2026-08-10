# Contributing to cm

This covers building, testing, and the conventions. `docs/` holds the design decisions, and
[AGENTS.md](AGENTS.md) has the same rules as this file with the incident behind each, plus the
procedures a coding agent working here needs.

## The one rule that matters most

**Never run a bare `cm` command against your own setup.**

Whoever works on cm is usually *using* cm, with real sessions holding real work. A bare `cm` in this
repo talks to that running server, so `cm kill --all` or `cm server stop` takes those sessions with
it. Every manual test runs in an isolated environment:

```sh
export CM_RUNTIME_DIR=$(mktemp -d /tmp/cmdev.XXXX)
export CM_STATE_DIR=$(mktemp -d /tmp/cmdevs.XXXX)
export CM_CONFIG=/nonexistent.toml
```

`CM_CONFIG` points at a file that does not exist on purpose. An *empty* value means unset, so cm
falls through to the real config file. That mistake had an entire e2e suite reading the developer's
`detach_key` for weeks while looking isolated.

Prove the isolation rather than assuming it: `cm config` inside the sandbox must report the default
`detach_key ctrl-\` and the temporary directories. Unset `CM_SESSION` too, since it is inherited, and
a bare `cm attach` in a sandbox retargets the session that launched it instead of creating one.

Clean up what you start, and check before killing anything you did not start. `clients=0` does not
mean abandoned, since a detached session can hold a live shell with someone's work in it.

## Setup

Toolchains are pinned in `mise.toml`. [mise](https://mise.jdx.dev) is the fast path, but nothing
requires it: Go and, for the emulator, Zig 0.16.0 are the only hard requirements.

```sh
mise install          # pinned Go, Zig, protoc, buf, fish
mise run libghostty   # build libghostty-vt into third_party/ (slow, cached)
mise run build        # -> bin/cm
```

`mise run libghostty` is needed once, and again whenever `GHOSTTY_REF` in `mise.toml` moves.
libghostty-vt has no tagged release and its API is explicitly unstable, so the ref is a pinned
commit and bumping it is a deliberate act.

Pass `-Doptimize=ReleaseSafe` if you build it by hand. Zig defaults to Debug, which turns on
per-page integrity verification in ghostty, and that is not a tuning knob: a reverse index with the
cursor on the top row cost 14ms at 50x120 against 10us elsewhere, so paging up in `less` was
visibly delayed while paging down was not.

### cgo is required

`internal/vt` wraps libghostty-vt, and `cm read`, `cm history`, and screen restore all depend on it.
A `CGO_ENABLED=0` build fails deliberately, with an error naming the reason.

There used to be a `!cgo` stub and a second no-cgo Linux image, on the theory that cm should degrade
rather than break without the emulator. Both were retired: a build where those commands return empty
*successfully* is a worse outcome than one that does not build, and it cost real debugging time
twice, each time looking like a bug in cm rather than a missing emulator.

## Installing

```sh
mise run install                      # into ~/.local/bin
PREFIX=/usr/local mise run install    # or anywhere else
mise run uninstall                    # removes it again
```

The install renames into place rather than copying over the existing file, which matters more than it
sounds. Copying onto a path whose binary has a running process gets every later invocation SIGKILLed
on macOS: `cp` writes into the existing inode and invalidates the kernel's cached code-signature
pages that the live process still maps. It presents as `zsh: killed cm ls` with nothing in any log.
A rename replaces the directory entry instead, so the new binary is a new inode, the swap is atomic,
and an already-running server keeps working on the old one until it is restarted.

## Testing

```sh
mise run check                                # fmt, vet, test
go test ./...                                 # everything
go test -short ./...                          # skips e2e, which spawn real processes and ptys
go test -race ./internal/... ./cmd/...
mise run test-linux                           # the full suite on Linux, in Docker
mise run build-linux                          # just check the Linux build compiles
```

Run `-race` before believing a concurrency change, and `mise run test-linux` before believing
anything platform-specific. A macOS-only run never compiles the Linux paths, and `/bin/sh` there is
dash rather than bash, which has caught real bugs. The image builds libghostty from source, so screen
restore, history, and adoption-with-scrollback are covered rather than skipped.

`mise run generate` regenerates protobuf and ttrpc code after editing `proto/`. The `.proto` files
are the contract.

### Testing rules

- **Every fix ships with a test that would have caught the bug.** Prefer the seam over end to end.
  `fakeTerminal` in `internal/server/session_test.go` exists for this.
- **Assert the whole value a function returns, not individual fields.** A field-by-field check passes
  while the rest of the struct is wrong.
- **Verify the test fails with the fix reverted.** Not "probably would": actually revert it and
  watch. Several tests here passed for the wrong reason, including a control that never fired and a
  needle containing a Go-escaped string rather than real bytes.
- **A flaky test is a bug until proven otherwise.** If a regression test catches the bug 1 time in 6,
  it is not standing guard. Move the assertion to the unit level where the state can be constructed.
- **Use `testing/synctest`** for concurrency and timing.
- **Test-only behavior goes behind the `cm_testhooks` build tag**, so a released binary does not
  contain the code at all. An env var a shipped binary honors is one a stale `export` can use to make
  it lie.

### Anything touching escape sequences

Read [docs/testing.md](docs/testing.md) first. That conversation is where most of cm's bugs live and
it is the hardest thing here to observe, because a wrong result looks like a clean pass. Four traps
from it:

- **The control for a multiplexer bug is another multiplexer**, not a bare terminal. Comparing
  against bare kitty said "not a cm bug" about a bug that reproduced against zmx on the first try.
- **`cm read --raw` is not the byte stream.** It re-serializes the terminal model, so a session whose
  log really did contain OSC sequences showed none. Use `--raw --follow` redirected to a file, or log
  `%q` at the hop in question.
- **Client count changes behavior.** Zero, one interactive, one read-only, and many are four
  different cases, and the first and third are where a failure is a *hang* rather than an artifact.
- **Drive the pty directly** with `printf 'A\033[6nB'` rather than running vim, which confounds the
  test and can stop exercising it silently.

Anything that depends on real rendering or real keypresses (attach, detach, screen restore, the
detach key) needs a real terminal, and it should be a throwaway one rather than the terminal you are
working in. `cm attach` takes over the terminal that invoked it, and the detach key belongs to
whatever is outermost, so a window can end up unusable.

## Docs

Update them when behavior changes. `docs/` is a set of decision records, not a manual: each file says
what was chosen, what was measured, and what was rejected. Adding to one is cheap; leaving a false
claim in one is expensive, because the next person trusts it.

When a measurement decides something, record the number. "Any cm invocation costs about 23ms" and
"Connect adds 10.9 MB" are both load-bearing facts that would otherwise get guessed at.

Three places for what you learn, by kind:

- **`cm doctor`** for anything a user could hit whose symptom points somewhere other than the cause.
  The bar is that it must correspond to something that actually went wrong; a check for a
  hypothetical is noise, and noise teaches people to ignore diagnostics.
- **`docs/`** for a decision and its alternatives.
- **[docs/ideas.md](docs/ideas.md)** for something worth doing later, with what it would cost.

## Commits

Small logical commits, one concern each, and every commit builds and passes tests so history
bisects. Commit messages explain why, and say what was measured: the numbers here are load-bearing,
and one that goes unrecorded gets re-derived or guessed.

## Releases

Pushing a `v*` tag builds a binary per platform and publishes a GitHub Release with a `SHA256SUMS`
file covering every archive. One job per target rather than a cross-compile, because cgo means every
target needs libghostty built for its own OS and architecture.

The version is not stamped with `-ldflags`. `paths.Version` reads it from the Go build info, so a
build from a tagged commit reports the tag on its own, and the workflow verifies that rather than
trusting it. It also fails on a dirty checkout, since the toolchain stamps `vcs.modified=true` and
the binary would then silently report `-dirty` about a tag build.

A tag containing a dash is published as a pre-release, matching how `paths.isReleaseVersion` reads
one.

## Out of scope, permanently

Windows, tabs, and splits. The terminal emulator already does those, and not competing with it is
why cm is small. [docs/ideas.md](docs/ideas.md) records what is being considered and what has been
ruled out.
