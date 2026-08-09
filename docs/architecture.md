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
upstream API break is one package to fix.

The server holds terminal state behind an interface with an injected constructor, so fanout,
reconnect, and ownership logic are testable without the emulator. The server's own tests use a
nil constructor for exactly that.

cgo is required, and that is a deliberate reversal. There used to be a `!cgo` stub for this package
and a no-cgo Linux image to exercise it, so that `CGO_ENABLED=0 go test ./...` proved cgo had not
leaked into another package and a build without the emulator degraded rather than broke.

The degraded mode was not worth its cost. `cm read`, `cm history`, and screen restore on reattach are
most of what cm is for, and all three need the emulator, so what the stub produced was a build whose
central commands returned empty *successfully*. That failed silently twice in ways that took real time
to attribute: once when `cm run` printed nothing, and once when a test's readiness check could never be
satisfied because it was waiting on rendered output that would never come. Both looked like bugs in cm.

The containment claim is still true and still worth keeping; it is simply no longer checked by building
without cgo.

A `CGO_ENABLED=0` build fails on purpose, and `internal/vt/requires_cgo.go` exists only to say why. Without
it the error is "build constraints exclude all Go files in internal/vt", which describes the symptom rather
than the cause and sends the reader looking for a build tag that is not there.

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

## Tags

`cm tag NAME key=value` labels a session, and `cm list --tag key=value` filters on it. Tags are also set
at creation with `--tag` on `attach` and `run`, and they show in `cm list`, `cm info`, and the JSON output.

They exist because a name groups a session one way and often cannot group it at all. A per-window session
is named from a server counter, so it is called `s17` and `cm list --prefix` has nothing to match on. Even
a deliberately named session belongs to several groupings at once -- a project, a worktree, the fan-out
that created it -- while its name says one thing. And a name cannot change, since the store keys on it and
the shim socket derives from it, so a session that turns out to be something else keeps a misleading name;
a tag can be corrected. That makes tags a cheap partial answer to the rename gap below.

Four decisions are worth recording, because each had a defensible alternative.

**Key/value, not bare labels.** A bare set of strings would cover filtering, which is the immediate use.
It would not cover a program remembering something about itself, which is what the custom-resumption idea
in `ideas.md` needs -- `claude --resume <id>` requires somewhere to put the id, and that file already
concludes the store has nowhere for it and that borrowing `Command` or `Env` would be wrong. One mechanism
that serves both is better than adding a second one later. A key with an empty value is legal, so `--tag
review` still reads as a plain label.

**A JSON column, not a side table.** This is the opposite conclusion from the same shape of data
elsewhere: the `env` column's migration justifies JSON on it never being queried by key, and tags *are*
queried by key, which is normally the argument for a table. It is still wrong at this size. Session counts
are in the tens, every caller that filters already holds the whole list, and expiry and `doctor` list
unfiltered anyway, so a join and a cascade delete would make a linear scan over twenty rows asymptotically
better and practically slower. Filtering happens in Go for the same reason. Revisit if sessions ever number
in the thousands.

**Persisted, unlike a report.** A report describes a running program, so restoring one would claim
something needs input long after it finished. A tag describes the session, so it survives a server restart
and is carried across a session being recreated. That inheritance sits *above* the persistence gate in
`inheritForRestore`, which matters: the old record is deleted whether or not it had a saved log, so gating
tags on persistence would drop them silently on an install with persistence off, and on any session whose
shell exited and was attached to again. Recorded tags merge with what the caller asks for, caller winning
per key, so retagging one thing does not discard the rest.

**The character set is a security boundary, not a style rule.** Keys and values allow letters, digits,
`-`, `_`, `.`, and `/`, up to 63 bytes each, and 63 is what a DNS label allows so it is a limit users have
met before. The restriction excludes escape sequences, and that is the point: a tag is supplied by whoever
creates a session and printed straight to the terminal of whoever runs `cm list`, so an unfiltered value
could retitle or repaint their window. Validation is server-side as well as in the CLI, because the socket
is the trust boundary and the CLI is one client of many. The same set keeps a tag unquoted in a shell and
unambiguous in a selector, since neither `=` nor `,` is in it.

Repeating `--tag` narrows rather than widens: two terms select the sessions that have both. There is
deliberately no negation or set membership. A full selector grammar is familiar from elsewhere and is more
syntax than filtering tens of sessions justifies, and it can be added later without changing what already
works. A malformed selector is an error rather than a silent match of everything, which is the dangerous
default for `cm kill --tag`.

