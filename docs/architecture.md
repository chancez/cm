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
session bookkeeping, and terminal state in one place. It also means remote access, if it is ever built,
is a gateway concern rather than a matter of exposing every shim.

## Sequence numbers on both hops

`internal/seqlog` is used by both the shim and the server, because both need the same thing:
a bounded byte log where a subscriber names a position and is told whether the bytes it asked
for still exist.

The shim's log covers the gap while no server is subscribed. Without it, a server restart
would either wedge the shell on a full pty buffer or silently drop output. The server's log
covers the gap for clients, so one attaching mid-session is not shown a blank screen.

Numbers count bytes rather than writes, so a resume point stays meaningful even if a chunk is
split differently on a second pass. `seqlog.NewAt` exists so an adopted session's log continues
from where the previous server left off rather than from zero, which would make every existing
client position look like the distant future.

The two hops do *not* share one numbering, and assuming they do has caused two separate bugs.
Output is rewritten on the way through, which changes its length, so the server's log counts
different bytes than the shim's. Both numbers therefore have to be carried: see "Adoption needs
both resume points" below.

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

### A client retries forever, and why the bound had to go

Reconnecting is unbounded: a client that has once reached the server keeps trying until it is
cancelled. Cancellation is `ctx`, which the closing window delivers, so nothing waits against the
user's wishes.

There used to be a 30s budget, and it killed live sessions. The bug was not the length but where the
clock started: the deadline was armed on a client's *first* failure and never rearmed after a
successful reconnect, so it bounded the client's whole lifetime rather than any one outage. A window
open for hours had already spent it on earlier restarts, and the next restart ended it.

Measured from the logs of the incident. One `cm server stop` and restart: three sessions died and
twenty survived. The three were the only clients that retried inside the ~180ms before the new server
bound its socket, so they saw a dial failure where the others saw a live server, and all three had
first reconnected hours earlier (08:31 and 10:18 against a 14:43 restart) with no budget left. Every
survivor was on a first or recent reconnect. Two of the three shims recorded `shell_exited=false`,
which is the tell: the shell was still alive and its terminal was discarded anyway.

Unbounded is also correct independent of that bug, and follows from the layering. The shim owns the pty
and the shell keeps running with no server at all, so a server that is slow to come back is a reason to
wait rather than a reason to throw away a terminal someone is using. The asymmetry to keep: a *first*
dial that fails is still a hard error, because there is no session on screen to preserve and no reason
to think one is coming, which is what makes a typo in a socket path report itself instead of hanging.

Two consequences of never giving up:

- **Backoff.** After `reconnectQuietPeriod` the retry interval goes from 100ms to 1s, so a server that
  is gone for good is not dialled ten times a second for the life of the window.
- **Logging on a delay.** A reconnect used to be logged every time, which with twenty sessions meant
  twenty lines per restart saying nothing had gone wrong, and that noise is what hid the real outages.
  Nothing is logged for the first 3 seconds, which covers an ordinary 450ms restart entirely. Past that
  the outage is reported, repeated every 30s while it lasts, and its recovery is noted -- but only if
  the outage itself was reported, so a quiet restart stays quiet on both sides. Silence matters more
  than it used to: the client holds the terminal while it waits, and with no eventual error to explain
  it, an unexplained freeze is indistinguishable from a hang.

### A restart does not drop output, measured

Worth recording because the question keeps coming back, and because two bugs that *looked* like
restart data loss were bookkeeping errors instead.

Measured against a session emitting numbered lines at roughly 10 MB/s, followed to a file and
checked for holes in the numbering. A restart takes about 450ms. One restart: no discontinuity at
the restart. Three back-to-back restarts: no discontinuity at any of them. The control run, with
*zero* restarts, showed one hole at the same position as the other two runs, which is what
identified it: a follower's initial subscribe asks for the current end and the shell keeps writing
while the subscription is set up, so the hole is the subscribe, not the restart. Without that
control the first run's hole reads as a restart gap and sends you fixing the wrong thing.

Nothing needs to be handed off, because the shim already holds what a handoff would carry. It owns
the pty and buffers 4 MiB regardless of whether a server is subscribed, the new server resubscribes
from a recorded position, and the client keeps its terminal and resumes from its own. Passing client
connections to the new server, or deferring hooks until a client reconnects, were both considered
and rejected: they add authoritative cross-restart state to a design whose central property is that
anything the server holds is rediscoverable, and neither addresses a loss that is occurring.

