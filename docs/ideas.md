# Ideas

Things cm could grow, kept here so they can be weighed later instead of implemented now or forgotten.

Nothing here is a commitment. Each entry says what it would do, why it might be worth it, and what it
would cost, because the cost is usually the part that decides. Where an idea has already been half-answered
by something that happened, that is recorded too: several of these exist because a bug or a measurement
pointed at them.

This is not a task list. Work that is actually in flight lives outside this file; what is here is
unstarted, and some of it should stay that way.

Two standing constraints shape the whole list. **Windows, tabs, and splits are out of scope** -- the
terminal emulator already does those, and not competing with it is why cm is small. And **cm never learns
what is running inside a session**: a state a program reports is just a state, so nothing here should
special-case an agent, a build tool, or a shell.

## Known gaps

These are missing rather than undecided. Each is small on its own; the reason none is done is that nothing
has needed it yet.

**Going back after a switch.** `cm switch work` moves this window to another session, and there is no
convenient way back. It is expressible: the window's name still points at the original, so
`cm switch kitty.164` returns. What is missing is discovering that name from inside the new session, since
`CM_SESSION` there is the *new* session's ID and nothing reports the name the window was launched under.
After a `cm rebind` it is worse, because no name points at the original at all and it appears in `cm list`
only as an unnamed `@id`.

The shape: `--prev` returns to the session this window was on before its last move, and `--reset` to the
first session this client attached to.

**The client keeps the history, and the server keeps nothing.** This looks like it needs server-side state
and does not. The client is the one process that survives every switch, now that a switch reattaches in
place rather than re-execing, so it can hold the whole history in memory for as long as the window lives. It
then attaches to a session out of that history exactly as it would to any other, and the server learns the
target as the ordinary `Open` of the new attachment, indistinguishable from a fresh attach.

What the server does is relay the request, and only because the request and the knowledge are in different
processes: `cm switch --prev` is a separate CLI process running inside the session, it can reach the server,
and it has no channel to the client at all. So the server-to-client event that already carries a switch
target grows a second shape it never looks inside -- "go back one" or "go back to the first" -- which it does
not resolve, validate, or store. Calling that tracking would be wrong, and so would calling it forwarding a
target: the server never knows where the client went.

A design that removes even the relay: make it a *key* rather than a command. The client already intercepts
one, the detach key, so a "go back" key needs no server involvement whatsoever, which is also how tmux does
its last-session binding. It is not a replacement, because a key cannot be scripted or bound from a kitten
and each operation needs one of its own, but it is the cheapest possible version and worth knowing about
before building the plumbing.

The alternative was for the client to report its history on each `Open` so the server could resolve it.
Rejected: it duplicates state the client already owns, and the only thing it bought was checking the target
exists before any window moves, which turned out to be a separate bug rather than an argument. See below.

Since the client owns it, a list costs nothing more than two values, so `--prev` can be a real step
backwards rather than a toggle. What that opens up is **skipping**: a candidate whose session has since been
killed can be passed over for the next one, which is only possible with more than one candidate to try. The
client is also the only place that can try cheaply, since it can attempt the attach and move on. Running out
of candidates is an error naming what was asked for; falling back to something unrelated is how a command
stops being trustworthy.

A prerequisite, now fixed, was in the way of all of it: a client attaching to a session that does not exist
retried silently forever, because an ID that resolves to nothing is refused rather than created. `cm attach
@deadbeef` printed nothing and held the window. Skipping a dead candidate is impossible while a dead
candidate hangs instead of failing, so the fail-fast fix is what makes this buildable at all.

The one thing that does need the server: `cm rebind --prev` would have to write a binding, and only the
server can, so it would need the target rather than a directive. Two ways out, and neither is decided.
Back-navigation could be switch-only, with moving a name back left to an explicit `cm rebind` once the
target is known; or the client could report its history purely so the server can offer it, which is also
what a "came from" column in `cm clients list` would need, and that column answers the discovery half of
this for someone who never types the flag.

**Alternate-screen scrollback.** A full-screen program draws on the alternate screen, and lines that
scroll off there are gone: `cm read --lines` cannot recover them, because they never entered scrollback.
This is correct terminal behavior and also the single most confusing limitation in practice, because the
symptom is a reply that looks truncated for no reason. The `cm` skill tells an agent to fall back to
writing a file. Whether cm could retain more is a libghostty question, not a cm one, and worth asking
upstream before designing anything.

Note the resumption-mark entry below covers the same gap from the other side: it does not recover the lost
lines, but "what changed on the screen" is answerable for a full-screen program where a command boundary is
not.

**Kitty graphics across a reattach.** Done, and not the way this entry expected, which is why the rest of
it is kept: the plan here was to re-emit from libghostty's image storage, and that turned out to be the
wrong source. See "Kitty graphics" in [architecture.md](architecture.md) for what was built and for the one
part still not understood.

What changed the design was a measurement. libghostty stores what a payload *decodes to* and discards the
payload, so rebuilding a transmission from it means re-encoding raw pixels: 90x the inbound size for the
captured 1712x1294 screenshot, 11815084 bytes against 217378. cm keeps the compressed bytes the program
sent instead, in its own store, and replays them verbatim. libghostty's storage is for rendering; cm's is
for re-transmission, and neither replaces the other.

Two things this entry got wrong, recorded because both were load-bearing. Storage was described as
something to enable, and it is on by default at 10000000 bytes, so cm had been retaining images since it
first linked the library without using them. And the hard part was never the re-emission: it was that a
transmission can name a single-use *file*, which cannot be forwarded at all.

Everything below is the original entry, left as written.

Images pass through as APC bytes while a client is watching and
are absent after reattaching, because libghostty's formatter does not re-emit them. `alternatives.md`
lists this under shared limitations, alongside zmx, as a property of the approach.

