# cm

Terminal sessions that outlive the terminal.

Start a shell in a session, detach from it, close the window, restart your machine's terminal,
and come back to it later with your screen and scrollback intact. Sessions also survive cm
itself being upgraded or restarted, because the process holding your shell is not the process
you talk to.

cm deliberately provides no windows, tabs, or splits. Your terminal emulator already does
those, and staying out of its way is why cm is small.

Already using tmux or zmx? [docs/alternatives.md](docs/alternatives.md) compares them, including
where cm is the wrong choice.

## Install

Binaries are published for macOS and Linux on both arm64 and x86_64.

With [mise](https://mise.jdx.dev), which picks the right archive for your platform and keeps it
updated:

```sh
mise use -g github:chancez/cm      # or: mise use github:chancez/cm, per project
```

Or download the [latest release](https://github.com/chancez/cm/releases/latest) by hand. Each
release ships a `SHA256SUMS` covering every archive.

```sh
tar -xzf cm_*_darwin_arm64.tar.gz
install cm_*_darwin_arm64/cm ~/.local/bin/cm
```

To build from source instead, see [CONTRIBUTING.md](CONTRIBUTING.md).

## Getting started

```sh
cm attach work     # create the session, or reattach if it exists
ctrl-\             # detach, leaving the shell running
cm ls              # see what is running
cm attach work     # pick up where you left off
```

There is no server to start. Any command starts one if none is running.

`cm attach` is the only command you need for both halves of that, which is the point: it
creates a session or reattaches to an existing one, so the same command works from a script,
a keybinding, or a terminal emulator's startup file.

## Sessions

A session has a name you choose, or one cm allocates:

```sh
cm attach work                  # named
cm attach                       # cm picks a name and prints it
cm attach --no-attach build     # create it without attaching
cm attach notes --read-only     # watch without being able to type
cm attach work --detach-key ctrl-]
```

Reusing a name reuses the session, so a command that creates one is safe to re-run.

A session outlives the client attached to it, however that client goes away: detaching, closing
the window, or the process being killed all leave the shell running. Ending one is `cm kill`.
Attaching from inside a session creates a nested session rather than hijacking the window you
ran it from.

Detaching is `ctrl-\` by default. Set `detach_key` in the config file to change it, or
`--detach-key` for one attachment, which is what you want when something outside cm already
claims the key.

`cm detach` does the same thing without a keypress, which covers the two cases a key cannot.
The key reaches whichever client owns the real terminal, so from a nested attach it detaches
the parent; naming the session says which one you meant. And a script or an agent has no
keyboard at all.

```
cm detach          # let go of the session I am in
cm detach inner    # a specific one
cm detach --all    # every client the server has
```

## Upgrading

Installing a new build does not disturb anything that is running, which is the point of the
three layers, but it also means nothing picks the new build up on its own:

```sh
cm server restart        # replace the server; sessions keep running
cm client upgrade --all  # replace the clients; windows keep their screens
```

A client re-execs itself and resumes where it stopped, so a window shows the same screen a
moment later rather than repainting. Clients already on the server's build are skipped, so
running it twice does nothing.

Shims are the exception and stay where they are. A shim holds the pty, so replacing one ends the
shell in it, and only a new session gets a new shim. `cm doctor` reports how many builds the
running shims span; on a machine with a session per window that is routinely several, and the
only way to change it is to finish with a session.

## Tags

Tags group sessions that a name cannot, including every session cm named for itself:

```sh
cm run --tag project=cm --tag role=build -- make   # label at creation
cm tag s17 project=cm                              # or afterwards
cm ls --tag project=cm                             # repeating --tag narrows
```

`--tag` then selects on `list`, `info`, `read`, `history`, `wait`, `signal`, and `kill`, so a
group created together can be driven as one:

```sh
cm wait --tag run=abc --until idle   # all of them, concurrently; --any for the first
cm read --tag run=abc                # each session's output under its own header
cm kill --tag run=abc                # the safe form of --all: only what matched
```

A selector that matches nothing is an error rather than a silent success, which is what makes
`cm kill --tag` safe to put in a teardown script.

## Driving a session without attaching

This is what makes cm usable from a script, a CI job, or a coding agent. A session is a real
pty, so a program that checks whether it has a terminal behaves as it would interactively.

```sh
cm run -- make -j4                        # run, print output, exit with its status
cm run -d -- ./long-thing                 # return immediately, print the session name
cm send build 'make' --enter --wait idle   # send input, then wait for it to finish
cm send build --key ctrl-c                 # a keystroke, not the six characters
cm signal build int                        # a signal, when a keystroke cannot get through
cm read build --lines 50                   # the recent tail, soft-wrapped lines rejoined
cm read build --last-output                # just the last command's output, for a parser
cm read build --since-commands 1           # that output with its prompt and command line
cm wait api --until exited --timeout 5m    # block on a state instead of sleeping
cm history build                           # everything, scrollback included
```

`cm run` exits with the command's status, so it composes with `&&` and `||` like a local
command.

`cm wait` and `cm send --wait` watch the session's own output, so they cannot miss a transition
the way polling can. Pass `--timeout` to anything that can block: it turns a hang into an
answer.

Two things are worth knowing before you build on this. `--since-commands N` reads back by
command rather than by guessing a line count, because cm brackets every command with OSC 133
and knows where each one began. And `idle` and `busy` come from those same markers, so a shell
that does not emit them never reports idle and a wait for it never returns. `cm doctor` names
that case as `no-shell-integration`.

`list`, `info`, `kill`, `get-env`, and the waiting commands accept `--json`, whose shape is a
documented contract rather than whatever the wire format happens to be. See
[docs/config.md](docs/config.md#json-output).

For a fuller guide to orchestrating work this way, including running other agents in sessions,
see [skills/cm/SKILL.md](skills/cm/SKILL.md). Claude Code users can install it as a plugin
rather than copying the file:

```
/plugin marketplace add chancez/cm
/plugin install cm@cm
```

The skill teaches driving cm; it does not install the binary, so install cm itself first.

## Letting a program say what it is doing

cm can tell that a command is running. It cannot tell whether that command is computing or
sitting at a prompt of its own waiting for an answer, and only the program knows which. So a
program can say:

```sh
cm report --state blocked --detail "needs approval"   # uses $CM_SESSION
cm wait reviewer --until blocked                      # now reachable
```

From a shell, load the integration and use the function instead, which writes an escape
sequence directly and costs nothing where a cm invocation costs about 23ms:

```sh
eval "$(cm shell-init zsh)"          # bash likewise; fish: cm shell-init fish | source
cm_report blocked "waiting for approval"
```

Nothing here is program-specific. cm never learns what is running in a session and has no
patterns matched against a program's output, so anything that can run a command on a state
change can report, and a program cm has never heard of works exactly as well as one it has.
See [contrib/hooks/](contrib/hooks/) for how to wire it up, including a Claude Code example.

## Across a reboot

Opt in per session or by name pattern, and the *content* comes back: scrollback, the last
screen, and the working directory, with a fresh shell started in it.

```sh
cm attach work --persist
```

A pty is a kernel object and a shell is a process, so neither survives a reboot. Content
persistence and process persistence are different guarantees and cm does not blur them. See
[docs/persistence.md](docs/persistence.md).

## Configuration

Optional. cm works with no file, and every setting has a default that suits the common case.
`cm config` prints what is in effect and where each value came from.

Worth knowing about: `detach_key`, `scrollback` lines, the resize policy when several clients
share a session, and `[env]`, which fixes a problem specific to long-lived sessions. A shell
captures its terminal's environment once, at startup, so reattaching from a terminal that has
since restarted leaves it holding a dead `KITTY_LISTEN_ON` or `SSH_AUTH_SOCK`. `cm get-env` and
a prompt hook refresh them. See [docs/config.md](docs/config.md).

Session names complete dynamically, annotated with each session's state, once
`cm completions zsh` is installed.

## When something looks wrong

```sh
cm doctor            # problems, with --clean to fix what is fixable
cm status            # what the running server is doing
cm logs server -f    # the server's diagnostic log
cm logs shim work    # one session's
```

`cm doctor` is the first thing to reach for. Every check in it corresponds to something that
actually went wrong and was slow to diagnose because it failed silently rather than reporting
an error: a shim left holding a pty, a socket with nothing behind it, a client and server from
different builds, how many builds the running shims span, a runtime directory long enough to
break unix sockets.

`cm ls --json` reports each attached client's build and pid, which is the other half of the same
question: a version difference is legal here, since a session outlives its server on purpose, but
the effect of one is silent, so being able to see what is attached matters.

The diagnostic logs matter more than usual here, because cm swallows errors in anything
advisory so that a failed title update or metadata write cannot end a session. Everything
swallowed is logged.

## How it works

```
client  <--ttrpc-->  server  <--ttrpc-->  shim  <-->  pty  <-->  shell
 (tty)              (VT state,          (one per session,
                     scrollback)         holds the pty)
```

One shim per session owns the pty and an append-only sequenced log of output, and nothing
else. That is why the server can be replaced without disturbing a running shell: it holds no
state a new server cannot rediscover. Restarting the server is a brief freeze rather than a
lost session, because the shim keeps buffering output while it is away and clients resume from
their last sequence number without redrawing.

The server is the single entry point, and clients never talk to a shim. It holds terminal state
via [libghostty-vt](https://github.com/ghostty-org/ghostty), which is what makes reattaching
restore your screen and scrollback, and what lets several clients share one session.

## Docs

`docs/` is a set of decision records: what was chosen, what was measured, and what was
rejected.

- [alternatives.md](docs/alternatives.md) - tmux, zmx, and libghostty: what differs and why
- [architecture.md](docs/architecture.md) - why three layers, and what each owns
- [restore.md](docs/restore.md) - how screen restore works, and why each detail is there
- [config.md](docs/config.md) - the config file, and the session environment problem
- [persistence.md](docs/persistence.md) - reboot persistence, and what can survive
- [rpc.md](docs/rpc.md) - why ttrpc, measured against gRPC and ConnectRPC
- [libghostty.md](docs/libghostty.md) - using libghostty-vt from Go, and its constraints
- [concurrency.md](docs/concurrency.md) - the lifetime invariants, and how the races were found
- [testing.md](docs/testing.md) - testing terminal behavior so a wrong result does not look like a pass
- [ideas.md](docs/ideas.md) - what cm could grow, what each would cost, and what it will not do

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers building, testing, and the conventions. Note one rule
up front: never run a bare `cm` command against your own setup while developing, because it
talks to the server holding your real sessions.