A gap is still possible in one case, and it is a buffer-size question rather than a handoff one: a
session sustaining more than roughly 9 MB/s through the whole restart window overflows the shim's
4 MiB. The test above was near that edge and still lossless. `yes` in a loop reaches it; an
interactive shell or a coding agent does not. The lever is `shim.DefaultLogBytes`, at a fixed memory
cost per session.

What did go wrong three times was bookkeeping. The bytes were retained and the client asked for them
by the wrong number: see "Adoption needs both resume points" below, and `docs/restore.md` for the
reservation window that made a query go unanswered.

The third was an ordering bug in shutdown itself, and it is the reason a restart's resume points are
written before its socket closes. `Serve` used to call the transport's `Shutdown` before
`Manager.Close`. ttrpc closes its listeners as the *first* step of `Shutdown` and only then waits for
connections to go idle, so the socket stopped accepting while every resume point was still unwritten.
`cm server restart` decides the old server is gone by dialing that socket, so it launched the
replacement inside that window, and the new server's `Reconcile` read positions the old server had not
written yet.

The symptom is why it survived so long: no error anywhere, just every adopted session coming back
through the "output gap detected" repaint instead of resuming. That reads as the gap detection doing
its job. It was caught in a live restart where sessions were adopted with `from_seq=0` while the store
held positions in the millions, 26ms after the shutdown line.

Two things about the test are worth keeping. Polling the socket cannot catch this: the window is
sub-millisecond, so a dialing loop passes with the bug present, which is worse than no test. The
regression test wraps the listener and reads the store inside `Close`, which is the exact instant a
replacement server could observe. And it was confirmed by reverting the fix, where it fails every run
with the same `(0, 0)` the live incident produced.

## State: sqlite for metadata, files for bytes

Session metadata lives in sqlite (`internal/store`): the registry, cwd, title,
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

## Ownership was removed, and why a server flag cannot express per-window lifetime

There used to be an `--own` flag, setting `Open.own`. An owning client's connection dropping
*without* an explicit detach ended the session; an explicit detach left it running. The claim
was that this distinction is what a terminal emulator cannot determine from outside, so the
server should draw it and per-window sessions would clean themselves up with no counter file
and no `lsof`.

The claim was backwards, which is the part worth keeping. The emulator is the *only* thing that
can tell a closed window from a quit, because it knows whether it is shutting down. The server
sees one dropped connection either way. So the case ownership was built for is exactly the case
it gets wrong: quitting kitty disconnects every client at once, and `--own` would destroy every
session rather than leave them to be reattached, which is the entire point of running windows
inside a multiplexer.

The kitty integration this was built for therefore never used it. It reaps from a watcher, which
fires on every way a window can close and can check `on_quit` and the window map to distinguish
a deliberate close from teardown. Any integration needs that watcher regardless, and once it has
one, `cm kill` from the watcher is strictly better: the decision is made where the information
is. Nothing else ever asked for ownership -- `cm run` and `cm attach --no-attach` both set the
field to false explicitly, since a client that disconnects by design would have destroyed its
own session.

The database column was vestigial in a second way. Ownership lived on the attachment, so a
session adopted after a server restart had none until its client reattached and said so again,
which made the stored flag a write-only record of a past request. Nothing ever read it back.

What its removal simplified, recorded because each part looks arbitrary alone:

- The `sawDetach` flag. A dropped connection surfaces as a receive error *and* as a cancelled
  request context, racing each other, so a detach could not be inferred from how the stream
  ended and had to be recorded when the message arrived. Nothing consults the difference now.
- The detach acknowledgement handshake. See below.

A session outlives its client unconditionally, and `cm kill` is the only thing that ends one.

### The detach acknowledgement, and why clients stopped waiting

A client sending Detach used to wait for the server to confirm. ttrpc sends are asynchronous, so
returning immediately tore the connection down with the message possibly still queued: the server
then saw a client that had vanished without detaching and destroyed the owned session. Pressing
ctrl-\ in a per-window session killed the work in it, the opposite of what detaching means. Only a
real client on a real terminal found it, because every unit test drives the service through a fake
stream whose `Send` completes synchronously.