That framing is now out of date, and the reason is worth stating precisely because the formatter has
not changed. The pinned libghostty commit already ships an *inspection* API for kitty graphics:
`third_party/ghostty/include/ghostty/vt/kitty_graphics.h`, added upstream in `bdc0b6c19` on 2026-07-03,
where `GHOSTTY_REF` is `4d605bf0d` from 2026-07-30. Placements are iterated
(`ghostty_kitty_graphics_placement_iterator_new`, `_next`, `_get`) and each resolves to an image handle
whose `ghostty_kitty_graphics_image_get` reports width, height, pixel format, and a borrowed pointer to
the pixels. `internal/vt` wraps none of it. So re-emission is something cm could build rather than
something to wait on upstream for, which is a different kind of gap from the one recorded today.

Two constraints decide the shape, and both are in the header rather than inferred:

- **Storage is opt-in and needs a PNG decoder.** Nothing is retained unless
  `GHOSTTY_TERMINAL_OPT_KITTY_IMAGE_STORAGE_LIMIT` is non-zero *and* a decoder callback is installed
  through `ghostty_sys_set` with `GHOSTTY_SYS_OPT_DECODE_PNG`. cm sets neither, so cm's terminal model
  currently holds no images at all. Supplying a PNG decoder is a dependency decision, not a flag.
- **What comes back is decoded, uncompressed pixels, never the original PNG.** The header states the
  reported format is never `GHOSTTY_KITTY_IMAGE_FORMAT_PNG` and that zlib payloads are inflated before
  storage. Restoring therefore means re-transmitting raw RGB or RGBA, which can be far larger on the way
  out than what arrived. That number is the first thing to measure, because it decides whether this is
  viable at all: a screen of images is bounded by the storage limit cm chooses, not by what the program
  sent.

Borrowed handles are invalidated by the next mutating terminal call, which fits the existing
single-writer discipline rather than fighting it. See [concurrency.md](concurrency.md).

zellij shipped this in 0.45.0 and its design is worth reading first, since it solved the parts that are
not about the C API. Images live in a session-global refcounted store with LRU eviction under a quota.
Placements are anchored to a *canonical* (pre-wrap) line index rather than a screen row, so reflow on
resize does not detach an image from the text it belongs to -- which is the same class of bug as the
scrollback-shift on resize in [restore.md](restore.md). Re-emission is driven off a *per-client* map
from internal image id to host image id, cleared on attach and detach, so a newly attached client
re-transmits everything by construction rather than through a special reattach path. Host ids are
allocated from 2,000,000,000 upward to avoid colliding with whatever else is talking to the terminal.

Worth knowing what zellij does *not* do: images are not serialized to disk, so a zellij session
survives reattach but not a reboot. cm persists content across a reboot, so cm would face a question
zellij never answered, and the pixel-size measurement above is what decides whether the answer can be
"yes" rather than "content only, minus images".

This has since become more than a restore feature, and that is the argument for doing it rather than
deferring it again. Intercepting graphics is also what would resolve the position cm currently has no
answer for, described under "What cm is, from a program's point of view" in
[architecture.md](architecture.md): forwarding a graphics query to a real terminal and routing the reply
back is what produces the reply-echo and file-transfer failures observed with `kitten icat`, and
consuming the protocol removes that round trip entirely. So the same work fixes a live bug and a
documented gap.

**`cm doctor` checks as incidents arise.** The standing rule is that a debugging session that cost real
time should leave a check behind. The obvious candidate -- a session that reports neither OSC 133 nor its own
state, so `--wait idle` hangs -- is already covered by `no-shell-integration`, which checks both. What is not
covered: a shim whose persisted log has stopped growing while its session is live, which is a silent
downgrade the shim logs and nobody reads; and a session whose reported state has been `busy` for
implausibly long, which usually means a reporter crashed between its start and end report and left the
session permanently un-waitable.

**Session rename.** Done, and not the way this entry proposed. It described keeping a display name separate
from the identity, which is the cheaper half of the right answer; what shipped went the other way and made
the ID the identity, so a name is nothing *but* a display name. Renaming is `cm bind` plus `cm unbind`, a
session can have several names, and `cm switch` points a terminal window at another session. See
`docs/architecture.md`.

## Driving sessions programmatically

The part of cm that gets used from scripts and agents, and where the last few real bugs were.

**`cm exec`, a one-shot with clean output.** `cm run` on a reused session sends input to a shell, so the
output contains the shell's echo of the command and the prompt around it. That is honest -- it is what the
session printed -- and it is not what a caller parsing the result wants. A form that ran the command
without a shell echoing it, or that stripped the echoed line and prompt, would remove the most common
post-processing step. The catch is that "the command's own output" is not well defined once a shell is
involved, which is why `cm run` reports what it does today.

**Structured output boundaries.** Done: `cm read --since-commands N` and `--last-output`. See
`docs/architecture.md`.

Worth recording what changed on the way, since this entry proposed "output since sequence N" and that is
deliberately not what was built. A sequence number at the CLI is a hazard rather than a convenience: cm
has two sequence-number spaces, mixing them corrupts output, and a stale position read from the wrong
place silently rather than failing. A command count is also what a person and a script actually think in.
The wire carries the resolved position, and nothing asks a user for one.

What is still open, and was not obvious until this existed: boundaries live in memory with the session,
so a server restart forgets the ones before it and an ended session has none at all. Persisting them
would mean writing a position per command to the store, which is a row on the hot path for something only
read back interactively. `cm run` already covers the ended-session case by saving output, so the gap is
narrow: reading back a command that ran before the last server restart.

**A resumption mark: "what happened while I was away".** Two use cases converge on one primitive, and they
want different presentations of it, which is the part to settle before building anything.

The human one, and the one that motivated this: coming back to a session you tabbed away from, and wanting
to start reading where you stopped watching rather than at the bottom. Scrolling up to hunt for the place
you left off is the actual daily annoyance, and it gets worse the more output arrived while you were gone.
What is wanted is a position, not a diff: put the viewport where I was, and let me scroll down to now.

The agent one: "what changed on the screen since I last looked". `--last-output` and `--since-commands`
already answer this *when the shell emits OSC 133*, and better than a screen comparison could, because a
command boundary is a real event rather than an inference. The case they do not cover is a full-screen
program, which brackets nothing and draws on the alternate screen, and that is the same gap the
alternate-screen entry above describes. AgentAPI exists for exactly this reason: it drives coding agents by
comparing screen snapshots and attributing text that appears below the previous content to the agent.

