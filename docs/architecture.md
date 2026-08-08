# Architecture

```
client  <--ttrpc-->  server  <--ttrpc-->  shim  <-->  pty  <-->  shell
 (tty)              (VT state,          (one per session,
                     scrollback,         holds the pty,
                     policy)             no terminal logic)
```

## Why three layers

The shim exists so the server can be replaced without disturbing a running shell. It owns
the pty and an append-only sequenced log of output, and nothing else: no terminal emulation,
no scrollback, no session policy. Because it holds no state a new server cannot rediscover,
killing the server leaves every shell running.

One shim per session, not one for many. A crash or an upgrade then has a blast radius of one
session. zmx makes the same argument for its daemon-per-session choice.

The server is the single entry point. Clients never talk to a shim, which keeps fanout,
ownership, and terminal state in one place. It also means remote access, if it is ever built,
is a gateway concern rather than a matter of exposing every shim.

## Sequence numbers on both hops

`internal/seqlog` is used by both the shim and the server, because both need the same thing:
a bounded byte log where a subscriber names a position and is told whether the bytes it asked
for still exist.

The shim's log covers the gap while no server is subscribed. Without it, a server restart
would either wedge the shell on a full pty buffer or silently drop output. The server's log
covers the gap for clients, so one attaching mid-session is not shown a blank screen.

Numbers count bytes rather than writes, so a resume point stays meaningful even if a chunk is
split differently on a second pass. `seqlog.NewAt` exists because the server's log must
continue the shim's numbering; starting from zero would make a position mean different things
on each hop.

A subscriber whose position was already dropped is told so. That flag is not cosmetic:
replaying bytes across a hole cannot reconstruct state the missing bytes established, so the
receiver has to resynchronize rather than continue.

## Restart is a freeze, not a blink

A client does not tear down its terminal when the connection drops. It reconnects, resumes
from its last sequence number, and catches up; input typed during the gap is buffered and
flushed. The full snapshot-and-restore path runs only on a fresh attach.

Server shutdown deliberately leaves shims running. A session that is merely being released
records that fact, so the pump ending is not mistaken for the session ending: without that
distinction a clean shutdown marks every live session dead and the next server refuses to
adopt them.

## State: sqlite for metadata, files for bytes

Session metadata lives in sqlite (`internal/store`): the registry, ownership, cwd, title,
each session's shim socket, and the sequence number the server last consumed. Terminal output
does not. It is high-volume, written sequentially, and only read back in order, so putting it
in rows would place a SQL insert on the hot path.

The database is a durable record, not the authority on liveness. A live session owns fds and
goroutines that cannot live in a database, so the only real answer to "is this session alive"
is whether its shim answers. On startup the server loads records, probes each socket, and
adopts what responds.

A session is marked dead only on a definitive connection refusal, never on a timeout. A busy
shim that misses a probe is still holding a live shell, and discarding its record would orphan
it permanently. zmx learned the same lesson.

Names come from a monotonic counter and are never reused, even after deletion. Reuse would let
a client holding an old name silently reattach to an unrelated session.

## Ownership, and implicit sessions

A client may declare itself the owner of a session. If an owner's connection drops *without*
an explicit detach, the server ends the session; an explicit detach leaves it running.

That single distinction is what a terminal emulator cannot determine from outside, and it is
why the existing kitty setup needs a latched flag plus a window-map check to guess at it.
Combined with server-side name allocation, it gives per-window sessions that clean themselves
up, with no counter file and no `lsof`.

Getting this right required handling both exit paths. A dropped connection surfaces as a
receive error *and* as a cancelled request context, racing each other, so the detach is
recorded as a flag when the message arrives rather than inferred from how the stream ended.

## Nested sessions work

zmx treats the presence of its session environment variable as a request to *switch* the
parent terminal's session, so an `attach` from inside a session hijacks the window it was run
from. An upstream PR to fix it was withdrawn, with the conclusion that nested sessions are
unsupported.