Ownership was the only consumer. With no session dying from a lost Detach, a discarded message
costs nothing, so every client sets `no_ack` and none waits. What that leaves is the server's log
recording a detach it never heard about as a client that died, which is a cosmetic difference in a
diagnostic rather than something a user loses work to.

`Detach.no_ack` is kept rather than removed, and reads as vestigial without this. It is what keeps
a warning out of the log for a client whose connection is closing as its Detach goes out --
measured at about 40% of runs, and warning about intended behavior made `cm doctor` report a
healthy installation as having a problem.

The server-to-client `Detached` message stays, and is now only ever the server's own initiative.
`cm detach` needs it: without it an evicted client reads the clean close as the server going away
and reconnects within a second, silently undoing the detach.

## Nested sessions work

zmx treats the presence of its session environment variable as a request to *switch* the
parent terminal's session, so an `attach` from inside a session hijacks the window it was run
from. An upstream PR to fix it was withdrawn, with the conclusion that nested sessions are
unsupported.

cm never treats `CM_SESSION` as a *target*. Intent comes from the request, so attaching from inside a
session creates a nested session. This matters more than it sounds for per-window sessions, where every
manual attach is nested by construction.

### Who a report belongs to

Nesting breaks an assumption the rest of the server makes: that everything in a session's output stream
is a statement by that session's shell. A nested client's stdout *is* the parent's pty, so the child's
OSC 7, OSC 2, OSC 133, and cm's own OSC 25453 all travel through the parent's stream, indistinguishable
from the parent shell's own bytes.

The parent used to record all four against itself. `cm list` showed the child's directory and title
beside the parent, the values reached the store and so survived a restart, and `cm wait outer --until
blocked` was satisfied by a report made inside the child. Measured with lsof: neither shell had actually
changed directory, so every one of those readings was false.

Filtering the bytes is not possible, and not necessary. The parent's shell is blocked inside `cm attach`
for the whole nesting, so it reports nothing, and every report on its pty in that window is provably the
child's. So the client tells the server which session it is running inside, via `Open.inside_session`
from `CM_SESSION`, and the parent suspends attribution until it detaches. `cm list` shows the parent as
`(hosting inner)`, which is also what explains why its directory is standing still.

Two details are load-bearing. The pass-through itself is deliberately left alone: the parent's terminal
title is correct precisely *because* the child's OSC 2 reaches it untouched, so this changes bookkeeping
and not bytes. And suspending is not enough on its own, because the parent's terminal model really does
absorb the child's values; the baselines a change is measured against have to keep moving while the
published values stay put, or the child's last directory is published as the parent's the moment the
nesting ends. That is the case `TestParentKeepsItsOwnValuesAfterNestingEnds` exists for, and a freeze
without the re-baseline passes every other test.

The client requires both `CM_SESSION` and a terminal on stdout before claiming to be nested. The
variable alone is inherited by everything a session's shell starts, so `cm attach x > file` would freeze
a parent that was still reporting honestly. A false positive is the expensive direction: it silences a
session's own reporting for as long as the attachment lasts.

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

### Adoption needs both resume points, not one

There are two of them because there are two numbering spaces, and adoption is where conflating them
did real damage. `last_seq` counts the *shim's* bytes, since that is what a resubscribe asks the shim
for and the shim knows nothing about the rewrite. `client_seq` counts what clients actually received.

A single number was stored and used for both. The adopting server started its client log at
`last_seq`, while a reconnecting client asked to resume from a position it had counted in
post-rewrite bytes. The client's number was therefore *ahead* of the new log's end, and
`seqlog.Subscribe` clamps a position past the end to the end, so the difference was skipped without a
word. Measured at nine bytes per prompt marker, the width of the `;redraw=0` that
`RewritePromptRedraw` appends to a marker sent without one, which is the form real shells send. Three
commands is 27 bytes: comfortably enough to eat the front of an escape sequence, and the remainder
renders as literal text. It presented as a corrupted TUI that a `ctrl-l` repaint fixed, which points
at the client's rendering rather than at a lost prefix on the server's side.

