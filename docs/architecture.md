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

**The two numberings are distinct types, so mixing them is a compile error.** `internal/seq` names
them: `seq.Shim` for what the shim counted, `seq.Log` for what clients received after the rewrite.
`seqlog` is generic over the space, so a log is tied to one and `recent.Subscribe(lastSeq)` no longer
builds. Both were `uint64`, which is why conflating them compiled, and it cost three bugs, all silent:
adoption storing one number for both, `--since-commands` anchoring in the wrong space, and `modelSeq`
needing a comment to say which space it was in. The remaining conversions are the honest crossings and
each says why: the protobuf wire, the sqlite row, and one deliberate fallback in adoption for a
database written before `client_seq` existed.

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

## A session is an ID, and a name is a binding onto one

A session's identity is an ID: allocated when it is created, never reused, never chosen by a caller, never
changed. A name is a row in a separate table pointing at an ID. A session has zero names, one, or several,
and any name can be moved to a different session without the session noticing.

Before this, the name *was* the identity: the store's primary key, the variable part of the shim socket
path, and the value shells had already exported as `CM_SESSION`. Three limitations followed from that one
fact, and they turned out to be the same limitation. A session could not be renamed. It could not have a
second name. And a terminal window could not be pointed at a different session, because the only handle
the window had on its session was a name it could not move.

What the split buys, and each of these is now one write to the bindings table:

- Renaming is binding a new name and unbinding the old one. `cm bind`.
- Several names for one session, so an emulator's automatic name and a name a person chose coexist.
- `cm switch`, which points a terminal window at another session for as long as that client lives, and
  `cm rebind`, which moves the window's name too so a restored window follows. `cm rebind --replace` ends
  the session the name came off, which is safe to offer only because nothing else depends on that session
  once its last name has moved.

Nothing moves on disk when a name changes, which is what makes it cheap. `shim_socket` and `log_path` are
recorded in the row rather than derived, a property they were given so a socket layout change could not
orphan existing sessions, and it pays off again here: renaming touches no file, and the sessions carried
across the ID migration kept the paths they were created with.

**Attaching, creating, and reviving are one rule.** Resolve the name; if it resolves to nothing usable,
allocate an ID and point the name at it. `Manager.Open` used to branch on whether a record existed and
what state it was in.

**Reviving keeps the ID.** A session whose shell exited keeps its record so `cm list` can say why, and
attaching to it starts a fresh shell with its content replayed, under the same ID and the same names.

The first attempt allocated a *new* ID for the revived session, on the reasoning that an ID should name one
shell and never a second. That was too strong, and it cost two things. It forced attach-by-ID to refuse
outright on an exited session, since it could not return the ID it was asked for, which made an ID a handle
that stopped working the moment a shell exited and left a session with no names impossible to revive by
anything. And it leaked: a new ID means a new log path, so the old log was orphaned with its record
deleted, and expiry only removes a log through the record that names it.

What an ID actually has to promise is narrower: it is never handed to a session that is not the
continuation of the one it named. A revive satisfies that -- same record, same content from the same log, a
new shell -- and it is the same continuity attaching by name has always given. The hazard the stronger
reading was aimed at is ID *reuse across unrelated sessions*, and random IDs rule that out on their own.

**An `@id` reference never creates.** Reviving is not creating: it continues a record that is there. An ID
that names no record at all is stale, and inventing a session for it is how a client silently ends up
somewhere it did not ask to be, so that is an error. A *name* that resolves to nothing is different and
does create, which is what makes `cm attach work` idempotent for a terminal emulator restoring a window.

**IDs are random rather than counted.** Eight characters from a thirty-character alphabet with no vowels
and none of the glyph pairs that get misread, so 30^8 is 6.6e11: at ten thousand sessions over a machine's
lifetime the chance any two collide is about 1 in 13000, and a collision hits the primary key and is
retried. A counter would have been shorter and was rejected because it would live in the database. Losing
or replacing the state directory restarts it, and an ID recorded outside cm would then resolve to an
unrelated session; two servers with separate state directories would hand out the same low numbers as well.
That silent wrong-session resolution is the one failure an identity has to rule out, and a random ID that
outlives its database fails to resolve instead, which is the correct outcome. It is the same reason names
were never reused when names were identities.

**The sigil is a proof rather than a convention.** `@` is not in the set `ValidateSessionName` allows, so
an ID reference can never be mistaken for a name however anyone names a session. Without it, `cm attach 7`
would become ambiguous the moment somebody bound the name `7`, and would resolve differently depending on
which sessions happened to exist. `ValidateSessionID` is now the path traversal boundary that
`ValidateSessionName` used to be, since an ID is what becomes a filename.

**A name records what killing by it means, and this is not ownership coming back.** A borrowed name
releases and leaves the session running; every other name kills. The distinction is needed because the
caller doing the killing is usually a terminal emulator's window-close watcher, which fires for every
window and cannot know whether that window is where a session lives or borrowed it from elsewhere, while
whoever created the name does know. The difference from the `--own` flag below is what makes it safe: that
was consulted when a *connection dropped*, which the server cannot tell apart from a terminal quitting,
whereas this is only ever consulted on an explicit kill. The watcher still decides whether to kill at all,
so the decision stays where the information is.

**`CM_SESSION` is the ID with its sigil.** A name would be friendlier and would be wrong: the name a
session was created under can be pointed elsewhere while its shell runs, and every script in there that
captured the variable at startup would then be reading a different session. The client holds the ID too and
reconnects by it, since a reconnect is a return to one particular session rather than a fresh request for
whatever a name points at now.

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

### Recognizing the detach key, and why a lone escape is not held forever

The detach key is matched by the client, before input is forwarded, and matching it is harder than
comparing a byte. A terminal with the kitty keyboard protocol or xterm's modifyOtherKeys reports ctrl-\
as a CSI sequence rather than as 0x1C, so the client watches for three encodings: the control byte,
`ESC [ 92 ; 5 u`, and `ESC [ 27 ; 5 ; 92 ~`. zmx learned this from Claude Code, which enables
modifyOtherKeys on startup and made its detach key stop working entirely.

A sequence can also be split across two reads, so a partial one is withheld until the rest arrives
rather than forwarded to the shell. That holdback had no bound, and every encoding begins with ESC, so a
lone escape looked like a partial sequence and waited for a keystroke that might never come. **Pressing
escape delivered nothing at all until the next key was pressed.** That is the keypress that leaves insert
mode in zsh's vi mode, in vim, and in Claude Code, so the symptom was a mode indicator that did not
change, and the next key then being read in the mode the user thought they had left.

The bound is `escapeGrace`, 50ms, after which whatever is held is released as ordinary input. Two orders
of magnitude above the gap it has to cover, since a terminal writes a whole key sequence in one write and
the halves of a local split arrive microseconds apart, and below the point where a keypress feels late.
It is the same number vim and neovim use for `ttimeoutlen`. tmux's `escape-time` defaults to 500ms and is
the setting everyone turns down, which is the mistake worth not repeating: it is sized for a link far
worse than this has to survive, and it makes escape feel broken.

What it costs, stated plainly: a detach sequence whose halves arrive more than 50ms apart is no longer
recognized, so its escape reaches the program and the remainder is typed at it. That needs a link that
divides a 7-byte write across two frames 50ms apart.

All of it was checked in a real terminal against a real session, with the previous build as the control,
because the unit tests cannot see a keyboard:

- A lone escape now arrives on its own. The control, built from the commit before this one, still showed
  nothing a full second after the escape, and delivered `^[B` together the moment the next key was
  pressed.
- Ctrl-\ still detaches as the control byte, and `ESC [ 92 ; 5 u` written as one write still detaches,
  leaving the session running with no clients.
- Split with a 300ms pause, the cost above is what happens: `^[[92;5` reaches the program when the grace
  expires, the late `u` follows it, and nothing detaches.

Two alternatives were rejected:

- **Never hold a lone escape.** Zero latency, and it breaks the split case outright rather than only
  when a link stalls. It also cannot be reasoned about from the outside: whether ctrl-\ works would
  depend on how a terminal happened to divide a write.
