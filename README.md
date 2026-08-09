# cm

A terminal multiplexer that persists shell sessions, built on
[libghostty-vt](https://github.com/ghostty-org/ghostty) for terminal emulation.

Sessions survive detaching, the client exiting, and the server being upgraded or
restarted. It provides no windows, tabs, or splits: your terminal emulator already does
that better.

## Architecture

```
client  <--ttrpc-->  server  <--ttrpc-->  shim  <-->  pty  <-->  shell
 (tty)              (VT state,          (one per session,
                     scrollback)         holds the pty)
```

The shim exists so the server can be replaced without disturbing a running shell. It
owns the pty and an append-only sequenced log of output, and nothing else: no terminal
emulation, no session policy. Upgrading the binary does not upgrade running shims, which
is the point, so the shim protocol stays small and additive-only.

The server is the single entry point. It holds terminal state via libghostty-vt, so
reattaching restores your screen and scrollback, and multiple clients can share one
session.

Because the shim buffers output while the server is away, restarting the server is a
brief freeze rather than a lost session. Clients resume from their last sequence number
without redrawing.

## Commands

```
cm attach [session]      attach, creating the session if needed (ctrl-\ detaches)
cm list                  list sessions; --tag/--prefix to filter
cm info [session]        one session's details; --field for a single value
cm tag [session] k=v     label a session so it can be grouped and filtered
cm history [session]     print contents including scrollback; --format=plain|vt|html
cm send <session> <text> send input without attaching; --key for ctrl-c, arrows, ...
cm run -- <command>      run a command in a session and exit with its status
cm wait [session]        block until a session reaches a state
cm read [session]        print a session's recent output; --follow --timeout to stream
cm get-env [session]     print env vars from the session's latest client
cm logs [session]        print cm's diagnostic log
cm signal [session] <s>  signal a session's foreground job (int, term, ...)
cm kill <session>...     terminate sessions; --signal to escalate past SIGHUP
```

Session names complete dynamically once `cm completions zsh` is installed.

`attach` with no name asks the server to allocate one, and `--own` ends the session when the
client disconnects without detaching. Together those give a terminal emulator a session per
window that cleans itself up on close.

Tags group sessions that a name cannot, which includes every session the server named itself:

```
cm run --tag project=cm --tag role=build -- make      # label at creation
cm tag s17 project=cm                                 # or afterwards
cm list --tag project=cm                              # repeating --tag narrows
```

`--tag` then selects on `list`, `kill`, `wait`, `read`, `history`, and `info`, so a group
created together can be driven as one:

```
cm wait --tag run=abc --until idle    # concurrently, all of them; --any for the first
cm read --tag run=abc                 # each session's output under its own header
cm kill --tag run=abc                 # the safe form of --all: only what matched
```

A selector matching nothing is an error rather than a silent success, which is what makes
`cm kill --tag` safe to put in a teardown script.

A server starts automatically when needed, so there is normally no reason to run `cm server`.

## Status

Working and usable: persistent sessions, attach/detach, screen and scrollback restore on
reattach, multiple clients per session, resize, session survival across a server restart or
crash, history, send-without-attach, and cwd/title tracking forwarded to clients.

Also tracks the terminal-related environment variables of whichever client attached most
recently, so a long-running shell can refresh values like kitty's `KITTY_LISTEN_ON` that
otherwise go stale when the terminal restarts. See `docs/config.md`.

`list`, `info`, `kill`, and `get-env` accept `--json` for scripting; the shape is a documented
contract rather than whatever the wire format happens to be.

Sessions can also survive a reboot, opt-in per session or by name pattern: the content comes back
and a fresh shell starts in the recorded directory. See `docs/persistence.md`.

Sessions can be driven without attaching, which is what makes cm usable from a script or an agent:

```
cm send build 'make' --enter --wait idle   # send, then wait for it to finish
cm send build --key ctrl-c                 # a keystroke, not the characters
cm signal build int                        # a signal, when a keystroke cannot get through
cm read build --since-commands 1           # what the last command printed, prompt and all
cm read build --last-output                # just its output, for a parser
cm read build --lines 50                   # the recent tail, soft wrap rejoined
cm wait api --until exited                 # block until a session ends
cm run --env KEY=value -- ./task           # a command in its own session
```

`--since-commands N` reads back by command rather than by guessing a line count: cm brackets
every command with OSC 133, so it knows where each one began. Each block opens with the prompt
and the echoed command line, which is what lets you tell several commands apart.

A program inside a session can say what it is doing, which is how a long-running agent or build reports
something cm cannot see for itself:

```
cm report --state blocked --detail "needs approval"   # uses CM_SESSION
cm wait reviewer --until blocked                      # now reachable
```

Nothing about that is program-specific: cm never learns what is running, so anything that can invoke a
command on a state change can report. `cm info <session> --field busy` reports what cm derives on its own,
from OSC 133. See `docs/architecture.md` and `contrib/hooks/`.

See `docs/` for design notes and trade-offs:

- `docs/architecture.md` - why three layers, and what each owns
- `docs/restore.md` - how screen restore works and why each detail is there
- `docs/config.md` - the config file, and the session environment problem
- `docs/persistence.md` - reboot persistence: what can survive, and the decisions made
- `docs/rpc.md` - why ttrpc, measured against gRPC and Connect
- `docs/libghostty.md` - using libghostty-vt from Go, and its constraints
- `docs/concurrency.md` - the lifetime invariants, and how the races were found
- `docs/ideas.md` - things cm could grow, what each would cost, and what is deliberately not being done

## Building

Requires Go and, to build libghostty-vt from source, Zig 0.16.0. Both are pinned in
`mise.toml`.

```
mise install
mise run build
```

That leaves the binary in `bin/cm`. To put it on your PATH:

```
mise run install                      # into ~/.local/bin
PREFIX=/usr/local mise run install    # or anywhere else
mise run uninstall                    # removes it again
```

The install renames into place rather than copying over the existing file, which matters more than it
sounds. Copying onto a path whose binary has a running process gets every later invocation SIGKILLed
on macOS: `cp` writes into the existing inode and invalidates the kernel's cached code-signature pages
that the live process still maps. It presents as `zsh: killed  cm ls` with nothing in any log. A
rename replaces the directory entry instead, so the new binary is a new inode, the swap is atomic, and
an already-running server keeps working on the old one until it is restarted.

Tests run on the host with `mise run test`, and on Linux in Docker with `mise run test-linux`. The
Linux run matters because a macOS-only run never compiles the Linux paths, and `/bin/sh` there is dash
rather than bash, which has caught real bugs. It builds libghostty from source, so screen restore,
history, and adoption-with-scrollback are covered rather than skipped.

cgo is required. There was a second, no-cgo Linux image and a `!cgo` stub for `internal/vt`, on the
theory that cm should degrade rather than break without the emulator. Both were retired: `cm read`,
`cm history`, and screen restore are most of what cm does, and a build where they return empty
*successfully* is a worse outcome than one that does not build. It also cost real debugging time
twice, each time looking like a bug in cm rather than a missing emulator.

A `CGO_ENABLED=0` build fails deliberately, with an error naming the reason.

`mise run build-linux` checks that the Linux build compiles, using the same image.

`go test -short` skips the end-to-end tests, which spawn real processes and ptys.