Two consequences worth keeping. The pair is read in one place, `Session.resumePoints`, and in a fixed
order: `lastSeq` first, then the log's position. The pump appends to the log *before* taking `mu` to
advance `lastSeq`, so no lock available there closes the window between them, and this order is what
makes the window harmless. It can only leave the stored client position at or ahead of what the shim
position accounts for, so the next server resubscribes from slightly behind its log's start and
re-delivers the overlap. The other order loses bytes instead, which is the whole failure again.

And `client_seq` defaults to 0 on a database written before the column existed, which is
indistinguishable from a session that served nothing. Adoption falls back to `last_seq` in that case:
wrong by the rewrite drift for that one adoption, which self-corrects on the next restart, rather than
starting the log at zero and making every client position look like the distant future.

`seqlog.Subscribe` now also flags a clamped position as a gap. That does not repair the drift and is
not meant to; it converts "silently skip bytes" into "tell the reader its view is discontinuous". The
clamp itself is still legitimate for a log that was reset behind a subscriber, but the two cases are
indistinguishable from inside the log, and a spurious resynchronize is much cheaper than silent
corruption.

### What a client does with a gap

It repaints, by dropping its resume position and reconnecting. That turns the next attach into a fresh
one, which the server answers with a serialized screen, so the recovery reuses the mechanism that
already exists rather than adding a second one.

Continuing is the thing that cannot work. The escape sequences that established the current screen may
be part of what was lost, so every later chunk is interpreted against state that never existed. The
gapped chunk is therefore not written either: its bytes are in the snapshot the repaint replays, so
writing them first would paint them twice, once against the wrong state.

A follower is the exception, and gets the bytes as they arrive. `cm read --follow` streams to a pipe
and sets `NoRestore` precisely because a repaint would corrupt what it is writing, so for one of those
a gap is a fact to report rather than something to fix, and dropping the chunk would lose real output.
The condition is keyed on `NoRestore` rather than on whether `Output` is set: both say "not painting a
terminal", but `NoRestore` is the one that says a repaint is unwanted, and it is what the server
already reads.

## Upgrading a client, and why only a client can be upgraded

A restart replaces the server and nothing else, which left two thirds of an installation on whatever
build it started with. `cm clients upgrade` covers the clients. Nothing covers the shims, and nothing
can.

The asymmetry is the design rather than a limitation. Each of the three layers can be replaced exactly
as far as what it *owns* allows:

- A **server** owns bookkeeping, all of it rediscoverable, so it can exit and be replaced. That is what
  Reconcile is for.
- A **client** owns a terminal and a stream position. The terminal is the user's and survives the
  process; the position is recorded and resumable. So a client can be replaced with nothing lost.
- A **shim** owns the pty and the shell. Replacing one means ending the shell, so it cannot be upgraded
  at all: only a new session gets a new shim. `cm doctor` reports the spread instead, since knowing is
  the only available remedy.

A client upgrade reuses the reconnect path rather than adding one. The server sends the same `Detached`
event `cm detach` sends, with `upgrade` set, and closes the stream. The client re-execs itself and
attaches again with `resume_from_seq`, which is the identical sequence it performs on every server
restart. What the flag changes is only whether it comes back.

Two details carry the whole "seamless" claim, and both are about what *not* to do.

`syscall.Exec` rather than spawning a child, so the process id, the descriptors, and the terminal are
kept. A child would mean the original exits, its shell prints a prompt over the session, and the
terminal is restored and re-rawed: three visible artifacts for a feature whose entire purpose is
invisibility.

The terminal is deliberately not restored first. `TTY.Close` writes a full reset, which is right for a
client that is finishing and wrong for one being replaced: the reset clears the session off the screen.
So the exec happens before teardown, and teardown runs only if the exec *fails*, in which case the
process is still holding a raw terminal and has to put it back.

The argv is rebuilt from the flags cobra parsed, not by editing the original. Editing means guessing
which bare word is the session name, and `--dir /tmp` puts a bare word directly after a flag: the first
implementation took `/tmp` for the session name. Two things are then forced. The resolved session name
is always written out, because a client started with no name had one allocated by the server, and
re-execing without it would allocate a *second* session and orphan the first with the user's shell in
it. And only flags that were actually set are emitted, so a default that changed in the new build takes
effect instead of being pinned to the old one's value.

A client already running the server's build is skipped, so running the command twice does not make every
window repaint. A client that reported *no* version is asked anyway: the field exists because older
clients did not send one, so an unknown build is more likely to be stale than current, and the ambiguous
case resolves toward the action that fixes things.

