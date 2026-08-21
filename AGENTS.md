# Working on cm

Notes for anyone, human or agent, making changes here. `README.md` describes what cm is and
`docs/` holds the design decisions; this file is about how to work on it without breaking things.

`CONTRIBUTING.md` is the short version of this file, for a human arriving from the README. It states
the rules; this one states them with the incident behind each. When a rule changes, change it here
and check whether `CONTRIBUTING.md` repeats it.

Everything here is a lesson from something that went wrong. Where a rule looks fussy, the reason is
stated, because a rule without its reason gets dropped the first time it is inconvenient.

## What cm is, briefly

Three layers: a client, one server, and one shim per session. The shim owns the pty and a sequenced
output log; the server owns session bookkeeping and the terminal model; clients never talk to a shim.
That split is why a server can be restarted or upgraded without killing running shells.

Two consequences shape most changes. A session outlives the server, so anything the server holds must
be rediscoverable rather than authoritative. And cgo is required: `internal/vt` wraps libghostty-vt,
and `cm read`, `cm history`, and screen restore all depend on it.

Out of scope, permanently: windows, tabs, splits. The terminal emulator does those. See
`docs/ideas.md` for what is being considered and what has been ruled out.

## Setup, build, test

```
mise install          # pinned Go, Zig, protoc, buf, fish
mise run libghostty   # builds libghostty-vt once, into third_party/ (slow, cached)
mise run build        # -> bin/cm
mise run check        # fmt, vet, test, validate the plugin manifests
```

Tests:

```
go test ./...                     # everything
go test -short ./...              # skips e2e, which spawn real processes and ptys
go test -race ./internal/... ./cmd/...
mise run test-linux               # the full suite on Linux in Docker
```

Run `-race` before believing a concurrency change, and `mise run test-linux` before believing
anything platform-specific: a macOS-only run never compiles the Linux paths, and `/bin/sh` there is
dash rather than bash, which has caught real bugs.

`mise run generate` regenerates protobuf and ttrpc code after editing `proto/`. The `.proto` files are
the contract.

## Never run cm against the developer's own setup

This is the rule that matters most, and the one easiest to break by accident.

The person working on cm is usually *using* cm, with real sessions holding real work. A bare `cm`
command in this repo talks to their running server. `cm kill --all` or `cm server stop` would take
their sessions with it.

So every manual test runs in an isolated environment. There is a skill for this,
`.agents/skills/cm-sandbox/`, and it is worth reading before the first time. The short version:

```sh
export CM_RUNTIME_DIR=$(mktemp -d /tmp/cmdev.XXXX)
export CM_STATE_DIR=$(mktemp -d /tmp/cmdevs.XXXX)
export CM_CONFIG=/nonexistent.toml
```

`CM_CONFIG` pointing at a file that does not exist is deliberate: an *empty* value means unset, so cm
falls through to the real config file. That mistake made an entire e2e suite read the developer's
`detach_key` for weeks while looking isolated.

Clean up what you start, and check before killing anything you did not start. `clients=0` does not mean
abandoned: a detached session can hold a live shell with someone's work in it.

## Skills

Two sets, kept apart on purpose.

**`.agents/skills/`** is for working *on* cm, and is loaded automatically here: `.claude/skills` is a
symlink to it, so an agent in this repo picks these up. Read the relevant one rather than re-deriving it.

- **`cm-sandbox`** -- run cm by hand without touching the developer's sessions. Required before any
  manual test that mutates state, and it includes a `check` that *proves* the isolation rather than
  assuming it.
- **`cm-verify-fix`** -- prove a fix works and that its test would have caught the bug: reverting to
  check the test fails, mutation testing without losing uncommitted work, and turning a race into
  something deterministic.

**`skills/cm/`** is for *using* cm, and ships as the tool's own skill for anyone driving sessions or
orchestrating agents with it. It is deliberately not in `.agents/`: it teaches `cm attach`, `cm kill`,
and `cm send` against whatever server is running, which is exactly what an agent developing cm must not
do. If you are here to change cm, use the sandbox skill instead.

`skills/cm/` is also distributed through the Claude Code plugin marketplace this repo declares, so it
can be installed with `/plugin marketplace add chancez/cm` rather than copied by hand. Two files carry
that, and the layout is the part worth understanding before changing either:

- `.claude-plugin/marketplace.json` is the catalog. It must be at the repo root for
  `/plugin marketplace add chancez/cm` to find it, and that location is Claude Code's requirement
  rather than a choice.