That comparison is worth being careful about, because it looks like the thing this file refuses under
"Detecting what is running" and is not quite. Two separable things:

- *Structuring the screen* -- "these rows differ from the rows I saw last time", "the screen stopped
  changing". No program knowledge, nothing to chase as a UI changes, and cm's terminal model already holds
  everything needed.
- *Interpreting the screen* -- "this `>` is a prompt box", "this is an approval dialog". Needs per-program
  knowledge and rots as the program changes. This is what the refusal is about and it should stay refused.

AgentAPI is evidence the line is real: its segmentation is program-agnostic and only its cosmetic cleanup
is UI-specific, so the parts fail independently. Worth writing the distinction into the refusal, since as
stated it reads as forbidding both.

What blocks the human half is not the reading, it is knowing when "away" began, and the answer is worse
than it first appears. cm does not learn that a window lost focus unless the *program inside the session*
enabled DECSET 1004, because `internal/client/terminal.go` never enables focus reporting for cm's own
purposes -- it only puts the terminal in raw mode and resets it. So a session sitting at a shell prompt,
which is the common case for tabbing away, reports nothing. `Session.ReportFocus` gates on
`term.FocusReporting()` for this reason, and `service.go` calls it on first attach and last detach, so
attach and detach are the only focus edges cm reliably knows.

Three ways to get a mark, in increasing order of how much cm has to invent:

1. *Detach and reattach.* Already an event cm sees, and the resume machinery already carries a position.
   This is nearly free and covers closing the window, but not tabbing away, which is the case asked for.
2. *An explicit mark.* `cm mark` from a keybinding, or a flag on read. Predictable and needs no detection
   at all, but it asks the user to remember to say so before leaving, which is the one thing they will not
   do.
3. *cm enabling focus reporting itself* so it learns about tabbing away regardless of what is running.
   Tempting and the most invasive: cm would be setting a mode on the user's terminal that nothing asked
   for, and it has to be undone exactly. There is also a latent hazard to fix first --
   `internal/server/service.go` forwards a client's focus events to the pty unconditionally, while
   `ReportFocus` gates on whether the program asked. Consistent today only because the program that
   enabled 1004 is the one receiving them; if cm enables it for itself, a program that never asked starts
   receiving `ESC[I`/`ESC[O` as input.

The cheap version worth trying first is (1) plus a read that takes a position, since both halves already
exist: attach records a resume point, and `read --since-commands` proves a boundary-relative read is a
shape people want. Note the presentation differs from every existing read: a mark wants output *and* a
viewport placed inside it, where `read` prints a range and returns. Whether that belongs in `read`, in
`attach`, or in the scrollback the terminal already owns is genuinely undecided, and the last option
deserves weight -- if a mark were an OSC the emulator understood, the scrolling would be the emulator's
job, which is the side of the line this project usually wants to be on.

**A timeout on everything that can block.** Done: `cm read --follow --timeout`, plus one shared definition
for the flag that wait, send, and run already had. See `docs/architecture.md`.

What the entry asked to settle first turned out to be the whole design: a deadline does not mean the same
thing everywhere. wait and run exit non-zero because they were asked for a result; a follower exits zero
because a deadline is simply being told to stop, and failing would make a caller discard output it received.

**Waiting on more than state.** Done: `cm wait --match`, with `--match-raw` for the emitted bytes. See
`docs/architecture.md`.

Plain substring rather than a regex, deferred rather than rejected: substring covers "did it print DONE",
and a regex on a stream raises anchoring questions -- whether `^` means start of line or start of output --
that deserve their own thought. The matcher is a seam rather than inline code precisely so a regex, or a
`cm watch` consuming the same evaluation, needs no second implementation of the chunk-splitting and
escape-sequence handling.

**Waiting on several sessions.** Done: `cm wait --tag` waits on a group concurrently, requiring every
session by default and returning on the first with `--any`. See `docs/architecture.md`.

What is still open is waiting on an arbitrary *list* of names rather than a group that shares a tag. Tags
covered the case that motivated this, since a fan-out is usually created together and can be labelled as it
is, and a caller with a list of unrelated names can still background one wait each. Worth revisiting only if
something needs to wait on sessions it did not create and cannot tag.

**Bracketed paste for `cm send`.** `cm send` writes exactly the bytes it was given. For a single line or a
`--key` that is right, and for a multi-line block it is a footgun: a shell reading `a\nb\nc` runs three
commands as the newlines arrive, and an editor auto-indents each line as though it were typed. The caller
most likely to hit this is the one cm is built for, an agent sending a heredoc or a code block.

The fix is to wrap the payload in bracketed paste (`ESC[200~` ... `ESC[201~`), which tells a program that
supports it to treat the bytes as pasted data rather than keystrokes. wezterm's `cli send-text` defaults to
this and has `--no-paste` for the raw form, which is the right way round: the safe behavior is the default
and keystroke semantics are the opt-in.

What needs deciding first is the compatibility question, and it is the reason this is recorded rather than
done. Bracketed paste is a *mode*: a program that has not enabled DECSET 2004 receives the wrapper as
literal `ESC[200~` text and puts it on its command line. cm already knows whether the mode is set, since
the terminal model tracks it, so the choice is between honoring that (wrap only when the program asked,
which is correct but makes the same command behave differently depending on what is running) and an
explicit flag (predictable, but the default is then wrong for somebody). Note `--key` must never be
wrapped, since a keystroke is not a paste, and that is the one part that is unambiguous.

Not to be confused with what `cm send` already gets right: `--key` accepts named and control keys
(`enter`, `up`, `f5`, `ctrl-c`, `c-c`, `^C`), so a caller wanting an interrupt does not have to produce
the byte. `internal/input/keys.go` records the incident that motivated it. kitty's `send-key` and `ht`'s
`sendKeys` are the same idea, and cm is not behind there.

**Idempotent send.** `cm send` writes to a pty; if the call fails partway there is no way to know how much
arrived, and no way to retry safely. This has not bitten anything yet, and would matter for a caller
driving cm over a flaky link.