- **Forward the escape immediately and keep matching across the boundary**, detaching retroactively when
  the rest arrives. This is strictly better on paper -- no latency, and the detach key always works --
  but it delivers a stray escape to the program on every split, and escape means "cancel" to a great
  deal of software. Losing a half-typed prompt in Claude Code because a detach split is a worse surprise
  than 50ms, and an invisible one. It remains the upgrade path if a real link is ever measured stalling
  past the grace, since it turns that failure into a stray escape rather than garbage on the command
  line.

The deadline is anchored to the first byte withheld rather than restarted by each read, so a stream that
keeps ending in a partial cannot postpone the release indefinitely, which would be the unbounded wait
again by another route.

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

A third consequence, added later: `lastSeq` counts what the pump *consumed*, not what the shim sent.

The pump holds back a chunk's tail when it ends inside an unfinished OSC 133 sequence, so every consumer
downstream sees a whole marker. It has to, because `RewritePromptRedraw` is stateless and silently skips a
marker it receives in pieces: the introducer went out unrewritten, nothing matched it afterwards, and the
client got `redraw=1`. A terminal that believes that clears the prompt lines on the next resize and waits
for a repaint that arrives in the pty's coordinates rather than the window's, so the prompt is cleared and
does not come back. Measured as every split strictly inside the marker, a 26-byte window for one carrying
parameters, and reproduced by writing the marker in two `printf`s.

The holdback sits *before* the graphics transform on purpose, so the held bytes are still the shim's and
`lastSeq` can simply not count them. A restarting server then resubscribes from before them and the shim
sends them again, which matches a log that never received them. Counting them would skip them on the next
server, which is this same hole from a new direction. Holding *after* the graphics transform would mean
mapping post-transform lengths back to shim positions, which is the mistake this whole section is about.

The holdback covers *any* short trailing sequence, not just a prompt marker, because the same gap was in
`noteQueries` and cost more there. A terminal-only query split by a read boundary was never recorded, and
since the stream is forwarded verbatim the client's terminal answered it anyway; `answerFromClient` then
discarded the reply, nothing being outstanding to match it, so the program that asked waits forever. Measured
across seven query shapes: OSC 10, OSC 11, OSC 52, CSI 14t, CSI 16t, XTGETTCAP, and a kitty graphics query.
OSC 11 is `wallfacer -h`, the recorded hang the proxy exists for.

Two scanners with the same bug is the argument for holding back once in the pump rather than making each
scanner stateful. `ansi.Tracker` is already the only escape-sequence state machine in cm, so
`ansi.PartialTailLen` asks it where the sequence ends and the pump trims there.

It scans forward rather than searching backwards for the last ESC. The backward version was written first, as
an OSC-specific helper, and was wrong: given a terminated sequence followed by the start of another it found
the terminated one, saw its terminator, and reported nothing pending, so the second was lost exactly as
before. Caught by sweeping the boundary through a stream with two prompts. A backward search is also wrong in
general, since an ESC appears inside a string control's payload as part of its ST terminator.

`maxHeldTail` is 256 bytes, and the bound is about kitty graphics rather than tidiness. A transmission is an
APC carrying a payload chunked at about 4 KiB, so a partial one is routinely larger than any query or prompt
marker; holding those would delay every image and buffer megabytes, and buy nothing, because the graphics
scanner already reassembles a transmission across chunks. Past the bound the tail passes through, which is
what happened before any holdback existed.

The scan costs about 1.3us per 1022-byte chunk, measured as 5240 to 6467 ns/op for the pump's per-chunk
scans on plain output, 4708 to 6245 with a prompt marker, and 4921 to 6934 with graphics. At 1 MB/s of
session output that is roughly 0.13% of a core, against 36us for a single reverse index in the emulator.

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

## Upgrading, and what a rollback cannot undo

`cm upgrade` is the whole of it: it restarts the server, waits for the clients to come back, and asks them
to re-exec. Shims stay on the build that spawned them until their sessions end, which is not a gap to close
but a consequence of what each layer owns; the section below states why, and the command reports the count
rather than hiding it.

Three details in that order are load-bearing.

*The server goes first.* A client re-execs and reattaches, so upgrading clients first brings each one back
on the old server and makes it reconnect again when that server restarts: two repaints per window instead of
one. Server first also means the build a client compares itself against is already the new one.

*Then it waits for the clients to reconnect,* and that wait is not padding. Restarting disconnects every
client, each reconnects on its own 100ms retry, and asking in that gap finds nobody attached. The first
version of the command did exactly that: it reported "no clients were attached" with a window plainly
attached, and the client came back on the *old* binary. Only a real client showed it, since a listing cannot
be missing a client that never existed.

*And it says one thing when it worked.* One line naming the build everything is on, because that is the
whole of a healthy run. A client that never reconnected is a window left on the old build, so that goes to
stderr as a warning, the way a kill reports processes that survived it: loud enough to notice and off the
stream a script reads. Kept shims are not mentioned at all, since a session predating the restart keeps its
shim on every run where any session exists, which made the most repeated line the least actionable one. The
count stays in `--json`, and `cm doctor` reports how many builds the running shims span, which is the form
worth reading.

The verb covers the server alone. A client is *asked* to come back and the server cannot observe whether it
did, so clients appear as a count rather than as something claimed to have happened.

The pieces remain separately usable: `cm server restart` for the server alone, and `cm clients upgrade` for
one window from a keybinding, which has no business restarting a server.

Two things bound how far an upgrade can skew. The shim protocol does not change, which is what lets a new
server adopt a shim an old one spawned, and matters most for the layer that cannot be replaced. The server
protocol may change, and a client and server are expected to be upgraded together: `cm doctor` reports the
skew, and `cm clients upgrade` is how the client half converges without closing a window.

**A shim is re-exec'd from the binary on disk, so an old server spawns new shims.** This is the skew that
is easy to miss, because it needs no upgrade command to happen: replacing the binary is enough, and from
that moment a still-running old server pairs itself with shims from the new build. Installing and carrying
on working, which is the normal way to try a build, is exactly this state.

So the argv the server passes a shim is a compatibility surface in both directions, and it has produced two
bugs. The shim validated `--session` as an ID, which rejects a dot, and an older server passes a name: every
session created after the install died before binding its socket, and the server waited out its full
ten-second readiness timeout for a socket that would never appear. Measured at 10.38s per attempt against a
kitty split, whose sessions are named `kitty.N`, and 0.36s once names were accepted; a session called `work`
worked throughout, which is what made it look intermittent. The shim also built `CM_SESSION` by adding the
ID sigil, turning a name into `@kitty.325`, a reference that resolves to nothing, so every cm command inside
such a session answered "no session given".

The rule both point at: anything added to that argv has to be optional, and its absence has to mean what
the older server meant. `--session-ref` is the worked example. The server states the reference it wants
exported rather than leaving the shim to derive one, because the two spellings overlap and cannot be told
apart by inspection: a name of only lowercase letters is also a syntactically valid ID.

Sessions created before an upgrade keep working, including their environment: a shell in one has already
exported what that build put in its environment, and nothing rewrites it afterwards. A change to what cm
exports therefore reaches new sessions only, which is a property to design around rather than a bug. The ID
change is the worked example. A session predating it exported `CM_SESSION` as a name, and that name still
resolves, because the migration turned every existing name into a binding; only sessions created afterwards
see the `@id` form.

**A schema change is one-way, so it is snapshotted first.** Migrations only move forward, and a database a
newer build migrated cannot be read by an older one. Two things make that survivable.

Opening refuses when the recorded version is ahead of what the build knows, and says both numbers. It used
to fail later and confusingly: `migrate` had nothing to apply, `Open` succeeded, and the first query failed
with a missing column, naming the column rather than the version skew that removed it.

**Only a server migrates.** A client opens the database with `OpenExisting`, which reads and refuses any
schema that is not the one this build knows. The reason is the skew above: while a new binary is installed
and an old server is still running, a client is the *newer* process, and `cm logs shim <name>` reading a
binding took the schema from 6 to 7 in 0.01s, after which every request that server served failed with
`SQL logic error: no such column: name`. Migrating is a decision about a file two other processes are using,
and the process that owns it is the one that should make it.