cm only ever exports `CM_SESSION` into a session's shell and never reads it. Intent comes from
the request, so attaching from inside a session creates a nested session. This matters more
than it sounds for per-window sessions, where every manual attach is nested by construction.

## Restarting the server

Sessions survive a server exiting, because the shim owns the pty. `cm server stop` asks a server to
stop and leaves every session running; the next command starts a server that adopts them through
Reconcile. That is the upgrade path.

Adoption has to rebuild the terminal model, which is the part that is easy to miss. The model lives
in the server, so a new server starts with a blank screen, while the *resume point* recorded in the
store points at the end of what the previous server already consumed. Consuming from there alone
leaves the model empty, so `cm history` returned nothing and a reattaching client saw a blank screen
even though the shell was fine.

The bytes are not lost: the shim retains 4 MiB and reports its oldest retained sequence, so adoption
replays from there up to the resume point, then hands over to the session's pump. Two details matter
and both were bugs waiting to happen. The replay must stop exactly at the resume point, since
overlapping by even a few bytes duplicates a *fragment* of a line rather than a whole line, which
looks like a rendering fault. And it writes only to the terminal model, never to the client log,
because a resuming client has already seen that output and appending it would replay old output as
though it were new.

## Terminal state

`internal/vt` is the only package that imports "C". Everything else works with Go types, so an
upstream API break is one package to fix, and the other layers stay buildable without cgo,
which matters because the shim needs no terminal emulation.

The server holds terminal state behind an interface with an injected constructor, so fanout,
reconnect, and ownership logic are testable without the emulator, and a session still works
without one, minus screen restore.

That claim is checked rather than asserted. `internal/vt` has a `!cgo` stub, so
`CGO_ENABLED=0 go test ./...` fails if cgo leaks into another package, and `mise run test-linux`
runs the suite that way in Docker. The stub is a stub, not a fallback: it reports that the
emulator is unavailable, and the wiring in `cmd/cm` checks `vt.Available` and passes the manager a
nil constructor, which the manager already treats as "run without a terminal model".

The distinction matters more than it looks. A constructor that *errors* instead of being absent
fails at session creation, since that is where it is called, so a build without cgo could not
start a session at all rather than merely losing screen restore. What a no-cgo build loses is
screen restore on reattach and `cm history`; sessions, attach, detach, multi-client, and
persistence all work. The server logs the downgrade once at startup, because "my scrollback
vanished" is otherwise hard to attribute to a build flag.

Restore is a port of zmx's `serializeTerminalState`. Its details are all bug fixes; see
`docs/restore.md`.

## What a session reports about itself

A terminal emulator driving cm needs to know more than "this session exists". Three things are tracked
from what the shell says in its own output, and all three are published as events as well as being
readable with `cm info` and `cm list --json`.

Title comes from OSC 2, and working directory from OSC 7, decoded rather than passed through: OSC 7
sends a percent-encoded URI with a host, so a session that has ssh'd elsewhere reports a path that does
not exist locally, and acting on it would open the wrong place.

Whether a command is running comes from OSC 133, via `internal/osc.CommandTracker`. This is the one an
emulator cannot work out for itself: cm owns the pty, so kitty only ever sees `cm attach` running and
cannot tell a session sitting at a prompt from one in the middle of a build. It is what a close
confirmation needs.

Reading it from the output stream rather than asking the shell to maintain it is the difference from
zmx, which needs `preexec`/`precmd` hooks writing a label. Two consequences beyond having no shell
configuration to install. zmx restricts label values to `[a-zA-Z0-9-_.]`, so a command has to be
mangled to fit and arrives lossy, while cm reports the command line as sent. And it works in a session
whose shell has none of the user's dotfiles, since the signal comes from kitty's shell integration.