Prior art worth having before designing it, because it is smaller than it looks. Eternal Terminal solves
the same problem for a dropped TCP connection with a byte counter per direction: each side tracks how much
it has consumed and announces that on reconnect, and the writer replays the difference from a bounded
buffer. No idempotency tokens and no request ids, just a count. cm already has that shape across a
*process* boundary rather than a network one, since the shim's sequenced log plus adoption replay is
exactly "tell me your position and I will resend the delta". So this is less a new mechanism than the
existing one applied to the input direction, which is currently uncounted.

## Reporting and integration

**Timing in reports.** OSC 133 gives cm the start and end of every command, so per-command duration is
already derivable and is not exposed. `cm list` showing "running for 4m" would answer the question people
actually ask a multiplexer, and the data is in hand.

**A richer shell integration.** The current one provides `cm_report` and deliberately installs no prompt
hook, because a shell at its prompt is idle rather than blocked and hooking it would mark every session
blocked forever. What a prompt hook *could* usefully add is a report that includes the command's duration
or exit status in a form cm does not already get from OSC 133 -- which, on inspection, is almost nothing.
Worth restating so the next person does not re-derive it: the integration is small because OSC 133 already
covers most of what a hook would report.

**Agent hooks as first-class contrib.** `contrib/hooks/` has a stop-hook example. Wiring `cm report` into
whatever hook an agent already has is the single highest-leverage thing a user can do, because it turns a
session cm cannot read into one it can wait on. More worked examples, per agent, would be cheap and useful.
Nothing in cm changes for this.

**Notification on state change.** Something outside cm has to poll `cm list` to notice a session becoming
blocked. A `cm watch` streaming state changes, or a configurable command run on transition, would let a
terminal emulator or a notifier react. The server already publishes these internally to attached clients,
so the plumbing exists; what is missing is a client-facing form.

Deliberately not built yet, and the reasons are worth having before someone starts.

*It cannot promise not to miss transitions.* `metaSub` is buffered to a depth of one and coalescing, so a
command like `true` starts and finishes between two reads and collapses into a single event. That is already
recorded in `wait.go`, and it is why `awaitState` compares a counter rather than watching for the session to
*be* busy. A stream of state changes has no counter to fall back on, so `cm watch` would show "idle" twice
for a fast command and silently omit that anything ran. Acceptable for a notifier, which cares about a
`blocked` that persists rather than every edge; not acceptable for anything reconstructing what happened,
and the entry should say so rather than let it be discovered halfway through.

*Most of what it would serve is already served.* "Tell me when an agent needs input" is `cm wait --until
blocked`, over a group with `--tag`. "React to a session ending" is `--until exited`. What is left that is
genuinely only `watch`: reacting without knowing which session in advance, reacting repeatedly without
re-invoking, and a consumer that is not attached. The plumbing "already exists" precisely because the
existing consumer is the attach stream, so a terminal integration -- the party most likely to want this --
already receives these events today.

*What would justify it* is a concrete consumer that is not attached: a notifier daemon, or the dotfiles
integration wanting to react to `blocked` without holding an attachment. Until one exists this would be a
second delivery path for something already delivered.

Note also that it does not reuse the output matcher `cm wait --match` is built on, despite an earlier claim
in `docs/architecture.md` that it would. State changes come from the metadata subscription; "has this text
appeared" is a different question.

## Output delivery

**Dropping stale mouse reports.** cm already drops a kitty key release and a focus report that arrive when
its model says no program wants them, which is the fix for `execute: 3u[O_` appearing after quitting codex.
Mouse tracking is the same shape: a program enables 1000/1002/1003, exits, and a report generated before
the reset reaches the terminal is typed at the shell. libghostty exposes the state as
`GHOSTTY_TERMINAL_DATA_MOUSE_TRACKING`, so the check is cheap.

Left out on purpose, because the cost of being wrong is not symmetric with the keyboard case. A dropped
release is invisible; a dropped mouse report means the mouse stops working in a session, and mouse state is
where the model is most likely to disagree with the terminal, since `docs/restore.md` records three
separate mouse modes being replayed to a fresh client. Worth doing if anyone reports the mouse artifact,
and not on speculation. See "Events that outlive the program that asked for them" in `docs/architecture.md`.

**Coalescing output to a client that has fallen behind.** cm streams bytes to every client. A client that
cannot keep up falls behind and is eventually told there is a gap, at which point
`internal/client/attach.go` drops its resume point and repaints from a fresh attach. That recovery works,
and it means the failure is already handled: what is not exploited is that cm holds a terminal model and
could therefore send a client *the current screen* instead of a backlog it will never care about.

The payoff is not bandwidth, and describing it that way undersells it. mosh's argument is that a protocol
obliged to deliver every byte fills network and pipe buffers, and a full buffer is what makes ctrl-c feel
dead: the interrupt is delivered promptly but the output already queued ahead of it still has to drain.
Because mosh is free to skip intermediate frames, it can regulate output so buffers never fill, and ctrl-c
takes effect within a round trip. That is a responsiveness property, reached by giving up byte-exact
delivery.

**What makes this worth recording for cm specifically is that cm does not have to make that trade.**
Coalescing would be a *client delivery* decision, while the shim's log stays byte-exact, so `read`,
`history`, and `wait --match` keep seeing every byte. mosh cannot do this: it synchronizes screen state and
therefore has no stream, which is why its own answer to scrollback is to tell you to run a multiplexer
underneath. Eternal Terminal takes the opposite side, keeping the byte stream and therefore never skipping
frames. cm holding both a model and a log is what makes both halves available at once, and no tool in the
field appears to do that.

What the neighbours do when a client falls behind, since the design space is narrow and all three answers
are worse than the one above:

- tmux keeps per-client, per-pane byte offsets into one buffer and drains it only to the minimum offset
  across every consumer, so a slow client applies backpressure all the way to the pty -- when every
  consumer is off, it stops reading the pty entirely. It then bounds the damage crudely: a control client
  whose oldest queued block exceeds `CONTROL_MAXIMUM_AGE`, 300000 ms, is killed with "too far behind",
  unless pause mode is on, in which case the pane is paused and a `%pause` notification is sent.