And a migration copies the database before it changes anything, to `cm.db.v<from>.bak`, which the refusal
above then names, because that snapshot is a database the older build can read. `VACUUM INTO` rather than
copying the file, since WAL mode means committed rows can still be in the -wal file and a copy of `cm.db`
alone would look complete while missing the newest sessions.

Three decisions about that snapshot are worth recording, because each has an obvious alternative.

*Taken in the migration rather than in an upgrade command.* A migration happens whenever a newer build
opens an older database, including the server that a bare `cm ls` starts automatically, so a command that
snapshotted first would be bypassed by the most common path.

*Not deleted when the migration succeeds.* That is when it becomes useful rather than when it stops being: a
rollback happens later or not at all. Nothing needs to guard a migration that *failed*, since each runs in
one transaction along with its `user_version` bump, which was measured against this driver: a deliberately
broken multi-statement migration left neither the new table nor the new version behind.

*Bounded by age rather than by anything else.* A snapshot's usefulness decays into a hazard, which is what
makes a week defensible instead of arbitrary. Every session created after it was taken is absent from it,
and a session absent from the database is one whose shim nothing can find again, so restoring a week-old
snapshot strands a week of shells. Past that point reinstalling the newer build is the only sane recovery,
and keeping the file only invites the other one. `database_backup_retention` sets it, `0` keeps them
forever, and the sweep runs on the same pass as the shim logs since both walk files against a retention
measured in days. The cost is tens of kilobytes: 28672 bytes for a snapshot of a one-session database,
against 61440 bytes for a real install's.

Restoring is still not free, and the refusal says so rather than presenting it as an undo: sessions have to
be stopped first, anything created since the snapshot is not in it, and any session missing from it is left
running with nothing able to find it.

That failure also exposed something older and more general: a server started in the background had its
stderr discarded, so *every* way it could fail to start read as `server did not become ready within 10s`.
The reason was always available and always thrown away. The spawned server's stderr now goes to a file in
the runtime directory and the timeout error quotes it, which turns "it did not come up" into whatever the
server actually said. A file rather than a pipe: a pipe needs a reader for as long as the server lives, and
the client that started it exits in seconds.

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
implementation took `/tmp` for the session name. Two things are then forced. A session reference is always
written out, because a client started without one had an identity allocated by the server, and re-execing
with nothing would allocate a *second* session and orphan the first with the user's shell in it. And only
flags that were actually set are emitted, so a default that changed in the new build takes effect instead
of being pinned to the old one's value.

The reference written is the one that was *typed*, unchanged, and an ID only when nothing was. That was
built the other way first -- always the ID, so that a name pointed elsewhere in the meantime could not send
the replacement to a session other than the one on screen -- and the trade was wrong.

A re-exec replaces the process image, so the new argv is what the kernel reports from then on: `ps` shows
it, and so does a terminal emulator that saves a session file from the *foreground process* rather than
from the command it launched. Writing the ID always therefore rewrote every window's recorded command into
one that attaches by identity, and an ID never creates a session. A window restored after its session had
gone then came back dead, where a name would have recreated it. Upgrading is casual and losing a session to
a reboot is ordinary, so those two meet often, while a name being rebound under a live window is rare. What
is given up is that such a window follows its name on the next upgrade rather than staying where it was,
which is defensible on its own terms: the name means the other session now, and following a binding is what
every other attach does.

### Replacing the session a name came off

`cm rebind --replace` ends it, and `rebind_replaces` makes that the default. Three rules, each about
something the request did not ask for.

*It waits for the window to leave first.* The clients were asked to move a moment earlier and are
reattaching, so ending the session before they have gone evicts them from it instead and a window exits
rather than moves. A window still attached after the wait is one this call did not move, since --all-clients
was not given, so its session is kept and the reason reported.

*The kill runs on its own context, not the request's.* In the ordinary case the caller is the shell inside
the session being ended, so this kills the process waiting for the reply. On the request's context that death
cancels the kill halfway and leaves the session running.

*The busy check is skipped when the caller is inside the session being replaced.* There it cannot mean
anything: `cm rebind` is itself a foreground command, so OSC 133 reports that session busy every time and
refusing on it would refuse always, while a backgrounded job does not set it at all. What guards that case
instead is that a foreground command would have prevented the user typing the command. From elsewhere the
check is real, and `--force` overrides it, as it does the refusal to end a session that still has another
name.

Ending a session whose shell had just exited also used to report failure. The shim signals the shell's
process group, and if the shell went in the window before the shim reaped it that call fails with ESRCH,
which was the one shape of "already gone" the server did not tolerate. Reached most often here, since the
session being ended was created moments earlier.

### A switch reattaches rather than re-execing

`cm switch` reuses the loop above, not the exec below it. The server sends the same `Detached` event with a
target on it, and the client goes back around its reconnect loop against that session instead of returning.

Built as an exec first, by reusing the upgrade path wholesale, and that was the wrong half to reuse. An
upgrade *has* to replace the process, because the point is to run a different binary. A switch runs the same
binary against a different session, which is what a reconnect already is, so the exec bought nothing and
cost three things:

- The argv changed, since a re-exec is what the kernel records from then on. `ps` stopped showing what the
  window was started with, and an emulator saving a session file from the foreground process recorded the
  new one.
- A bare switch's durability became an accident of that. Whether it survived a terminal restart depended on
  which argv the emulator saved, rather than on whether a name had been bound, which is the distinction the
  command is *for*.
- Rebuilding an argv is a job with edge cases, and it already had one: a flag whose value is a bare word.
  Switching had no need of any of it.

Reattaching in place keeps the terminal, the process id, the input goroutine, and the argv. What changes is
the session, which is the whole of the request. Two things are dropped on the way through, and both would be
wrong to carry: the resume position, because it counts bytes in a stream the client is leaving, and any
input typed just before the switch, because it was meant for the session being left and typing it into
another shell would be worse than losing it.

A switch overrides both, since attaching elsewhere is the entire request.

A client already running the server's build is skipped, so running the command twice does not make every
window repaint. A client that reported *no* version is asked anyway: the field exists because older
clients did not send one, so an unknown build is more likely to be stale than current, and the ambiguous
case resolves toward the action that fixes things.

What the response counts is what was *asked*, not what was upgraded. The server sends the request and
closes the stream, so whether a client returns is known only to the client, and a client too old to
understand the flag exits instead. Reporting "upgraded" would be claiming an outcome the server cannot
observe.

### Which client is being used, and why typing is the only signal

`cm clients upgrade --current` upgrades one window rather than every client of a session, which needs an
answer to "which client is someone using". Three ways to get one look plausible. Two are impossible and
the third is invasive and still wrong, so the mechanism is worth stating alongside the reason the
obvious approaches were not taken.

**Asking is impossible, because the pty is a broadcast medium.** A session's output fans out to every
attached client, so an escape sequence asking "which client are you" reaches all of them and every one
answers. That is not a new problem: it is exactly the duplicate-reply bug the query proxy exists to
prevent, where two attached terminals turned a single `CSI c` into `\x1b[?62;52;c\x1b[?62;52;c`. A
per-client question would have to go down the query channel, which already exists for this reason, but
that only moves the problem: cm would be asking a client it had already chosen.

**A command inside the session cannot see its own client.** Its stdout is the shim's pty rather than any
one terminal, and the client is not among its ancestors. This is the same asymmetry that forced a nested
attach to *declare* itself rather than be detected: a nested client's output arrives on the parent's pty
indistinguishable from the parent shell's own.

**Focus reporting would need cm to enable a mode nobody asked for.** cm only learns about focus when the
program inside the session enabled DECSET 1004, so a session sitting at a shell prompt reports nothing,
which is the common case. Enabling it for cm's own purposes means setting a mode on the user's terminal
that nothing requested, and `service.go` forwards client focus events to the pty unconditionally, so a
program that never asked would start receiving `ESC[I` and `ESC[O` as input. Even with that fixed, focus
does not answer the question: zero focused clients is ordinary once the window is behind a browser, and
several terminals report focus independently.