**cm never interprets a key.** No tag changes how a session is treated. Config keyed off a tag would be
fine, since the person writing the config chose the key, but cm inferring meaning from one is the same
mistake as scraping a screen to work out what is running.

### Which commands take a selector

`--tag` selects on `list`, `kill`, `wait`, `read`, `history`, and `info`. It is not uniform, and the
omissions are decisions rather than gaps.

`send` does not take one: broadcasting keystrokes to N shells is a footgun, and `--wait` cannot mean
anything sane across N sessions. `attach` and `run` already use `--tag` to mean "label what you create",
and one flag must not also mean "select" on the same command. `get-env` and `report` describe one session
by nature.

Three rules are shared by every command that accepts both a name and a selector, and they live in one
place rather than being restated per command. A name and a selector together is refused, since there is no
reading of `cm read foo --tag bar` that is not a confusion about which applies. A selector matching nothing
is an error, because "no sessions matched" and "acted on all of them" must never be indistinguishable --
`cm kill --tag run=typo` exiting 0 having killed nothing looks exactly like a successful teardown. And
neither a name nor a selector is an error rather than a default to everything.

Selectors are expanded client-side into names, which is the same choice `cm kill --all` already made: the
server keeps one meaning per request, so a kill is always "kill these names" and a wait is always "wait for
this session". Expanding server-side would mean the same expansion in every handler, each a place where an
empty match could silently become everything.

**`kill --tag` is the safe form of `--all`.** It names exactly what matched, so a script tearing down its
own fan-out cannot reach sessions someone else is using. Unlike `--all`, an empty match is an error:
`--all` on an empty server is a satisfied request, while an empty selector is usually a typo.

**`wait --tag` requires every session by default, and waits concurrently.** A partial success has to be a
failure, or `cm wait --tag ... && collect` would collect from sessions still working; `--any` returns on
the first instead. The waits run in parallel because a sequential collector takes the sum of their
durations and throws away the parallelism the sessions already have -- the same trap the `cm` skill
documents for `cm send --wait`. One connection carries all of them, since ttrpc multiplexes calls behind
its own send lock and the server subscribes per session. Measured: five sessions each sleeping three
seconds complete in 3.02s rather than 15s, and `--any` over a group whose other session never exits
returns in 9ms rather than running out a 20s timeout.

**Output from several sessions is headed with `=== name ===`**, matching what `skills/cm/SKILL.md` already
tells an agent to write by hand around a fan-out's results. Headed whenever a selector chose the session,
including a single match, since the caller did not know which session it would be; a named session prints
bare, so piping one session's output is unchanged. `cm info --field` stays bare either way, because a field
is what a script reads.

Two combinations are refused because the output would be broken rather than merely ugly. `read --follow`
is an endless stream, so N of them interleave with no way to tell them apart and no header can mark a
stream that never ends. `history --format=html` produces a whole document per session, and several
concatenated is not a document at all. `cm info --json` returns an array for a selector and an object for a
named session, so an existing `cm info NAME --json | jq .cwd` keeps working while a selector composes with
`.[]`.

## Reading output back by command

`cm read --since-commands N` returns everything from where the last N commands began, and
`--last-output` returns only what the most recent one printed. Before this, a caller reading a session
guessed with `--lines` or planted a marker in the command, even though cm brackets every command with
OSC 133 and already had the answer.

**Commands, not sequence numbers, at the CLI.** The obvious design is "output since sequence N", and it
is worse. There are two sequence-number spaces here and mixing them corrupts output, so a number a caller
holds onto is a hazard; worse, a position from the wrong space reads from the wrong place *silently*
rather than failing. A command count also matches how a person and a script think about a session.
`ReadRequest` carries the resolved position, so the wire keeps the precision and nothing asks a user for
one.

**Two anchors, because there are two questions.** `--since-commands` anchors at the shell's `133;A`
prompt marker, so each block opens with the prompt and the command line the shell echoed. That is what
makes reading several commands useful at all: consecutive outputs run together with nothing between them,
so a caller cannot tell where one ended, which is the exact problem this feature exists to remove. The
delimiter comes from the shell rather than from cm, so no separator format has to be invented or parsed.
`--last-output` anchors at `133;C` instead, giving a parser just the program's output with none of the
shell's text. Well defined for one command only, which is why it is a separate flag rather than a mode of
the other.

**Boundaries are recorded from the rewritten bytes.** `internal/osc.BoundaryTracker` is separate from
`CommandTracker` for one reason, and it is the sharpest trap in this feature. `CommandTracker` is fed the
shell's output *before* `RewritePromptRedraw`, which is right for reading markers as the shell sent them.
The log numbers bytes *after* the rewrite, and the rewrite appends nine bytes to a prompt marker carrying
no `redraw` parameter. A position taken from the pre-rewrite stream therefore drifts from the log by nine
bytes per prompt, silently, and the drift grows over a session's life. Same hazard as the two
sequence-number spaces above, reached from a new direction.