- screen's `nonblock` declares a display blocked after a timeout and stops sending it output, which is the
  same idea with no recovery story.
- zellij pushes whole viewports to subscribers and diffs them, which coalesces by accident but re-sends the
  entire viewport on any single-cell change.

Two things to settle before building. cm's server cannot apply backpressure to the pty the way tmux does,
because the shim owns it, so "stop reading" would have to become a shim protocol change -- which is
probably an argument *for* coalescing rather than against, since coalescing needs no such thing. And
switching a live client from byte stream to screen state mid-session is a different operation from
restoring on attach: the serializer exists ([restore.md](restore.md)) but it is written for a client that
is about to start painting, not one already mid-stream. The measurement that decides whether this is worth
it is how far behind a real client actually gets, which nothing currently reports.

## Bigger expansions

Each of these is a larger change than anything above, and each has a specific reason it is not a small one.

**A web UI.** A browser view of sessions -- what is running, what is blocked, a live screen -- for watching
work from something other than a terminal. The transport is the real cost, not the UI: ttrpc has no HTTP/2,
so no browser can talk to it. That means gRPC-web or Connect for at least the browser-facing surface.

Read-only and interactive are very different amounts of work, and it is worth deciding which is wanted before
anything else. Every RPC except `Attach` is unary -- `List`, `Read`, `History`, `Wait`, `Status` -- so a view
that shows state and polls a session's rendered output needs no streaming at all. `Attach` is the exception,
and it is a full-duplex bidi stream that cannot be approximated half-duplex without losing the ability to type
while output streams. A dashboard is much less than a terminal in a browser.

`docs/rpc.md` already measured that trade: Connect adds 10.9 MB to the binary and gRPC 12.3 MB, against
ttrpc's 4.7 MB, and the binary re-execs itself as a shim per session, so roughly a quarter of any size
increase becomes resident memory per session. At around 20 sessions, linking gRPC unconditionally would
cost 70-90 MB resident for a transport only the browser uses. That is why the doc's conclusion is that it
should be a *build-time* choice rather than a runtime flag.

Two ways in, and the second is probably right. Switch the whole API to Connect, which is one contract and
one codegen path but makes every local invocation pay for the browser. Or keep ttrpc for the local socket
and put a separate gateway in front, which keeps `cm` small and the shim cheap, at the cost of a second
surface to keep in step with the first. Either way the `.proto` files are the contract, so this is a codegen
change plus adapting call sites rather than a redesign.

**Custom resumption commands.** A session revived after a reboot can re-run its recorded command
(`--on-restore command`), and that command is re-run *verbatim*: the record holds a flat `Command` string
that `strings.Fields` splits back into an argv. For a shell that is right. For anything that has its own
notion of a session it is wrong in a specific way -- what should come back is not `claude` but
`claude --resume <that conversation>`, and cm has nowhere to put the id.

So this needs two things, and they are separable. First, a way for a program to tell cm something to
remember about itself, which is the same shape as `cm report` but persisted rather than describing a live
state. Second, a restore command that can refer to it, which means either a template
(`claude --resume {{.session_id}}`) or storing the full argv to re-run instead of the one that was started.

The first half now exists: tags are persisted free-form key/values, and a program can set one on itself
with `cm tag cm.dev/session-id=abc123` from a hook. That was a reason to make tags key/value rather than
bare labels, so this needs no `cm annotate` and no new column. What is left is the second half, the
templating, plus deciding how a restore command refers to a tag and what happens when the tag it names is
absent. Note the character set: a tag value allows only letters, digits, `-`, `_`, `.`, and `/`, which
covers a uuid but not an arbitrary opaque token, so a program with a rich id may need to be told to hand
over something narrower.

Worth noting what it unlocks, because it is more than convenience: an agent that survives a reboot with its
conversation intact is a different thing from one that comes back empty in the right directory. It is also
the first feature that would have cm store something a program asked it to remember, which is a small but
real widening of what cm claims to know.

**`cm attach --remote ssh://user@host`.** Run the client locally against a server on another machine, so
local terminal features -- the clipboard, notifications, the emulator's own keybindings -- keep working
while the session lives remotely. `ssh host cm attach` already covers the plain case and is why this has not
been needed, but everything in that session belongs to the remote terminal.

The transport is not the hard part: ttrpc's `Serve` takes any `net.Listener`, and tunnelling the socket over
ssh needs no protocol change. Authentication is: `docs/rpc.md` records that remote access would mean
building auth rather than inheriting gRPC's credential ecosystem, and names this as the decision most likely
to be revisited. Tunnelling over ssh sidesteps that entirely by borrowing ssh's authentication, which is
what makes the `ssh://` form the version worth building -- it is a client-side convenience over a tunnel
rather than a network service.

The parts that genuinely need thought are elsewhere: which end resolves `--dir` and the session's
environment, what a dropped link does to the resume loop that already handles a server restart, and whether
`cm list` shows local and remote sessions in one table or keeps them apart.

**A kitty wrapper for spawning windows.** A `cmk` or `kitten cm` that creates a kitty window or tab with a
cm session already in it, so an agent could give each sub-agent its own visible tab instead of a headless
session. Mechanically this is cheap: kitty's remote control can launch a window running any command, and
this machine already has `allow_remote_control socket-only` with a `listen_on` socket, so no new plumbing is
needed.

It is listed here rather than under the known gaps because the idea is not settled, which matches how it was
raised. Two questions decide it. Does it belong in cm at all, given that windows and tabs are explicitly the
emulator's job -- a wrapper that only *asks kitty* to open a window is arguably on the right side of that
line, but it is the first thing in cm that would know kitty exists. And is a visible tab per sub-agent
actually wanted? Twenty background sessions are fine; twenty tabs appearing unbidden is not, so this
probably needs to be something a person invokes rather than something an agent does on its own.

The cheap version needs nothing from cm and is a dotfiles function:

```
kitten @ launch --type=tab --title "cm: NAME" cm attach NAME
```

Verified in a kitty sandbox: the tab opens with the session attached, and `cm list` reports `clients 1`, so
the client really is running in there. Note `cm attach` rather than `--no-attach` -- the point is a tab you can
watch and type in, and `--no-attach` would create the session and exit, leaving an empty tab. That is where to
start, and it would answer the "is this wanted" question before any code lands here.