The markers are events rather than state, which shapes two decisions. The tracker is stateful and
handles sequences split across reads, because a pty read is bounded by the kernel buffer rather than by
anything the shell intends. And the result is deliberately *not* persisted: "a command is running" is
true of a process, not of a record, so a stored value would come back after a restart describing a
command that finished long ago and a confirmation built on it would fire forever. The cost is that a
session adopted by a new server reports idle until its next command.

## State a program reports about itself

`cm report --state busy|blocked|idle|clear` records what a program in a session is doing. It is visible in
`cm list` and `cm info`, forwarded on the metadata event, and waitable with `cm wait --until blocked`.

The reason it exists is that `blocked` cannot be derived. cm reads OSC 133 to know whether a command is
running, which is enough for a shell and not enough for anything interactive: the shell reports a command
as running whether it is computing or sitting at a prompt of its own. A coding agent is one long-running
command from the shell's point of view, from the moment it starts until it exits, so the derived state says
"busy" for its entire life while the agent moves between working, waiting for an answer, and done.

A report takes precedence over the derived state rather than merging with it, because a program describing
itself is better evidence than a marker its shell emitted.

Nothing about this is agent-specific, and that is the design decision worth defending. cm has no list of
known programs and no patterns matched against their output, so a program it has never heard of works
exactly as well as one it has. The alternative, which herdr implements as a fallback for agents lacking
hooks, is a TOML manifest of regexes per agent matched against the bottom of the screen -- versioned,
fetched from the network, and updated whenever an agent changes its UI. Their Claude manifest was updated
four days before this was written. Asking the program is a fixed cost; recognizing it is a treadmill.

`cm report` with no session name uses `CM_SESSION`, so a hook running inside a session needs no plumbing.
That is the one place cm reads the variable, and it does not weaken the rule about `attach` above: using it
as the default target of a report moves nothing and retargets nothing, and an explicit name overrides it.

Reports are deliberately not persisted, for the same reason busy state is not: they describe a running
program. A value restored after a server restart would claim something needs input when it finished long
ago, and anything waiting on that state would be released for no reason.

See `contrib/hooks/` for how to wire this to a program, including a Claude Code example.

## Waiting, and why the server does it

`cm wait` and `cm send --wait` block until a session is idle, busy, or exited. The server answers from
the session's own output rather than a client polling `cm list`, which costs one request and, more
importantly, cannot miss a transition that a sampling loop would.

Three details are load-bearing, and each is a bug that was hit rather than a precaution.

The subscription is registered *before* anything that could cause a change. `cm wait` subscribes before
its first check; `cm send --wait` subscribes before writing its input. Checking first and subscribing
second leaves a window where the transition happens in between, and the wait then blocks until its
timeout having missed exactly what it was waiting for.

A wait issued after sending input cannot be satisfied by the state the session was already in. A shell at
a prompt is idle, and it takes a few hundred milliseconds to report the command it was just given -- about
300ms for zsh, which is long enough to lose every time rather than occasionally. So `send --wait idle`
waits for a command to have started before idle counts, or it would return before the command existed and
the caller would read output from before its own input.

That start cannot be detected by watching for the session to *be* busy. The metadata subscription
coalesces to a depth of one, so a command like `true` starts and finishes between two reads and arrives
as a single event. The session instead counts commands the shell has reported starting, and a waiter
compares that count: it asks whether a command ran at all, which survives the collapse. Without it,
`cm send true --wait idle` timed out reporting "waiting for idle; it is idle".

`--wait` is part of the send request rather than a second call for the same reason. Two calls cannot be
ordered from outside, so a fast command finishes before the second one arrives.

## Known gaps

Kitty graphics and OSC 8 hyperlink targets are not part of a restored screen: libghostty's
formatter does not re-emit them. Both pass through live. zmx has the same limitation, and the
fix belongs upstream.

A pty cannot outlive a reboot. Persisting the output log would let a session's *contents* come
back, but the shell would be a new process. Content persistence and process persistence are
different guarantees and should not be blurred.

Busy state is unknown for a session adopted after a server restart, until its next command. It is
derived from a live stream and not stored, for the reason above.