**Rendered from a slice of the log, not from the session's model.** The session's terminal model holds the
*current* screen, which attached clients are looking at, so replaying historical bytes into it would
corrupt their view -- and older output may have scrolled off it while still being in the log. So a read
takes a slice of the log and replays it into a throwaway terminal. `seqlog.Snapshot` exists for this and
is `Subscribe`'s non-blocking counterpart: following blocks for output that has not arrived, and a bounded
read must not, or a quiet session hangs it.

**Three failures are reported rather than papered over.** A session whose shell has no OSC 133 has no
boundaries, so the error says so and points at `cm doctor`; returning empty output would look like a
command that printed nothing, which sends someone to debug their program instead of their shell
configuration. Asking for more commands than are known says how many there are. An ended session has no
boundaries at all, since they live in memory with it, so that is an error rather than a silent fallback to
a line count -- answering a different question than the one asked is how a caller comes to trust output
that does not mean what it thinks.

`--lines` cannot be combined with either, and the two cannot be combined with each other: they are
different bounds on the same read, and "the last 3 commands but only 50 lines" does not say which wins.

History is bounded at 64 blocks per session, since a long-lived shell runs commands indefinitely. The
bound is deliberately looser than the output buffer's, so a boundary usually outlives the bytes it points
at: a boundary whose output has been trimmed still says the command existed, which beats claiming it never
ran. A read that begins before the retained bytes logs that its view is short.

## Sending keys, and sending signals

`cm send` writes bytes to the pty, exactly as typing would. That is the right primitive and the reason
both of the following exist: input that did not go through the pty would never reach a program's line
discipline, so a shell would not see it as typing at all.

`cm send --key ctrl-c` names a keystroke instead of spelling out its bytes. Before it, a caller had to
produce the byte itself, and every natural spelling was sent as literal text -- measured in a sandbox,
`C-c`, `ctrl-c`, `^C`, and `\003` all landed on the command line, leaving the session holding
"C-cctrl-c^C\003" while the build a script believed it had interrupted kept running. Nothing errored.
So an unrecognized multi-character key name is now refused rather than typed, which is the whole point:
a single character is still sent literally, but "ctrlc" is a mistake and says so.

`ControlCode` is shared with the detach-key parser rather than duplicated. They describe the same
keystroke, and a user who configures a detach key and then sends that key by name would otherwise be
naming one thing two ways with only one of them right.

The key encodings are the default terminal forms, not the kitty-protocol or modifyOtherKeys variants.
Those are what a terminal sends *to* cm when someone presses a modified key; a program inside the session
has negotiated nothing with whoever is calling `cm send`.

`cm signal` exposes the shim's Signal RPC, which existed for `cm kill` but had no server RPC or command
reaching it. It is deliberately not a duplicate of sending ctrl-c, and which one is right depends on what
you are stopping. A control character travels through the pty, so the line discipline decides what it
means: a program that put its terminal in raw mode reads 0x03 as an ordinary byte and never sees a
signal, and a shell at a prompt with no job has nothing to interrupt. A signal is delivered regardless.
Verified against a job with SIGINT trapped, where `--key ctrl-c` could not stop it and `cm signal term`
did.

**The signal goes to the pty's foreground process group, not the shell's own group.** Building this found
the shim signalling the wrong one. Its comment claimed the group covered "a foreground job and its
children", and it does not: a shell with job control puts each job in a *new* group and hands that group
the terminal, so the shell's group holds only the shell. Measured under `/bin/sh` with `sleep 300`, the
shell sat in group 20144 and the sleep in 20242, and a SIGTERM to the shell's group reported success while
leaving the sleep running. That is the worst shape available -- the caller is told the job was signalled.
It now asks the pty via `TIOCGPGRP`, which is exactly what the line discipline consults for a keypress,
so `cm signal` and the key mean the same thing about *which* processes they reach. The ioctl goes through
`withPty` rather than a bare `Fd()`, since `Fd()` is not refcounted the way `Read` and `Write` are.

`--process-only` signals the shell alone, for the rare case where that is the target.

`cm kill` shares this machinery rather than having its own. The shim's Shutdown handler calls the same
`session.Signal`, so the foreground-group behavior above applies to teardown too: SIGHUP reaches a running
job, not only the shell.