**Mapping sessions to terminal windows and tabs.** Deferred, but the measurements are recorded because they
are what decides it and they were not cheap to get. The goal was to replace a `zmx-map` script, which shows
which kitty tab and window each session is in, with something native.

The two directions are not interchangeable, and each question needs a different one.

*cm identity outward, to the window.* A program inside a terminal can push a variable out to kitty with
`OSC 1337;SetUserVar=key=<base64>`. Verified through cm's own pty with a client running in a real kitty
window: the sequence passes through untouched and lands on the window, and `kitten @ ls` then reports
`user_vars={'cm_session': 'passthru'}`. kitty can also match on it (`--match var:`, `--when-focus-on var:`).
This is the half that works, and it would make a window self-describing, so "focus the window showing session
X" needs no bookkeeping anywhere. An unrecognized OSC is discarded by any conforming terminal, the same
property that makes cm's own OSC 25453 safe to emit unconditionally.

*kitty layout inward, to `cm list`.* This is the half that does not work, and the obstacle is kitty's event
model rather than anything in cm. Window ids are already available and stable: `KITTY_WINDOW_ID` is in the
captured client environment, re-recorded on every attach, and unaffected by tab close or reorder. Tab
*indices* are the problem. Measured with a probe watcher logging all seven events: closing the middle of
three tabs moved a window from tab index 3 to index 2 and fired only `on_close` for the closed window, and
`move_tab_forward` changed every index and fired nothing at all. There is no tab-membership or tab-index
event, so both ways cm could learn a tab number are stale by construction -- env at attach, because tabs move
without an attach, and a watcher-driven `cm tag`, because no event exists to hook. `on_focus_change` could
refresh the focused window's index, which is exactly backwards: the sessions whose location you want from
`cm list` are the ones you are not looking at.

That leaves cm asking kitty at display time, which means cm shelling out to `kitten @ ls` and knowing kitty
exists -- the line this whole entry is about. So a tab number in `cm list` is the one part that should
probably stay refused rather than deferred.

What is worth doing when this is picked up again: emit the user variable on attach, and let the mapping view
live where the layout does. `kitten @ ls` alone then returns tab index, window id, title, and the session
name for every window in one call, always fresh because kitty is authoritative for its own layout. That is
strictly better than `zmx-map`, which infers the session from launch argv and two `lsof` passes and needs a
MISMATCH column because the inference can disagree with itself. It is a small kitten rather than a cm
feature, and the cm side is one sequence.

Already done, and the practical half of this: `cm list` shows the session title in its own column, which is
what a person actually recognizes a window by.

## Persistence and history

**Searching history.** `cm history` prints everything and leaves searching to a pager or `grep`, which is
right for a person and awkward for a script that wants a line number or a match count. Given the sequence
numbers are already there, a search returning positions would compose better.

**Export a session.** A session's content is a log plus a terminal model. Writing it out as a transcript,
or as HTML with styling (which `cm history --format html` already does), covers most of this. What is
missing is a bundle: the content, the recorded command, the directory, the exit status. Useful for filing a
bug or handing a transcript to someone.

**Recovering a session whose shim died.** `store.go` notes that a future version may be able to resurrect
a session from its output log. Today a dead shim means a dead session, and the persisted content is
readable but not revivable. This is a real capability rather than a nicety, and it is bounded work: the
restore path already replays a log into a screen.

## Session environment

**Forward more of the creating client's environment.** Today a session's shell starts from the
*server's* environment, with only two things layered over it: the client's `PATH`, and the
terminal-describing variables from the capture list. Everything else a client exports reaches nothing.
So `cm attach foo` from a shell with `FOO=bar` exported gives a session with no `FOO`, which is not
what "open a session from here" suggests.

The expectation worth naming, because it is what a user has: creating a session by hand should behave
roughly like starting a subshell, so the environment carries over. Creating one from a terminal
emulator's own integration should behave like opening a new split, which is close to fresh apart from
the working directory. Those sound contradictory and are not, and measuring the two kinds of client
is what shows why.

A client spawned by kitty's integration has kitty as its parent and launchd as its grandparent, with
no shell in between: 14 variables and a 4 entry `PATH` on this machine. A client typed into a shell
has that shell's environment, which after a login shell here is 60 variables and a 26 entry `PATH`.
Both numbers come from the *client*, so forwarding the client's environment already produces both
behaviors without a mode flag or a heuristic. The client's own ancestry is the signal.

What this does not settle is what should happen to variables that describe a *process* rather than a
user: `CM_SESSION`, `direnv`'s per-project exports, an agent's session id, credentials exported for
one project. Forwarding them is right for the subshell case and wrong for anything else, and cm cannot
tell the two apart by name. Pruning by name was considered and is not obviously correct: the variables
worth pruning only appear when a user has deliberately started a session from a shell that has them,
which is exactly the case where carrying them over is the point.

There is also a larger version, and it goes the other way: build a session's environment from nothing
rather than forwarding anything, as sshd does. sshd does not run as the user and copies nothing from
any client, building from the passwd entry and a login shell instead, so every session is identically
fresh. macOS `login -fq $USER` does this and works; `TMPDIR` is the one gap, since launchd sets it per
user and a login shell does not build it, and it is recoverable through
`getconf DARWIN_USER_TEMP_DIR` or by letting `mktemp` find the directory itself.

That version is now the less attractive of the two, because it would flatten the distinction above
rather than serve it: a hand-created session would stop resembling a subshell. It also lands
differently per restore mode, since `restore_mode = shell` gets a login shell to rebuild everything
while a session running a bare command has nothing to rebuild with, and `login(1)` brings a setuid
binary into the spawn path with no clean Linux equivalent.

A caution about measuring any of this, learned by getting it wrong here. Numbers taken from an agent's
shell are not numbers about cm. An earlier version of this entry reported a 96 entry `PATH` with 71
mise per tool install directories, and treated that as evidence that inheritance accumulates junk. It
was an artifact of the measuring process: that shell had been started through a mise shim, which sets
`__MISE_SHIM` and expands `PATH` to every tool's install directory. The user's own login shell was 26
entries throughout, and kitty's was 4. Check `__MISE_SHIM` and the process ancestry before believing a
number from an environment nobody chose.