**Keystrokes have none of those problems, and are causal rather than inferred.** For the motivating case,
a `cm clients upgrade --current` typed at a prompt, the Enter that ran it travelled client to server to
pty to shell to `cm`, so the server saw it on one specific attach stream strictly before the RPC
arrived. The client that typed is the client that ran the command. `clientSize.lastInputAt` records it.

Recorded separately from `Session.leader`, which is the tempting shortcut. Leadership is a decision about
the pty's *size*, gated on `resize_policy` and refused to a follower; this is a record of who is being
used, wanted under every policy.

The two are related in one direction now: **leadership defers to it.** Under `leader`, an attaching client
used to claim sizing whenever nothing else held it, which is right for the first window on a session and
wrong after every client dropped at once. A server restart does exactly that, so each client reconnected and
the first one in took leadership and the session's size with it, on an order that says nothing about which
window anybody was using. It was observed as a session coming back at the second window's size.

So `registerClientSize` prefers the most recently used attached window over the arriving one. That became
possible only when `lastInputAt` started surviving a dropped stream: before, a returning client had never
typed as far as the server knew, so there was nothing to compare against.

What it does not promise is that no intermediate resize happens. Clients reconnect one at a time, and the
first one back is legitimately the most recently used window at that moment because it is the only one
attached, so it takes leadership and the window that was really in use takes it back on its own return.
Avoiding that flicker means waiting for reconnects to settle, which is a timer and a guess at how long. The
end state is what is guaranteed. Since `claimLeadership` returns early for three of the four policies,
reading the leader would have left anyone running `smallest` or `first-attach` with nothing marked, and
the failure would be silent, because a session with one client looks identical either way.

Three cases decline to answer rather than guessing: nothing typed yet, a bare reservation, and two
timestamps that tie. The reservation exclusion is the same distinction the query proxy had to learn,
where counting a not-yet-attached client as the answerer left a program's query answered by nobody. The
tie cannot arise from real keystrokes and means an injected clock, where resolving by map order put the
mark on pid 1000 and then 1001 across two identical calls. `--current` on a session with no identifiable
active client asks nobody and reports zero, rather than falling back to every client: a caller reached
for the flag precisely to spare the other windows.

## Losing a server, and who fixes it

A client that loses its server reconnects forever, because the shim holds the pty and the shell is still
running: waiting is right and giving up is not. What was missing was everything around that wait.

**The wait was invisible.** The client owns the terminal while it reconnects, so a frozen window is
indistinguishable from a hung program. Past `reconnectQuietPeriod`, which is the same three seconds the log
uses, the client paints one line on the bottom row saying what is happening and for how long. Nothing is
painted below that threshold, since a restart takes about 450ms and a flash of text on every upgrade would
be worse than silence.

That line overwrites a row the session owns, and cm's terminal model is the only thing that knows what was
there, so recovery drops the resume position and reattaches, which the server answers with a serialized
screen. It is the same move a detected output gap makes, and the same trade: output that scrolled past
during the outage is not replayed, which is the right choice once an outage has lasted this long.

**Nobody was starting a server.** Every cm command starts one when none is running, which is why the fix for
a frozen window was to open a new window and run any of them. The machinery existed and the client that had
noticed was the one process that could not reach it. It now does, throttled to one attempt every five
seconds, with the failure shown on the same line: a server that refuses to start is otherwise
indistinguishable from one that is slow, and one unknown config setting once stopped a replacement from ever
coming up.

**A stop has to stay stopped**, which is the one case where restarting is wrong. Restoring a database
snapshot and running a server in the foreground both require that nothing starts one behind your back. So a
server exiting on request leaves `server-stopped` in the runtime directory and a starting server removes it.

A file rather than a message on the wire, for three reasons: it reaches clients that attach after the stop,
it survives the process that wrote it, and it self-heals, since the next server to start clears it. It is
written after `Serve` returns rather than in the `Shutdown` handler, so a signal counts the same as the RPC,
and a process killed outright writes nothing, which is exactly right because nobody asked it to stop.

Honored for two minutes rather than forever. An upgrade that died between stopping the old server and
starting the new one would otherwise leave every window waiting for a server nobody will start, and that
failure is silent. Two minutes is longer than a snapshot restore takes, and that case has no clients
attached anyway, since the recipe stops the sessions first.

*Not a `cm doctor` check.* "Clients attached and no server running" looks like exactly what doctor is for,
and it cannot work: `cm doctor` connects to a server, which starts one, so running the check would create
the condition's cure and report nothing. That is deliberate for other reasons, and it makes this state
observable only from inside a client, which is where it is now handled.

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

**The 500ms is measured from when the client was last asked, which matters across a restart.** The record of an
outstanding question lives in the server's memory, and adoption resubscribes from where the old server stopped
rather than re-reading the bytes that carried the query, so a restart forgot the question and did not re-ask
it. The reply then matched nothing and was discarded as unsolicited, and the asking program got no answer.

The client is the only thing that still knows, because it was handed the bytes and wrote them to its terminal,
so it re-offers them in `Open` on reconnect and the server records them again. Measured: without a restart the
program reads `rgb:2828/2c2c/3434`, and before this it read nothing across one.

Re-recording them as *freshly asked* is a change to the budget rather than a repair, and it is the part worth
understanding. A restart takes seconds and the budget is 500ms, so preserving the record without resetting the
clock would change nothing: the sweep would abandon it immediately. That was measured too, and a reply arriving
2s late is discarded with no restart involved at all, which is `TestASlowAnswerIsDiscardedWithoutAnyRestart`.
The reply a terminal produced during the restart arrives within microseconds of the reconnect, so the reset
budget is not generous in practice.

What this does not do is let a client answer for itself. cm is still the only writer of a reply to the pty,
still matches a reply against a recorded question, and still asks one client at a time. What a client gains is
having bytes it sends treated as a reply rather than as typing, and it can already send typing, which reaches
the pty verbatim. A question re-offered that was in fact answered before the restart costs one expiry of queue
delay and then goes, which is the same outcome as a client that never answers; the client cannot tell the
difference, since which question a reply settles is decided server-side against the query's bytes and never
reported back.

### What is deliberately not suppressed

Every attached terminal still *sees* every query, both the proxied ones and the ones cm answers itself,
because cm forwards session output verbatim and suppressing it would mean editing the stream, which is the
reverted approach above. Terminals therefore answer questions cm never asked them, and those answers arrive
on the same input path as the one cm is waiting for.

Discarding them takes more than checking which client replied, and getting that wrong was the reported
`gh pr create --web` and `wallfacer sync` corruption: `^[[42;1R^[[42;1R` printed beside the prompt. Both
programs use termenv, which probes with `OSC 11` and sends `CSI 6n` immediately behind it as a sentinel,
because a terminal that ignores `OSC 11` still answers a cursor report and that is how termenv knows to stop
reading. cm proxies the `OSC 11` and answers the `CSI 6n` from its model, and the client answers both. So two
things have to hold, and each was a separate defect:

- **A reply must answer the question that was asked.** Matching on the client alone accepted the terminal's
  cursor report as the background colour, wrote it to the pty, released cm's own cursor report from behind
  it, and discarded the real colour reply as unsolicited. `query.AnswersQuery` holds that correspondence,
  next to the classifier because it is the same knowledge from the other direction.
- **A reply chunk must be split first.** A terminal answers several questions in one write, measured against
  a real kitty as the colour reply and the cursor report concatenated. Fixing only the matching left the
  doubling in place: the colour matched, and the cursor report rode to the pty inside the same blob.
  `input.SplitReplies` breaks the chunk up so each sequence is matched on its own.

Matching is conservative in the direction of *not* matching, and a mismatched reply does not consume the
request. An unrecognized reply costs the asking program only its 500ms expiry, which is what already happens
for every query cm cannot proxy, while a wrong match writes an answer to the wrong question. Because the
request survives, the real answer still lands if it arrives afterwards, which is the order termenv produces.