That default is a request, and `cm kill --signal` exists because a request can be declined. A job that
ignores SIGHUP survives it while `cm kill` reports the session killed, leaving a process holding a pty --
the resource macOS caps at 511 system-wide, whose exhaustion surfaces as "device not configured" somewhere
unrelated. Measured against a binary built before the foreground-group change, which leaks the same job,
so this is long-standing rather than new.

**A signal that does not work is reported rather than leaked.** The shim enumerates the process group
before signalling, waits 250ms, and reports which pids are still alive. `cm kill` warns on stderr with the
pids and names the fix, the JSON output carries them in their own field, and the shim logs it -- which is
what `cm doctor` already surfaces, since its log check scans shim logs for exactly this kind of thing. No
new check was needed.

The shim is the only place this can be detected. It still holds the pty and knows the process group; the
server deletes the session record immediately afterwards, so a stray process can no longer be attributed to
cm at all. Doctor cannot find it later either, and deliberately so: `Diagnose` is scoped to cm's runtime
directory and database because scanning the process table for anything that looks like a shim can be fooled
and could kill something that is not cm's.

It reports rather than escalates. A job trapping SIGHUP to finish writing a file is doing something
legitimate, and a shim that killed it anyway to tidy up would break that. The caller decides.

`--force` was already the escalation, and that was its undocumented half: it means SIGKILL *and* forget a
record whose shim cannot be reached. Splitting those into two flags was the alternative considered.
`--signal` made it unnecessary, since it expresses the escalation directly and leaves `--force` meaning
one thing -- be maximally forceful. Keeping SIGKILL in `--force` also protects existing callers: the kitty
watcher passes it on window close and would otherwise start sending SIGHUP.

The shim's `ShutdownRequest` gained an optional signal field, which that frozen surface permits. Zero means
"not specified", so an older shim -- and a new server routinely talks to one, since a shim outlives the
binary that spawned it -- honors `force` alone. The degradation is one-way and recorded in the proto. A session that has
ended is an error rather than a silent success, since a signal needs a process to receive it; a session
that ends between the lookup and the delivery is not, because the caller wanted it stopped and it is.

## Waiting on output

`cm wait --match TEXT` blocks until the text appears in a session's output, and is the only wait that needs
nothing from what is running. Every state cm can wait for comes from OSC 133 or from a program calling
`cm report`, so a session running something with neither could previously only be polled -- and the `cm`
skill documented that polling loop as the fallback, which is exactly the sampling a server-side wait exists
to avoid.

The comparison worth being precise about is not "a state wait fails there". Without OSC 133 cm sees no
command running, so `--until idle` is satisfied *immediately* and truthfully reports a session it knows
nothing about as idle. That is worse than failing: it answers before the work starts, so a caller reads the
previous turn's output believing it is the new one. Measured, and pinned by a test, because the case for
`--match` rests on it.

**Its own loop, not a branch in awaitState.** The two wake on different things. `awaitState` wakes on
metadata changes -- title, cwd, whether a command is running -- and re-reads the session's current values.
Output is not part of that, so a session can print for an hour with no metadata changing, and a match folded
into that loop would only be evaluated when something unrelated happened. The match loop subscribes to the
output log instead.

**The matcher is its own type, and it handles two non-obvious bugs.** A match can straddle a chunk
boundary, since a pty read is bounded by the kernel buffer and "DONE" arrives as "DO" then "NE" often
enough to matter; the matcher keeps `len(pattern)-1` bytes of tail for that. And escape sequences sit
between the characters, so a coloured `DO\x1b[0mNE` matches nothing byte-wise while a person plainly sees
DONE; one stateful `ansi.Stripper` handles that, including an escape split across chunks. A chunk that is
entirely escape sequences must not clear the tail, or a repainting program breaks a match spanning its
repaint.

It is shared by the two callers that exist -- a bare `Wait` and a `Send` -- through `matchOn`, which takes
the subscription rather than opening one, because *when* to subscribe is the part that differs and must not
be decided in one place.

An earlier version of this section claimed the matcher was a seam for a future `cm watch`. That was wrong
and is corrected here rather than quietly dropped, because it is the kind of claim that gets built on:
`watch` would stream *state* changes, which come from the metadata subscription, and "has this text
appeared" is a different question. The matcher would only serve `watch` if it grew an output-matching
mode.

Rendered by default, with `--match-raw` as the modifier rather than a `--match-raw` flag name, following how
`--raw` already works on `read`, `history`, and `send`. Raw changes whether a pattern can match at all
rather than merely how output looks, which is why it is a deliberate modifier and not a default.