What the neighbours do, which bears on how urgent this is. tmux reaches for the same mechanism, a
capture list plus an explicit `PATH` override at spawn, so the shape is not unusual. It is weaker
support than it first looks, though, because tmux's server is explicit and can be multiplied with
`-L`, while cm's is singular and close to invisible, so tmux is less exposed to the staleness that
motivates this entry and its own comment argues only that a command should be findable.

zmx is the interesting case here, because it is already what this entry proposes. It daemonizes from
the client, so the shell it execs inherits the client's environment wholesale, and the session's
environment simply *is* the creating client's. It needs no capture list and no `PATH` special case to
get there. That is the behavior a subshell-like session wants, reached by process model rather than by
copying, and it is evidence that forwarding a whole environment is liveable rather than reckless.

What zmx gives up for it is the central server, and with it the single place for fanout, bookkeeping,
and terminal state. cm cannot copy the mechanism without copying that trade, so forwarding explicitly
is how cm would reach the same place. See [alternatives.md](alternatives.md).

Two things the prior art settles, both looked up rather than reasoned about.

**Do not write a forwarded environment to disk.** tmux keeps a session's environment on the heap only:
`environ_free` on destroy, and no serialization anywhere in 153 source files. Its reboot-restore
plugins do not save it either. sshd's posture is the same in the other direction, and stricter than
expected: `AcceptEnv` defaults to empty with only `TERM` always accepted, and
`PermitUserEnvironment` defaults to `no`. So nothing comparable persists an environment, which matters
here because cm's session record is plaintext JSON in a file that has been observed at mode 0644, and
`DefaultCapture`'s allow-list exists precisely to keep credentials out of it.

That leaves reboot restore unresolved rather than solved. A revived session starts a fresh shell, and
without a persisted environment that shell gets the server's values for anything outside the
allow-list, which is the original staleness bug surviving in a corner. It is deferred rather than
fixed: persistence is opt-in, and the case is unexercised today. The options were writing the
environment to the record, which the paragraph above argues against, and taking the reviving client's
environment, which is probably right and is a plumbing change once someone needs it.

**A whole-environment forward should still exclude the dynamic linker variables.** `LD_PRELOAD`,
`LD_LIBRARY_PATH`, and the `DYLD_*` equivalents choose what code a process loads rather than how it
behaves, and sshd names exactly this as the reason `PermitUserEnvironment` defaults to `no`, since it
"may enable users to bypass access restrictions in some configurations". cm's client is a local
process already running as the user, so the trust boundary sshd is defending is not present, and the
exclusion is cheap and principled where a broader denylist would be guesswork.

tmux also has a mechanism worth knowing about if cm ever needs the distinction: `ENVIRON_HIDDEN`
marks a variable that lives in the session environment for tmux's own use, and `environ_push` skips
it, so it never reaches a spawned process. cm has no equivalent of "tracked but not handed to the
shell".

## Operational

**Let leadership follow a window across a re-exec.** `resumeState` already carries a returning window's
attach order and `lastInputAt` so it keeps its place and can be recognized as the one in use. Leadership
is not carried: `releaseClientSize` clears the token on detach, so a window that owned sizing comes back
from an upgrade owning nothing, and a resume deliberately acquires none. The gap that leaves is narrow -
a leader whose terminal was resized during the exec gap does not bring the session to its new size, and
waits for the next keystroke or SIGWINCH. Carrying a `leader bool` on `resumeState` would close it, and
the reason to be careful is that leadership is the one piece of this state whose restoration is visible:
restoring it wrongly reflows a window, which is the bug that made a resume stop acquiring sizing at all.

**A supervised server.** The server is started on demand by whichever command needs it. That is what makes
cm need no setup, and it means the server's own lifetime is not managed: a crash is recovered by the next
command, which is fine, but there is no way to say "keep one running". A launchd/systemd unit would suit
someone who wants that, and would change nothing about the on-demand path.

**Upgradable shims, by re-exec rather than fd-passing.** Clients can now be upgraded in place
(`cm clients upgrade`) and the server has always been replaceable, which leaves shims as the one layer that
keeps whatever build it started with for its whole life. A machine with a session per terminal window
accumulates builds visibly: one real install held twelve distinct builds across twenty-six shims, the
oldest ten days old. `cm doctor`'s `shim-version-skew` reports that, and reporting is currently the entire
remedy, because replacing a shim means ending the shell it holds.

The mechanism is `syscall.Exec` in the shim itself, and the parts that sound hardest were measured rather
than assumed. A standalone program that opens a pty, spawns a shell on it, and then re-execs itself:

- The pty master survives, **but only after clearing `FD_CLOEXEC`**, which Go sets by default on a pty
  from `pty.Open`. Verified: without clearing it the fd is gone in the new image, with it the fd is live
  and its window size is intact across the exec.
- The re-execed image **can still reap the shell**, verified by having the child `exit 7` and reading that
  status back with `wait4` after the exec. This is the part most likely to be assumed impossible: it works
  because exec keeps the pid, so the process is still the child's parent.
- The listening socket needs nothing special, for the same reason the pty does not: one process throughout.

Worth recording a false negative from that experiment, because it is the shape of a wrong conclusion that
would have killed the idea. The first run reported the child was never reaped, which read as a kernel
limitation. It was the test's own bug: the shell was blocked writing into a pty nobody was draining, so it
had not exited yet. Draining the pty made it reap correctly on the first try.

**`SCM_RIGHTS` fd-passing is the wrong tool here**, despite being the usual answer to "hand off a
descriptor". It needs two processes, which means the new shim has to bind a socket the old one still holds,
and that contention has already caused a real bug: a replacement shim could not claim the socket while the
previous one held it, and the symptom was misdiagnosed as one session's record clobbering another's, which
produced a fix that silently broke every detached session. The comment at `internal/server/manager.go`
records it. exec has one process from start to finish, so nothing contends for the socket and nothing is
re-bound, which removes the failure rather than handling it.