Mouse reports and focus events are also still forwarded from every client, unchanged. They describe one
window rather than the session, so each client sends its own, and restricting them would make a session
ignore the mouse in every window but one.

### The answers cm does not take from its model

Answering mode state from the emulator is right for almost every mode. Two are reported as **not
recognized**, whatever the model's real state is: left/right margin mode (DECLRMM, private mode 69) and
in-band size reports (private mode 2048). `DenyModes` in `internal/vt/denymodes.go` rewrites the replies,
and `deniedModes` there is the list.

This is a third category, cutting across the two above: answerable by a model, but the answer is only true
if something outside the model cooperates. The test for membership is whether the reply makes the program
rely on something the model does not control. The two members fail in opposite directions, which is why the
category is about the *dependency* rather than about forwarding.

**Mode 69** plus DECSLRM confine scrolling to a range of *columns*, which is how a program scrolls one side
of a vertical split without touching the other. libghostty implements it correctly, and that is the trap,
because the sequences that act on it are forwarded verbatim to a terminal that generally does not: kitty has
no mode 69 at all, logs "Unsupported screen mode", and drops the DECSLRM. The insert-line and delete-line
operations that follow then apply full width. The symptom was nvim scrolling **both** halves of a vertical
split.

**Mode 2048** is the reverse: nothing is forwarded, because a terminal that sets it promises to *originate*
something. It reports every resize as `CSI 48 ; rows ; cols ; ypixel ; xpixel t`, and a program given that
promise stops relying on SIGWINCH. cm answered "supported" and then never sent a report, since libghostty
emits them from `StreamHandler.resize` while cm resizes through `ghostty_terminal_resize`. The pty was
resized correctly and nvim was no longer listening to it. The symptom was nvim keeping half the window after
a kitty split closed, until something else forced a redraw.

Mode 2048 is now **denied and honored at the same time**, which reads like a contradiction and is the only
combination that works. cm still answers "not recognized", and it also sends the reports itself from its
resize path. The reason is that the denial does not stop the mode being set: nvim sends `CSI ? 2048 h` in the
same startup burst as 2026, 2027, and 2031, **without waiting for the reply** to the query it also sends.
Measured by relaying nvim's own output stream: one query, one set, no reset. So the reply is not what decides
whether a report is owed, and the model's mode state is. See "Honoring mode 2048" below.

Measured in one kitty window, same probe, same moment:

| host | reply to `CSI ? 69 $ p` | reply to `CSI ? 2048 $ p` |
| --- | --- | --- |
| bare kitty | `?69;0$y` (not recognized) | supported; kitty implements the mode |
| zmx 0.7.0 | `?69;0$y`, because zmx passes the terminal's reply through | `?2048;2$y`, kitty's own answer |
| cm, before this | `?69;2$y` (supported, reset) | `?2048;2$y` (supported, reset) |
| cm, now | `?69;0$y` (not recognized) | `?2048;0$y`, and cm sends the reports itself |
| tmux 3.5a | no reply observed | no reply observed; not in its DECRQM switch |

Two rows were re-measured later with a cleaner probe, and the corrections are worth keeping rather than
quietly fixing. zmx's mode 2048 answer is kitty's `;2` arriving through it, not "no reply": zmx answers
nothing itself, so the terminal's reply reaches the program unchanged. And tmux does implement DECRQM at
3.5a, contrary to the earlier note that it arrived in 3.6, with `{ 'p', "?$", INPUT_CSI_QUERY_PRIVATE }` in
its parse table and mode 2048 simply absent from the switch it dispatches to. The empty tmux column is what
a probe inside a tmux pane returned and is not explained; the source says it should answer `;0` for any mode
it does not know, so treat that cell as unresolved rather than as evidence.

Three details that make these harder to diagnose than they look:

- **Reset is as damaging as set.** nvim's `tui_handle_term_mode` sets `has_left_and_right_margin_mode` for
  set, permanently-set, *and* reset: the flag records that the mode can be *changed*. cm answered `;2` for
  both modes, so a fix that only suppressed `;1` would have changed nothing.
- **The damage outlives the query.** nvim probes once at startup and never re-asks, so every later scroll or
  resize takes the wrong path. The complaint is "all scrolling is broken now", or "it stopped resizing",
  neither of which sounds like a capability answer given once.
- **`cm read` looks correct throughout.** cm's model honours both modes, so the model's screen is right and
  only the attached terminal is wrong. Comparing `cm read` against the terminal is what separates the two.

`0` rather than `4` (permanently reset) because it is what the terminals themselves say: kitty answers `0`
for mode 69, and tmux answers `0` from the default arm of its DECRQM switch for every mode it does not know.

What denying mode 69 costs is nvim's terminal-side scroll optimization in a vertical split, where it repaints
instead: measured at 14114 bytes against 6696 for the same ten-line scroll. Denying 2048 costs nothing
measurable in itself, because SIGWINCH already carries the same news: with the mode denied, a shrink and a
grow emitted 4298 and 11202 bytes, against 4302 and 11206 for bare nvim in a plain pty.

That last number is where the reasoning went wrong, and it is worth naming because the measurement was
correct and the conclusion drawn from it was not. It shows only that a program *still using* SIGWINCH loses
nothing. It says nothing about a program that has stopped, which nvim has by the time it matters, and the
byte counts look reassuring precisely because the test program was one that never set the mode.

### Honoring mode 2048

Denying the mode was not enough, and the reason is that nvim never reads the answer. cm answers `;0`, nvim
sets the mode anyway, and then waits to be told about resizes that cm was not sending. The pty tracked four
consecutive resizes (14x99, 9x89, 15x109, 11x79) while nvim held 30x100 throughout; feeding it one report by
hand moved it immediately. So cm emits the reports itself, from `Session.reportSize` on the resize path.

An earlier version of this section argued the opposite, and recorded a reason that measurement refuted. It
said cm could not honor the mode because "cm forwards the probe to the attached client as well as answering
it, so kitty's own reports arrive on the client's input stream where `IsQueryReply` discards them as
unsolicited; emitting reports outbound would not fix that half." Both halves were then measured. Two DECRPM
replies really do exist and **cm's wins**: with a client attached, the program received `\x1b[?2048;0$y`
while the debug log recorded `\x1b[?2048;2$y` being discarded from the client. The inbound half was already
correct, so honoring the mode was only the outbound half. The claim was plausible, was never measured, and
cost the bug an extra diagnosis.

Where the three multiplexers sit, measured with the same probe in the same terminal, mode 2048 enabled by the
program and two resizes driven:

| host | answer to `CSI ? 2048 $ p` | reports delivered |
| --- | --- | --- |
| zmx 0.7.0 | `;2` (set), from kitty | all 3: `[48;30;100;390;700t`, `[48;24;90;312;630t`, `[48;30;100;390;700t` |
| cm, before this | `;0` (not recognized) | none |
| tmux 3.5a | no reply; not in its DECRQM switch | none, and none owed |

The three positions are genuinely different, and cm cannot hold either of the others:

- **zmx is a pipe.** Its emulator generates no replies and `handleInput` queues the leader client's bytes
  straight to the pty (`src/loop.zig`), so kitty answered and kitty reports. Mode 2048 works there as a
  consequence of forwarding everything.
- **tmux never implements the mode**, so nothing inside it enables one. It takes its size from `TIOCGWINSZ`
  and only queries the terminal for *pixel* dimensions. It also consumes a client's `CSI ... t` for its own
  bookkeeping and never forwards one to a pane, gated on `TTY_WINSIZEQUERY` with the comment "If we did not
  request this, ignore it" (`tty-keys.c`).
- **cm is neither.** It is not a pipe, because it answers what its model can answer and filters client
  replies so one query cannot be answered twice. And it cannot ignore the mode the way tmux does, because
  programs inside cm enable it regardless of the answer.

Generating the report rather than forwarding the client's is what makes the promise keepable. The promise is
all-or-nothing, and sizing changes for reasons that never pass through a client at all; a report built from
the size cm is setting covers those, while a forwarded one would miss exactly them. Pixel dimensions are
reported as `0`, which is what kitty itself sends when it cannot determine them: cm has a grid in cells and
no font, so any other number would be invented. nvim reads only the cell fields.