- `.claude-plugin/plugins/cm/` is the plugin, and holds `skills/cm` as a symlink to
  `../../../../skills/cm` rather than a copy. Claude Code dereferences symlinks when it installs, so
  users get real files, and there is only one SKILL.md in the repo to edit. If you move either
  directory, fix that symlink: its `../` count is relative to where it sits.

Everything Claude-specific lives under `.claude-plugin/` on purpose, including the nested
`.claude-plugin/plugins/cm/.claude-plugin/plugin.json`, which reads oddly but installs correctly and
is verified by a test. A top-level `plugins/` would look like a cm concept, and `.claude/` is worse:
that directory is agent *configuration* in normal use, and here it already holds the
`skills -> ../.agents/skills` symlink, so a distributable plugin sitting next to it would be read as
part of this repo's own agent setup.

The plugin is a subdirectory rather than the repo root, which is a size decision and was measured: a
root source (`"source": "./"`) copies the whole checkout into every user's plugin cache, 2.6 MB against
28 KB, and re-copies it on each update. It also puts `.agents/skills/` inside the installed plugin,
which are the develop-on-cm skills and exactly what a user driving sessions must not get.

`mise run check` validates both files, so a typo fails locally rather than at someone else's
`/plugin install`. `claude plugin validate` only checks the catalog when pointed at the root: the
plugin's own frontmatter needs a second run against the plugin directory, and the task does both.

Neither covers a change needing a *real terminal*: attach, detach, screen restore, and the detach key
depend on real rendering and real keypresses. Those need a throwaway terminal instance, never the one
you are running in, because `cm attach` takes over the terminal that invoked it and the detach key
belongs to whatever is outermost, so the window can end up unusable.

Launching one is about the developer's terminal rather than about cm, so it is local tooling and not
in this repo. Check your available skills and `AGENTS.local.md` or `CLAUDE.local.md` for what this
machine provides. If there is nothing, say so and ask rather than testing in the live terminal.

## Testing rules

**Every fix ships with a test that would have caught the bug.** Prefer the seam over end to end.
`fakeTerminal` in `internal/server/session_test.go` exists for this.

**Assert the whole value a function returns, not individual fields.** A field-by-field check passes
while the rest of the struct is wrong.

**A test that never fails is worse than no test.** Several attempts here passed for the wrong reason: a
control that never fired, a needle containing a Go-escaped string rather than real bytes, a pty echoing
control characters in caret notation. So:

- Verify the test fails with the fix reverted. Not "probably would" -- actually revert it and watch.
- If it is a race, measure the rate both ways. A regression test that catches the bug 1 time in 6 is
  not standing guard; move the assertion to the unit level where the state can be constructed.
- Confirm any mutation you make to check a test actually compiles. A mutation that fails to build
  tests nothing, and `go test` reports the build failure as a failure, which looks like success.

**A flaky test is a bug until proven otherwise.** `-race` widens real windows. The method that has
worked repeatedly: confirm it is not deterministic without `-race`; read the code path the error
message names rather than trying to reproduce harder; then reproduce at the unit level by *waiting for*
the state the race lands in instead of racing it.

**Use `testing/synctest` for concurrency and timing.**

**Test-only behavior goes behind the `cm_testhooks` build tag**, so a released binary does not contain
the code at all. `CM_VERSION` and `CM_SOCKET_WATCH_INTERVAL` work this way. An env var a shipped binary
honors is one a stale `export` can use to make it lie.

**Anything touching escape sequences: read `docs/testing.md` first.** That conversation is where most
of cm's bugs live and it is the hardest thing here to observe, because a wrong result looks like a
clean pass. Four traps from it, worth knowing even if you read nothing else:

- **The control for a multiplexer bug is another multiplexer**, not a bare terminal. Comparing against
  bare kitty said "not a cm bug" about a bug that reproduced against zmx on the first try.
- **`cm read --raw` is not the byte stream.** It re-serializes the terminal model, so a session whose
  log really did contain OSC sequences showed none. Use `--raw --follow` redirected to a file, or log
  `%q` at the hop in question.
- **Client count changes behavior**: 0, one interactive, one read-only, and many are four different
  cases. Zero clients and read-only followers are where the failure is a *hang* rather than an
  artifact.
- **Drive the pty directly** with `printf 'A\033[6nB'` rather than running vim or another real
  program, which confounds the test and can stop exercising it silently.

## Go conventions