What actually blocks it is not the fd at all. **A shim owns four things, and only three survive an exec.**
The fourth is the 4 MiB in-memory output log and its sequence numbering, which is ordinary process memory
and is exactly what makes a server restart lossless: adoption replays from the shim's oldest retained
sequence to rebuild the terminal model. Losing it means every server restart after a shim upgrade shows a
blank screen until the shell happens to redraw, which is the bug `docs/architecture.md` describes under
adoption, reintroduced deliberately.

The route through that is not a new serialization format, and most of it already exists. `--persist-path`
writes a shim's log to a file through `seqlog.File`, which on open calls `load()` to recover its contents
and reports its own `Bounds()` and `ReadFrom()`. So an upgrade could persist unconditionally to a temp
file, exec, and reopen it, rather than inventing anything. The sequence numbers must come back identical,
since `last_seq` in the store points into that space, and `seqlog.NewAt` exists precisely to start a log at
a given number.

That makes the shape of the work: persist-then-exec-then-reload, with the fd moved to a known descriptor
and the shell's pid passed on the command line so the new image can `wait4` it. None of the four pieces
needs a new mechanism, which is the argument for it being bounded, and the reason it is still not free is
below.

Two reasons this is recorded rather than built. It puts a self-rewriting code path in the one process that
must not fail: if the exec fails after the pty has been moved, the shell is unreachable, and exec failing
is not hypothetical here, since replacing a binary under a running process gets later invocations SIGKILLed
on macOS. And the payoff is currently theoretical: the shim protocol has been stable since v0.1.2, with no
changes to `shim.proto` in that range other than adding the version field the doctor check reads, so the
skew it would fix is real but so far harmless. The order to do these in is the order of what breaks: this
becomes worth building the first time a shim-protocol change makes old shims actually misbehave.

**Resource limits per session.** Scrollback is bounded per session by configuration, and a session that
produces output faster than anything reads it is bounded by the log. There is no ceiling on session *count*,
so a runaway script can create ptys until the system's limit, which `cm doctor`'s `pty-pressure` check
reports after the fact. A configured maximum would fail the create instead.

**A cm that listens on the network.** Distinct from the `ssh://` client above, and much larger: cm talks over
a unix socket deliberately, which removes authentication, transport security, and version negotiation from
the design. Serving to the network puts all three back. The `ssh://` form exists precisely to avoid this by
borrowing ssh's authentication, so this entry is here mainly to record that the two are not the same idea and
that only one of them is small.

## Deliberately not doing

Kept here so the reasoning is not re-litigated.

**Windows, tabs, splits, layouts.** The terminal emulator does these, and doing them too is how a
multiplexer becomes the thing you fight. This is the founding constraint, not a deferral.

**Detecting what is running.** cm could scrape a session's screen to work out whether an agent is waiting
for approval. It deliberately does not: that means knowing every program's UI and chasing it as it changes.
The report mechanism exists so the program can say instead, and a program describing itself is better
evidence than a screenshot of it.

What this does *not* refuse, since the wording has read as broader than intended: reporting that the screen
changed, or stopped changing, needs no knowledge of any program and is not covered here. The line is
between structuring a screen and interpreting one. See the resumption-mark entry above, which is on the
allowed side of it.

**Rolling an upgrade back to the previous binary.** Tempting because a running process keeps its
executable's inode alive after the file is replaced, so the old bytes are still there. Nothing can name
them on darwin, measured against a running server whose binary was replaced by rename the way
`mise run install` does it: `/proc/PID/exe` does not exist, `/.vol/<dev>/<ino>` resolves an inode by number
only while it is linked and returns `ENOENT` once the rename unlinks it, and `lsof` reports the path, which
by then names the *new* binary. A backup taken from what lsof reports would copy the broken build while
looking correct.

Stashing a copy before the replacement would work and is not worth it. It would have to live in the
installer or in a post-upgrade copy of about 23 MB, and it still could not be used in the case an upgrade is
most likely to fail in: a new server that migrates the schema and then fails cannot be rolled back to,
because an older build refuses a migrated database, and restoring the pre-migration snapshot is ruled out
above for stranding every session created since.

What is done instead is cheaper and covers the reason this came up. An unknown config setting is a warning
rather than fatal, and `cm upgrade` reads the config before stopping anything, so the failure that produced
this idea cannot recur.

**Deliberately not doing: a stable client id.** A client's place in the attach order survives a dropped
stream, keyed on its process id, which is what stops a repaint or an outage moving who sizes the session.

This entry used to propose an identity that outlived the process, on the reasoning that a pid cannot follow
`cm clients upgrade` because the client re-execs. That reasoning is wrong, and it is recorded here because it
was convincing enough to design a feature around, build, and only then measure. **exec keeps the process id.**
That is not incidental: it is the whole reason `reexecForUpgrade` uses exec rather than spawning a child, and
the comment there says so. Measured on a forced upgrade, the returning client had the same pid and its order
restored to 1.

What a pid genuinely does not survive is reuse by an unrelated process after this client exited. An entry is
consumed on first use and there are at most `maxResumeOrders` of them, so the window is small and the
consequence is only a place in the attach order. That is a hypothetical, and the bar here is a real incident.

If an identity outliving a process is ever wanted for something else, the shape is a `client_id` on `Open`
with the pid as the fallback, optional in both directions the way the shim argv is. It should not be added for
this.

**Configuration per session in a file.** Sessions are created by whatever starts them, with flags. A file
mapping session names to settings would move that decision away from the caller who has the context, and
`--env`, `--dir`, and `--on-restore` already cover it. The one exception already exists, because it has to:
persistence by name pattern, since a session's own creator cannot know it will be wanted after a reboot.

**Open: one oversized command yields an empty restore.** `maxHeld` in `internal/graphics/scan.go` bounds an
unfinished command at 1 MiB on the reasoning that "what has to fit is one command, not a whole image". A
program that sends a whole image in a single command exceeds that, and the result is worse than a missed
interception: the restore came back 35 bytes, with neither the image nor the screen text. kitty's own clients
chunk at 4096 so this is not what `icat` does, but nothing stops a program doing it, and the failure is
silent.