Two orderings are load-bearing, and both have a test. The report is sent **after** the pty ioctl, because a
report invites the program to call `TIOCGWINSZ` and nvim does exactly that, so the other order makes the
report briefly a lie. And it goes through `queueOrWriteReply` rather than straight to the pty, so it cannot
overtake an outstanding proxied query: a size report is a reply the program did not ask for, arriving when it
may be mid-query, which is precisely the `wallfacer -h` failure shape. Writing it directly passes every other
test in the file and fails only that one.

Rewriting the reply rather than stripping the DECSLRM from the output stream is deliberate, and it is the
same reasoning as "Why not let the attached terminal answer" above: removing bytes desynchronizes the shim's
numbering from the server's. A reply cm generates never enters the log, so rewriting one moves no positions.

The same reasoning permits the one deletion `DenyModes` does make. libghostty emits its own size report on a
resize once the mode is set, and `dropSizeReports` removes those. They are model-generated bytes destined for
the pty rather than session output, so they were never in the log either.

Dropping the model's report is what makes sending cm's own correct, rather than being in tension with it. The
model's is untimely: nothing drains the emulator's queue on the resize path, so it sat there until the next
output arrived and was then delivered as though it answered whatever query came after it, which is the
`wallfacer -h` corruption's failure mode. cm's is sent at the moment of the resize, in order, through the
reply queue. Keeping both would send two reports per resize and the model's would be the one arriving out of
turn. So the division is that the model decides *whether* a report is owed, since it tracks the mode the
program set, and the server decides *when* one is sent.

## One writer per stream

**Exactly one writer per shared byte stream, and bytes cm injects wait for a sequence boundary.**

There are two such streams: the pty, which carries input to the program, and each client's terminal.
Both have several things to say. The pty side has had this discipline since the `wallfacer -h` failure;
see "Ordering, and why a queue is needed" above. The terminal side did not, and the bill came due.

Five writers reached a client's terminal with nothing ordering them: the session's output, the screen
replayed on attach, a proxied query, the outage notice, and the window title. The title did not go
through `TTY` at all, it went to `os.Stdout` from a callback in `cmd/cm`.

The session's output arrives in chunks bounded by a pty read, so a chunk boundary lands inside an escape
sequence routinely: 6 to 8 of the roughly 90 writes in one nvim repaint. A title written at such a
boundary split the sequence. The terminal received `ESC [ 38:2:232 ESC ] 2;nvim ... BEL :102:113m`,
aborted the CSI, took the OSC as a title, and printed `:102:113m` on screen. Those characters shifted
the line, the line scrolled the screen, and every cell nvim did not repaint afterwards stayed stale
until a ctrl-l.

The rules, in `internal/client.screen`:

- The session's bytes are never withheld. Holding them would trade a rendering fault for a stall.
- Bytes cm generates wait while `ansi.Tracker` says the stream is mid-sequence, and go out at the next
  boundary.
- Anything held past `maxHeld` is dropped, not written. Dropping is the safe direction: a title is
  replaced by the next one, a proxied query expires on the server, and the notice repaints on its timer.
  Writing it anyway is the bug.

One deliberate exception, and it is not a violation: `TTY.Close` writes CAN before its reset, which
*aborts* a partial sequence rather than splitting one. That is the correct handling for the one case
where nothing more is coming.

Enforced rather than requested. `TestCommandLayerWritesNoEscapeSequences` fails if an escape literal
appears in `cmd/cm`, because that is exactly how this happened: writing one there is easy and looks
harmless. The command layer states policy, as `SetTitle` does, and constructs no bytes.

**Why the pty side needs no equivalent, measured.** The pty has several writers too, and unlike the
terminal side nothing serializes them: client typing, a client's answer to a proxied query, cm's own
emulator replies, and the in-band resize reports all reach `Session.Write` on their own goroutines, and
`shim.Session.Write` calls `ptmx.Write` outside its lock. The ordering discipline above is about *order*,
which is a different guarantee from *atomicity*, so this looked like the same bug on the other stream.

It is not, because the tty layer serializes a write to a pty master for its whole duration. Measured:
262148 bytes written concurrently with 4000 short writes, on both darwin and Linux, with not one short
write landing inside the payload, while short writes were recorded on both sides of it so the window was
demonstrably open. `TestConcurrentPtyWritesDoNotInterleave` holds that measurement. Chunking
`ptmx.Write` into 4096-byte pieces, which is what routing pty writes through a buffer of cm's own would
amount to, fails it immediately. That is the point at which the pty would need an ordering point of its
own.

One loose end this turned up: `server.Session.Write` discards the `WriteResponse`, so `Written` is never
checked. A short write would truncate input silently. It does not happen today, because `os.File.Write`
loops over `write(2)` until the buffer is consumed, and the test asserts the full count. It is a silent
failure mode rather than a live bug.

**Why tmux and zellij do not have this class of bug, and why copying them is not the answer.** Both keep
their own screen and re-render it, so every byte is theirs by construction. That is also why they cap
what a program can use, and why the emulator sits in the hot path of every byte: measured here at 14ms
for a reverse index against 36us, and `less` emits one per line. cm passes bytes through, which is what
lets kitty graphics and the keyboard protocol work at all, and the price is that cm has to earn the
single-writer property instead of getting it free. This section is that property, stated once.

**How it was found, which is the part worth not repeating.** Three rounds of captures taken *inside* cm
all replayed clean, because none could see a writer that bypassed cm's own abstraction, and an
incomplete capture that replays clean reads as proof the bytes are fine. `kitty --dump-bytes` settled it
in one run: kitty had received 160 bytes cm never sent through `TTY`. When a capture and reality
disagree, instrument the far end.

## What cm is, from a program's point of view

This section exists because a long run of bugs turned out to share one cause, and the cause is not in any
of the code those bugs were fixed in. Anyone about to fix another escape-sequence routing bug should read
this first and check whether the bug is a symptom of what is described here.

A program inside a session negotiates with what it believes is a terminal. It asks questions, reads
answers, and enables features only when something answers. So every multiplexer has to answer one
question, and answer it *consistently*: **what am I, to the program inside me?** There are three coherent
answers, and each of cm's neighbours picks one and holds it.

- **Transparent.** Classify nothing, forward every byte in both directions, never generate a reply. zmx
  does this: it does not wire libghostty's write-pty callback, so it produces no replies and has no
  routing decisions to get wrong. The cost is that features needing a reply do not work, and reattach
  cannot restore what it never modelled.
- **Be the terminal.** Consume a protocol entirely and re-originate it per client. zellij does this for
  kitty graphics: an interceptor pulls APC out of the stream before its parser
  (`zellij-server/src/panes/kitty_graphics/interceptor.rs`), it reads any transfer file itself, stores
  decoded pixels, and synthesizes fresh commands for each attached client. The program never talks to the
  real terminal at all, so there is no round trip to misroute.
- **Be a known quantity.** Advertise what you are, answer what you can, and refuse the rest clearly enough
  that programs adapt. tmux does this. It answers DA1, DA2, and XTVERSION from its own state, proxies only
  OSC 4 and OSC 52, and **drops a reply that matches no outstanding request** unconditionally
  (`input_request_reply` in `input.c` ends `if (found == NULL) return;`). Programs then special-case it:
  `kitten icat` checks for a tmux socket and stops probing, setting file and memory transfer to
  unsupported without asking (`kittens/icat/main.go:305-308`).

cm currently occupies none of these positions, and that is the finding. It forwards graphics APC verbatim,
which is transparent. It routes client input through a reply classifier, which is not. It delivers an
unmatched non-reply to the pty, which is neither zellij nor tmux. And it holds a full terminal model that
it does not use for graphics, so it pays zellij's cost without getting zellij's benefit.

Each of those choices was locally correct when it was made, which is exactly why the position drifted:
every one of them was a bug fix with a test. The consequence is only visible from outside, in what a
program experiences.

### The measured case that showed it