What the response counts is what was *asked*, not what was upgraded. The server sends the request and
closes the stream, so whether a client returns is known only to the client, and a client too old to
understand the flag exits instead. Reporting "upgraded" would be claiming an outcome the server cannot
observe.

## Terminal queries: cm answers, or asks one client and relays

A program in a session can ask the terminal questions: what are you (`CSI c`), where is the cursor
(`CSI 6n`), what is your background colour (`OSC 11`), what is on the clipboard (`OSC 52`). These are
synchronous. The program writes the question and blocks in `read()`, usually with no timeout, because on a
real terminal an answer always comes. So an unanswered query is a hang rather than a missing feature.

A multiplexer has to decide who answers, and cm now answers **all** of them itself. Queries split into two
sets, and the split is by what can possibly know the answer rather than by who is attached:

- **Answerable** from a terminal model: device attributes, device status, cursor position, mode state,
  XTVERSION, DECRQSS, and the OSC 4 palette query. cm answers these from its emulator, always.
- **Terminal-only**, where the value lives in the window rather than in any model: the OSC colour queries,
  the clipboard read, XTGETTCAP, and the XTWINOPS pixel reports. cm asks one attached client, waits for its
  reply, and writes that reply to the pty itself.

The invariant is that **cm is the only writer of a reply to a pty**. A client is a source cm consults, never
an answerer in its own right. `internal/query` holds the classification, `internal/server/queryproxy.go` the
mechanism, and the two sets are asserted disjoint and cross-checked against the live emulator by
`TestQuerySetsAgreeWithEmulator`, which sweeps 24294 sequences rather than trusting a list.

### Why not let the attached terminal answer

That was the design until this change, and it is worth recording why it went, because it reads as the
obvious approach: the real terminal knows the real answers, so forward the query and let it reply.

It requires electing exactly one client to answer, and the election has to be right in four situations.
Each was a shipped bug:

- A **read-only follower** elected. Its input is dropped on the way back, so nothing answered and the
  querying program hung while `cm read --follow` was running.
- A **reserved but not yet attached** client elected. `Service.Attach` deliberately opens that window to
  resize the pty before snapshotting the screen, and the resize makes the shell redraw, so a query from a
  `TRAPWINCH` handler lands exactly there. Same hang.
- **Two attached clients**, both answering. Output fans out, so both terminals saw the query and both
  replied: measured against a real kitty, a single `CSI c` came back as `\x1b[?62;52;c\x1b[?62;52;c`. The
  program reads one, and the spare is printed by the shell's line editor.
- **Across a server restart**, cm answered a query from the adoption backlog and the reconnecting client
  answered the same query again from the log. That is what typed a git branch name into a prompt, via a
  title report.

One writer removes the possibility rather than managing it. There is no election, and a reply that matches
no request cm made is discarded, which is what makes the restart case safe: a client replaying a query out
of the log produces bytes nobody asked for, and they go nowhere.

Two rejected alternatives, both tried:

- **Stripping the query from client-bound output** so no client could answer. Reverted twice. Unconditional
  stripping silenced queries an attached terminal was going to answer. Conditional stripping was correct
  about who answers and wrong about byte counts: removing bytes makes the client log shorter than the shim's
  numbering accounts for, which inverts the resume-position ordering adoption depends on and clamped a
  reconnecting client into the middle of an escape sequence. See `docs/libghostty.md`.
- **Not answering at all**, which is what zmx does: it never wires libghostty's write-pty callback, so its
  emulator produces no replies and it forwards output verbatim. Measured: a detached zmx session does not
  answer `CSI 6n`, where cm and tmux both do. That avoids this whole bug family at the cost of hanging any
  program that queries the terminal in a detached session, which `cm run` makes routine.

### Ordering, and why a queue is needed

A program asks its questions down one pty and reads the answers in order. cm can answer some immediately
while others need a round trip to a client, so writing the fast answer first reorders the conversation and
the program takes the wrong answer for its question.