**Only output arriving after the call counts.** The same rule that keeps a wait for idle from being
satisfied by the idle a session started in; subscribing from the log's current end is what implements it.
Text printed earlier is `cm read`'s job.

The consequence is an ordering requirement, and it caught a documented example in this repo before the
example was corrected: `cm send` followed by `cm wait --match` is a race, because a fast command prints and
finishes before the wait subscribes and the wait then blocks on output it already missed. A caller has to
start the wait first and send afterwards. That is precisely the window `cm send --wait` closes server-side by
arming the wait before writing the input, which is why it exists -- and it only helps a session reporting
OSC 133, so the two are complementary rather than alternatives.

Refused rather than resolved: `--match` with `--until`, since "idle and also matching" and "idle or
matching" are both plausible readings; `--match-raw` alone; and a match against an ended session, which
will produce no further output.

A plain substring, not a pattern. Substring covers the case that motivated this, and a regex on a stream
raises anchoring questions worth deciding separately.

**`send --match` and `run --match` exist for the ordering, not for convenience.** The subscription is armed
before the input is written, so a command that prints and finishes faster than a second request could arrive
is still caught. A caller composing `send` with a separate `wait --match` cannot close that window from
outside, which is why the flag lives on the send request rather than only on Wait. It matters most on the
reuse path of `cm run`: waiting for the shell to be idle needs OSC 133, so a `/bin/sh` session could only be
bounded by a timeout, and the `cm` skill documented that workaround. Measured -- the state form took its whole
3s timeout, the match form returned in 14ms.

**The shell's echo is skipped, and that is not an optimisation.** Writing to a pty makes the shell echo the
line back, and the echo contains the command, so a pattern naming anything in the command matched the echo
rather than the output: `send 'sh -c "sleep 2; echo UNIQUEWORD"' --match UNIQUEWORD` resolved in 11ms while
the real output arrived 2s later. That is the same class of wrong answer as a wait for idle satisfied by the
idle a session was already in -- a result handed back before the work happened -- and it is the match-wait
counterpart of the `afterInput` qualifier a state wait uses.

The skip is a budget in *text* bytes rather than the echo matched as a string, because a terminal does not
echo verbatim: it wraps, and a shell with line editing redraws the line as it goes. A byte count is robust to
both. The cost is that it over-shoots by whatever the terminal added or removed -- a stripped carriage return
already makes the text one byte shorter than what was written -- so a few bytes of real output can be
consumed with it. Acceptable because the pattern a caller waits for arrives in the output that follows, not
within a few bytes of the prompt, and pinned by a test so a change to the accounting is visible.

`--match` with `--follow` is refused. Follow stops when its wait resolves, and a match resolving mid-command
would cut the stream off partway through output the caller was watching, which reads as truncation rather
than as the flag working.

## Timeouts

`--timeout` bounds `cm wait`, `cm send --wait`, `cm run`, and `cm read --follow`. It reached the last of
those late, which is the one that mattered: a follow streamed until the session ended, so following a
program that never exits waited forever, and the `cm` skill had to tell callers to always pass a timeout
because a missing bound was the default failure.

**A deadline does not mean the same thing on every command, and that is deliberate.** `cm wait` and `cm run`
exit non-zero: each was asked for a result and could not produce one, so a timeout is a failure to report.
`cm read --follow` exits zero: it was asked to print output until told to stop, and a deadline is being told
to stop, so it has already delivered everything the session produced. Failing there would make a caller
discard output it successfully received.

The cost of that split is worth stating: exit status alone cannot distinguish "the session ended" from "my
timeout expired". That is the right trade for bounding a follow so it cannot hang, and `cm wait` is what
answers the question when the distinction matters.

The follower's zero status does not come from where it looks like it comes from. `client.Attach` already
treats a cancelled or expired context as a deliberate detach and returns no error at all, so the deadline
arm in `followSession` is not what produces it -- verified by disabling that arm, which changed nothing. The
arm stays so the behavior holds if that path changes, and the test guards the contract rather than the
implementation.

`--timeout` on `cm read` is refused without `--follow`. Every other form returns as soon as the server
answers, so a timeout would bound something that cannot hang, and accepting it quietly would confirm a
caller's belief that it was protected against a hang that was never possible.

Zero means no bound, which needs a helper rather than an inline `context.WithTimeout`: that call with a zero
duration produces an already-expired deadline, so passing an unset flag straight through would make every
command fail instantly. One `withTimeout` makes that impossible to get wrong per call site, and one
`addTimeoutFlag` keeps the help text from drifting -- it had already drifted into three different phrasings
across three commands.

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