`kitten icat` in a cm session, against a control of the same kitty with no cm. Both under a sandbox, with
the command driven into the session so it ran on the real pty.

| | negotiated medium | result |
| --- | --- | --- |
| bare kitty | `memory` (`t=s`) | clean |
| inside cm | `stream` | exit 0, plus visible garbage |

icat sends three capability probes, each an APC naming a transfer medium: `t=d` with inline data, `t=t`
with a temp-file path, `t=s` with a shared-memory name. Under cm the inline probe answers `OK` and the two
naming a *file* answer `EBADF ... No such file or directory`. cm forwards those answers to the pty, where
a shell sitting at a prompt has `echo` and `echoctl` on, so the tty echoes them back as caret notation. The
proof they are echoes rather than passthrough is the encoding: a capture held 6 literal `^[` sequences
against 466 real `0x1b` bytes. They then land on the *next* prompt line, and are read as input. Observed
consequences: a typed command silently mangled, and `zsh: 3 not found` from the shell executing a fragment
of a graphics error message.

Three fixes were considered and rejected, and the reasons are the useful part:

- **Answer `EBADF` deliberately so icat falls back.** Pointless: icat already falls back on its own, which
  the control shows by it negotiating `stream` and exiting 0.
- **Drop a response while the shell is at a prompt**, using the OSC 133 `Running` flag cm already tracks.
  Rejected as brittle. It depends on shell integration, so a shell that reports nothing would have every
  response dropped, which is the case `cm doctor` already names `no-shell-integration`.
- **Drop an unmatched reply, as tmux does.** This is the one that looked right and is the most instructive
  failure. It cannot be applied to graphics, because `direct` transmission is *mandatory* in icat
  (`kittens/icat/main.go:290`): without the `i=1;OK` answer it hard-fails with "This terminal does not
  support the graphics protocol", so dropping unmatched APC would stop images working entirely rather than
  merely silencing an echo. tmux escapes this only because icat recognizes tmux and never asks.

### What follows from it

The routing is not repairable in isolation, because the defect is the absence of a position rather than a
wrong branch. Two of the three answers above are open to cm.

Being the terminal for graphics is the one that resolves it rather than patching it. It removes the round
trip whose replies are the problem, fixes the file-medium failure, and is the same work
restore-on-reattach needs. That is the option that was taken; see the next section for what it became,
including the one part of it that keeping the pixels turned out *not* to solve.

Being a known quantity is cheaper and weaker. cm could answer XTVERSION as itself, and already emits
OSC 25453. But adaptation is hardcoded per multiplexer in the programs that do it, so it only pays off if
those programs learn about cm, which zmx faces too.

What should *not* happen is another local fix to the reply path for a graphics symptom. That is the pattern
this section is here to interrupt.

Being the terminal for graphics has since been built, and the section below is what it turned into. The
prediction above held: intercepting the protocol is what fixed the file-medium failure and the reply echo,
and it is also what restore-on-reattach needed.

## Kitty graphics: cm consumes the protocol, and its queries have two answerers

cm does not forward the kitty graphics protocol. It parses the commands out of the output stream
(`internal/graphics`), resolves them, and re-emits them. That is the one protocol cm consumes rather than
relays, and it is a deliberate exception to the rule above that the stream is forwarded verbatim.

The reason is that a transmission may name a *file* rather than carry its data, and the file is consumed
once. Kitty opens the path, reads it, and unlinks it: `kitty/graphics.c` deletes any path containing
`tty-graphics-protocol` after a successful read and `shm_unlink`s a shared memory name. So forwarding such
a command means the program's terminal and cm's own reader race for a single-use file, and the loser gets
`EBADF ... No such file or directory`. Measured against a control of the same kitty with no cm in between:
`kitten icat` sends three capability probes, and exactly the two that name something on the filesystem
failed while the inline one answered `OK`.

Reading the file in cm and rebuilding the command with its bytes inline means one reader. Three details of
that are load-bearing and each was found by a failure rather than by design:

- **An inlined payload is bounded by the geometry**, not by the container or by `S=`. A terminal derives
  the expected byte count from `s*v*(f/8)` for a raw pixel format and rejects anything longer:
  `graphics.c:731` computes exactly that and allocates it plus ten bytes. `S=` describes the *container*,
  which differs, because a shared memory object is rounded up to a page: a 3 byte image arrives inside
  4096, and trusting `S=` hands the terminal thousands of bytes of padding. That surfaced as
  `EFBIG: Too much data`.
- **Shared memory is read, not declined.** Declining it looked like a safe fallback and was a downgrade:
  bare kitty negotiates `memory` with icat, and a cm that refused that medium got `files`. Reading it needs
  `shm_open` on darwin, where the name is not a filesystem path at all, and a darwin shm descriptor does
  not support `read(2)` -- it fails `ENXIO`, which surfaces as "device not configured" and reads like a
  missing device rather than the wrong syscall.
- **Re-emission is forced to `q=2`.** An image cm sends generates no response, so nothing arrives on the
  input path answering a question cm never asked. That is what keeps the restore path out of the reply
  routing entirely.

**The store has to be rebuilt on adoption, and was not.** A client attaching gets the images re-sent ahead of
the screen, because a restored screen carries placements referring to images by id and libghostty's formatter
does not re-emit the images themselves. So what a client receives comes from cm's own store of the payloads,
not from the model.

Adoption rebuilds the model by replaying the shim's retained history, and that replay went straight to the
model without passing the graphics interception. The model therefore regained its images and cm's store did
not, so a client attaching after a server restart received placements with nothing to resolve them: the image
was blank and nothing said why. The code said this was fine, on the reasoning that an adopted session "starts
with no images and regains them as the program transmits, which is the same bound the model has on its own
storage". The model does not have that bound, which is the part that made the claim wrong rather than merely
pessimistic.

The replay now records into a store as well as feeding the model, using the same bookkeeping the live path uses
(`recordGraphics`) rather than a second copy of it. One scanner spans the whole replay, because a
transmission's payload is chunked and reassembles across calls; a scanner per chunk sees fragments and records
nothing.

### The part that is not understood, so do not repeat the attempt

A graphics query has **two** answerers, and the interaction between them is unresolved.