That is the recorded `wallfacer -h` failure, and it is worth following precisely because it looks like two
unrelated bugs. `wallfacer -h` sent `OSC 11` and blocked. A zsh prompt hook then sent `CSI 6n`, cm's
emulator answered it, and cm wrote the cursor report to the pty while wallfacer was mid-read. wallfacer
consumed the cursor report as though it were the background colour and exited. The terminal's real `OSC 11`
reply then arrived with nobody waiting, and the line editor printed `;rgb:2828/2c2c/3434`.

So a locally-answerable reply queues behind any outstanding proxied question, and the queue drains in order.
The queue is **per session, not per client**, because the order that must be preserved is the program's: it
asked down one pty and does not know or care which client each question went to.

A proxied question expires after 500ms, matching tmux's `INPUT_REQUEST_TIMEOUT`, and expiry releases
everything queued behind it. Without that bound, one query a terminal does not implement wedges every later
query on the session, which is much worse than the single unanswered query it replaces. Detach releases a
client's outstanding questions immediately rather than waiting, since nothing can answer them once it has
gone.

### What is deliberately not suppressed

Every attached terminal still *sees* a proxied query, because cm forwards session output verbatim and
suppressing it would mean editing the stream, which is the reverted approach above. Several terminals may
therefore each answer on their own. That is harmless here: cm asked exactly one client and recorded the
request, so exactly one reply matches and the others are discarded unread.

Mouse reports and focus events are also still forwarded from every client, unchanged. They describe one
window rather than the session, so each client sends its own, and restricting them would make a session
ignore the mouse in every window but one.

## Terminal state

`internal/vt` is the only package that imports "C". Everything else works with Go types, so an
upstream API break is one package to fix.

The server holds terminal state behind an interface with an injected constructor, so fanout and
reconnect logic are testable without the emulator. The server's own tests use a
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

**The model is fed after clients are woken, not before.** The pump appends to the log, which wakes
attached clients, and only then advances the terminal model. That order is load-bearing in the other
direction from most of this file: the model is a derived cache, used for screen restore and for
`cm read`, and no live client's output depends on it being current. Feeding it first put its cost in
front of every keystroke's response.

That was not theoretical. libghostty built in Debug took 14ms to process a reverse index with the
cursor on the top row, and `less` emits one per line when paging up, so a half page spent about 350ms
in the emulator before the first byte reached the terminal. Paging down emits plain lines and was
unaffected, so the visible symptom was that scrolling up lagged and scrolling down did not. The
emulator cost is fixed separately, in how libghostty is built (see `docs/libghostty.md`); this
ordering is what keeps a slow model from being a slow session regardless.

**Which means the model can lag the log, and a fresh attach has to account for it.** A fresh attach
serializes the screen and then streams from a position. The obvious position, the log's end, is now
wrong: the model may be several chunks behind, so the screen does not contain those bytes, and
streaming past them means no client ever shows them. The bytes are not delayed, they are lost, and
only on a fresh attach.

So `Session.modelSeq` records the log position the model's screen corresponds to, and attach streams
from there. It is paired with the model's contents under `termMu`, separate from `mu`, because a
caller that could see the screen updated but not the position, or the reverse, would either skip
output or replay it twice. Lock order is `mu` then `termMu`, and nothing takes `mu` while holding
`termMu`: `feedTerminal` drops `mu` before acquiring `termMu` for exactly that reason, and publishing
metadata happens after `termMu` is released.

Note that `modelSeq` lives in the log's numbering rather than the shim's, which is the two
sequence-number spaces again, reached from a third direction.

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

Title and directory are not a difference from zmx: it reads the same two sequences, tracking cwd from
OSC 7 since `85b045c` and replaying the title on attach, and it decodes the URI's host to avoid
chdir'ing into a remote path exactly as cm does. Independent arrivals at the same answer, which is
what the sequences being a standard is for.

The command state is the difference, and it is narrower than "zmx needs hooks". zmx reads OSC 133 too,
but only to rewrite `redraw=0` into prompt markers, the same fix cm applies; it does not derive a
running command from them, so saying what a session is doing means labelling it with `zmx set`. That
is a deliberately different design rather than a missing feature: a label is an explicit statement and
survives whatever the shell does, where a derived state costs nothing to maintain but only exists when
the shell emits the markers. cm pays for that with a session adopted by a new server reporting idle
until its next command, and gains not restricting the value: a label value is limited to
`[a-zA-Z0-9-_.]`, so a command line has to be mangled to fit, while cm reports it as sent.

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