- `new("string")` / `new(5)` rather than a `ptr()` helper or a temp variable plus `&`.
- Match the surrounding code's comment density and naming. This codebase comments *why*, at length,
  especially where a line looks wrong without its reason. Keep that.
- Plain ASCII everywhere: prose, code, comments, commit messages. No em dashes, arrows, curly quotes,
  or decorative unicode.

## Comments and docs

The convention here is heavier than most Go code, on purpose: nearly every non-obvious line names the
incident that produced it. When you fix something, put the symptom in the comment, not just the
mechanism -- the next reader is someone seeing the symptom again.

Update the docs when behavior changes. `docs/` is a set of decision records, not a manual: each file
says what was chosen, what was measured, and what was rejected. Adding to one is cheap; leaving a false
claim in one is expensive, because the next person trusts it.

When a measurement decides something, record the number. "Any cm invocation costs about 23ms" and
"Connect adds 10.9 MB" are both load-bearing facts that would otherwise get re-derived or guessed.

## Git

- Branches: `pr/chancez/<change-name>`.
- Small logical commits, one concern each. Every commit builds and passes tests, so history bisects.
- Commit periodically to save progress even when the work is unfinished; squash fixups into the right
  commit once the iteration stops.
- Commit messages explain why, and say what was measured. No pronouns, no "we". Do not use the user's
  name.
- **Never `git checkout -- <file>`** on a file with uncommitted work, and never rebuild a file from
  `git show HEAD:path` or a backup to isolate a change. Both silently delete everything else in that
  file. This has been broken five times, twice while mutation-testing, where the undo and the loss are
  the same command. For mutation testing use `cp file /tmp/x.bak` and restore from that. Before any
  `git checkout` of a path, run `git diff --stat <path>` first.

## Encoding what you learn

Three places, by kind:

- **`cm doctor`** for anything a user could hit whose symptom points somewhere other than the cause.
  The bar: it must correspond to something that actually went wrong. A check for a hypothetical is
  noise, and noise teaches people to ignore diagnostics. Thresholds get calibrated against a real
  install, not picked.
- **`docs/`** for a decision and its alternatives.
- **`docs/ideas.md`** for something worth doing later, with what it would cost.

## Things that have bitten, worth knowing up front

- **Unix socket paths are capped at 104 bytes** on darwin, and the failure is a bare `EINVAL`.
  `t.TempDir()` embeds the test name and blows past it; the e2e harness uses `os.MkdirTemp("", "cme2e")`
  for exactly this reason. `paths.MaxSocketPathLen` holds the limit.
- **`ECONNREFUSED` from a socket does not mean nothing is listening.** A unix listener stops accepting
  once its queue fills, and says so with the same error a socket nobody serves gives: measured at
  185160 refusals out of 302124 dials against a listener that was accepting throughout. Three places
  assumed otherwise, and the costly one marked a busy shim's session dead on server startup, stranding
  a live shell. **Only `ENOENT` is conclusive**, and only on both platforms, which is the part that
  bit twice: darwin reports a plain file as `ENOTSOCK` while Linux reports `ECONNREFUSED`, and a full
  queue is `ECONNREFUSED` on darwin but `EAGAIN` on Linux. Separating a busy listener from a dead one
  has to be behavioral, since the live one resumes answering within about 11ms while a stale socket
  never does; `socketRefusalGrace` is that wait. Linux also queues 4097 connections against darwin's
  128, so a test that fills a backlog needs a bound above both or it silently proves nothing there.
- **A leaked shim holds a pty**, macOS caps them at 511 system-wide, and exhaustion surfaces as
  `device not configured` in whatever test runs next. Always stop sessions before the server.
- **`cp` over a running binary gets later invocations SIGKILLed on macOS** -- the cached code signature
  is invalidated. It presents as `zsh: killed cm ls` with nothing in any log. `mise run install`
  renames instead, which is why it is a task rather than a `cp`.
- **`os.File.Fd()` is not refcounted** the way `Read`/`Write` are, so an ioctl on a descriptor that
  `Close` is racing is a real race even though the I/O calls are safe.
- **Deleting the runtime dir under a running server does not stop it.** It keeps listening on an inode
  nothing can name, and every later command starts a second server.
- **Two sequence-number spaces exist** and mixing them corrupts output: the shim's numbering and the
  server's post-rewrite numbering differ in length. See `docs/architecture.md`.
- **`os.MkdirAll(dir, 0700)` over an existing directory leaves its mode alone**, so pointing
  `CM_RUNTIME_DIR` at a shared path can yield world-readable session logs. Hence `loose-dir-perms`.