cm's terminal model answers these on its own. Verified directly: feeding
`ESC _ G a=q,f=24,s=1,v=1,S=3,i=1;MTIz ESC \` to the model produces `ESC _ G i=1;OK ESC \` on the
`WritePty` callback, which `internal/vt/adapter.go` wires, so it reaches the pty in production. The
attached terminal also answers, because cm forwards the query. One probe therefore still comes back
`ENODATA: Insufficient image data: 0 < 3`.

The obvious fix is to suppress the model's graphics replies in `DenyModes`, alongside the margin and
size-report cases above, on the reasoning that the real terminal is what draws the image so its answer is
the true one. **That was tried and made things worse**, and the numbers are the reason it is recorded here
rather than left for someone to rediscover:

    before   =3;OK =2;OK =1;ENODATA   -> negotiates memory, exit 0
    after    =3;OK =1;OK              -> "does not support the graphics protocol", exit 1

Suppressing the model's replies made a probe's answer *disappear* rather than deduplicating anything, so
the model's answer was not redundant: for at least one probe it was the only one arriving. icat's detection
loop quits on a DA1 sentinel, so a missing answer becomes "unsupported", and `direct` transmission is
mandatory (`kittens/icat/main.go:290`), which turns one absent reply into a hard failure.

Nine unit tests passed for that change, including a round trip through the real emulator, because they
assert what `DenyModes` produces and cannot see what the *other* answerer does. Only the
kitty-versus-control comparison caught it. So the next attempt needs a capture of what the terminal replies
and what the model replies, side by side, rather than a theory about which is redundant.

The remaining symptom is **not** cosmetic, and an earlier version of this section said it was, which is the
kind of false claim in a doc that costs someone real time. What was measured then was whether `icat`
succeeded: it negotiates the same medium as the control, exits 0, and displays images, all of which is
true. What was not measured was the terminal afterwards. Driving another command shows it:

    CHANCEZ-M-2YPG% =3;Oo NEXTLINE=2;OK=1;ENODATA:Insufficient image data: 0 < 3
    zsh: 3 not found

`echo NEXTLINE` was typed and arrived as `=3;Oo NEXTLINE`, so the response text is rendered into the grid,
it consumes typed characters, and zsh executes the `3` out of `=3;` as a command. Zero real escape bytes are
in what kitty displays, so these are printable characters on the prompt line rather than an invisible
sequence. That is the same failure as the reply echo this whole change set began with: bytes reaching the
pty while a shell sits at a prompt, where the tty echoes them and the line editor reads them as input.

So the interaction above is a live defect. What it also shows is that the two-answerer diagnosis was right
and only the fix was too broad: dropping *every* model graphics reply removed one that was the only answer
arriving, which is why `i=2` disappeared. The narrower rule is to drop a reply only for a command cm
**forwarded** to the terminal, and keep it for one cm **consumed**, since for a consumed command the model
is the sole answerer. `handleGraphics` already knows which of the two it did to each command.

## Events that outlive the program that asked for them

A program turns on optional reporting, exits, and the terminal keeps sending it for a moment. Those
events reach the pty and are typed at whatever is reading it now, which is the shell.

Reported as `execute: 3u[O_` left in zsh's line editor shortly after quitting codex. codex pushes kitty
keyboard flags 7, which includes report-event-types, and sets mode 1004. On ctrl-d it reads the key
*press* and exits; the *release* is generated afterwards. zsh's line editor ate each ESC as a meta prefix
and inserted the rest, so `\x1b[100;5:3u` showed as `3u` and `\x1b[O` as `[O`.

**Not a reply-ordering bug**, which is the trap: the symptom is identical to the recorded `wallfacer`
corruption, and the first three hypotheses were all about the query proxy. Measured against bare kitty in
the same terminal, the proxy delivers in 3.8-4.1ms against 3.6ms with order preserved, so it was never
slow or misordered. The clue that redirected it was the byte shape: `3u` is a key release, not a reply.

Why they reached the shell, measured through cm's own classifiers:

| client sends | IsUserInput | IsQueryReply | SplitInput | old result |
| --- | --- | --- | --- | --- |
| `\x1b[100;5:3u` release | false | false | nil | `sess.Write` verbatim |
| `\x1b[O` focus out | false | false | 1 part | `sess.Write` verbatim |

cm already recognized both as not-typing. Nothing acted on that: `Service.Attach` only splits a chunk
when it yields more than one part, so both fell through to the verbatim write.

The fix is a filter ahead of the typing decision. cm's model tracks the kitty keyboard flags and mode
1004, so flags back at zero means no program in the session wants protocol events and an event in that
encoding was generated for one that has gone. Same shape as `DenyModes`: cm is the one that knows.

**Only a release and a focus report qualify, never a key press**, and that asymmetry is the design rather
than an omission. The two ways of being wrong are not comparable. Dropping a stale release loses nothing,
because no shell wants one and a program that does want them degrades to not seeing releases. Dropping a
press would make a session ignore the keyboard, and the model can read flags zero while a program really
has them on, most plausibly after a server restart rebuilds the model from a bounded log that no longer
contains the push.

The race is how fast the key is lifted: the program's own pop has to reach the terminal before the
terminal encodes the release, and under cm that is a round trip through the shim, the server and the
client. It reproduced twice in three tries quitting codex as soon as it opened, which is why the
regression test constructs the state at the seam instead of racing it.

Mouse reports are the same shape and are deliberately not covered, since dropping one wrongly costs the
user the mouse. See `docs/ideas.md`.

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

**And the model can be ahead of what it is able to describe, which is the same bug from the other side.**
A partial escape sequence lives in the emulator's parser, not on its screen. `Restore` serializes the
screen, so resuming at `modelSeq` puts those bytes in neither the snapshot nor the stream: the client
receives the tail of a sequence whose front nothing ever sent. Measured on a program writing
`ESC ] 2;fidelity BEL ESC [ 38:2:1` and then pausing: the attaching client had the title set and printed
`:2:3m` as text, the nine bytes that opened the SGR gone, on about one attach in eight.

So `modelPending` counts the bytes of an unfinished sequence at the end of what the model consumed, and
attach backs off by that much. Replaying a partial sequence is free, since the client's terminal completes
it when the rest arrives; skipping it is not.

Stored as a count rather than as a second position on purpose. "The position, and the position of the last
boundary" has to be kept consistent by everything that writes either, and the first version was: a
`Session` built in a test set only `modelSeq`, so the boundary stayed zero and the attach replayed the
entire log. A backlog whose zero value means "nothing pending" is right by default, which is what a field
guarding a subtle bug should be.

## What a session reports about itself

A terminal emulator driving cm needs to know more than "this session exists". Three things are tracked
from what the shell says in its own output, and all three are published as events as well as being
readable with `cm info` and `cm list --json`.

Title comes from OSC 2, and working directory from OSC 7, decoded rather than passed through: OSC 7
sends a percent-encoded URI with a host, so a session that has ssh'd elsewhere reports a path that does
not exist locally, and acting on it would open the wrong place.

The `cm list` table abbreviates a directory under home to `~/...`, and does so only for a local one.
That the host is decoded is what makes the distinction available: a remote session's home is the remote
user's, so rewriting `/home/user/x` against this machine's home would assert a relationship that does
not exist, and both hosts putting users under `/home` is what makes it look right. The abbreviation is
display-only for the reason `docs/cli.md` gives, that `~` expands in a shell and nowhere else.

### Sizing the TITLE column

TITLE gets whatever width the other columns leave, rather than a fixed 30. A title is how a person
recognizes which window a session is, and `claude: reviewing the wid...` identifies nothing. Measured on
a 120-column terminal against two ordinary sessions: both titles were cut at 30 and both fit whole once
the budget was dynamic, with the widest row at 119 columns.

Three things make this more than `termCols - fixedCost`.

**The budget is computed from rendered cells, not from column definitions.** What the other columns cost
is only knowable from their content: PID is five digits or six, and STATE is `running` or
`running(blocked: needs approval)`. So every cell is rendered into a slice first, the other columns are
measured, and only then is TITLE truncated. That is also why the arithmetic has to match `tabwriter`
exactly: a column is as wide as its widest cell including the header, plus padding, and the *last* column
is not padded because nothing follows it. Both facts were probed rather than assumed, and both are
mutation tested. Getting either wrong by two columns wraps every row.

**Widths are counted in runes, because that is what `tabwriter` counts.** Counting bytes over-reserves for
any non-ASCII cell and hands TITLE less width than the terminal had, which renders correctly and is
therefore invisible. A path under a name with accents is the ordinary case. Truncation moved to runes at
the same time and for a second reason: cutting at a byte offset split a multibyte rune, so a title of
accented characters rendered as `ééé\xc3...`, putting visible corruption next to the marker that says the
tail was dropped. Rune count is still not display width, since a CJK or emoji rune occupies two cells and
counts as one, but matching the aligner is what keeps the columns lined up, and such a title costs
alignment on its own row rather than correctness.

**Not a terminal means the old fixed width.** Piped or redirected output must not vary with whoever's
window ran the command, or `cm list > file` stops being reproducible and diffs noisily between two
people's terminals.

The floor is the old 30 and the column never shrinks below it. Shrinking on a narrow terminal was
considered and rejected: CWD sits last and is unbounded, so an 80-column terminal showing a deep path
wraps whatever TITLE does, and the trade would be a shorter title in exchange for a table that still
does not fit.

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

They exist because a name groups a session one way and often cannot group it at all. A session created
without one has no name to match, so `cm list --prefix` has nothing to filter it by, and it is reachable
only as `@<id>`. Even a deliberately named session belongs to several groupings at once -- a project, a
worktree, the fan-out that created it -- while its name says one thing.

Names can now be changed, which they could not when this was written: a name is a binding, so `cm bind`
corrects a misleading one. That removes the argument tags used to rest on most heavily and leaves the other
two, which are the ones that were always load-bearing: a session with no name has nothing for `--prefix` to
match, and one grouping per name is one too few.

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
