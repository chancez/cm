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

**Alternate-screen scrollback.** A full-screen program draws on the alternate screen, and lines that
scroll off there are gone: `cm read --lines` cannot recover them, because they never entered scrollback.
This is correct terminal behavior and also the single most confusing limitation in practice, because the
symptom is a reply that looks truncated for no reason. The `cm` skill tells an agent to fall back to
writing a file. Whether cm could retain more is a libghostty question, not a cm one, and worth asking
upstream before designing anything.

**`cm doctor` checks as incidents arise.** The standing rule is that a debugging session that cost real
time should leave a check behind. The obvious candidate -- a session that reports neither OSC 133 nor its own
state, so `--wait idle` hangs -- is already covered by `no-shell-integration`, which checks both. What is not
covered: a shim whose persisted log has stopped growing while its session is live, which is a silent
downgrade the shim logs and nobody reads; and a session whose reported state has been `busy` for
implausibly long, which usually means a reporter crashed between its start and end report and left the
session permanently un-waitable.

**Session rename.** A name is chosen when a session is created and cannot change, so a session that turns
out to be something else keeps a misleading name for its lifetime. The store keys on the name and the shim
socket is derived from it, so a rename is either a store migration plus a socket move, or a display name
kept separate from the identity. The second is much cheaper and probably right.

Less pressing now that tags exist, since a mislabelled session can be retagged without being renamed. What
remains is only the display: `cm attach` and every other command still take the original name.

## Driving sessions programmatically

The part of cm that gets used from scripts and agents, and where the last few real bugs were.

**`cm exec`, a one-shot with clean output.** `cm run` on a reused session sends input to a shell, so the
output contains the shell's echo of the command and the prompt around it. That is honest -- it is what the
session printed -- and it is not what a caller parsing the result wants. A form that ran the command
without a shell echoing it, or that stripped the echoed line and prompt, would remove the most common
post-processing step. The catch is that "the command's own output" is not well defined once a shell is
involved, which is why `cm run` reports what it does today.

**Structured output boundaries.** A caller reading a session cannot tell where one command's output ends
and the next begins, so it guesses with `--lines` or a marker in the command. cm already knows: OSC 133
brackets every command, and the sequence numbers are recorded. Exposing "the output of the last command"
or "output since sequence N" would replace the guessing. This is the highest-value idea in this file and
the one most likely to be built next.

**Waiting on more than state.** `cm wait` takes a state. A caller that wants "wait until this text
appears" writes a polling loop around `cm read`, which is exactly the sampling the server-side wait exists
to avoid, and it can miss output that scrolls past between samples. A `--match` or `--regex` on the server
side would be a small addition to the existing wait machinery.

**Waiting on several sessions.** Done: `cm wait --tag` waits on a group concurrently, requiring every
session by default and returning on the first with `--any`. See `docs/architecture.md`.

What is still open is waiting on an arbitrary *list* of names rather than a group that shares a tag. Tags
covered the case that motivated this, since a fan-out is usually created together and can be labelled as it
is, and a caller with a list of unrelated names can still background one wait each. Worth revisiting only if
something needs to wait on sessions it did not create and cannot tag.

**Idempotent send.** `cm send` writes to a pty; if the call fails partway there is no way to know how much
arrived, and no way to retry safely. This has not bitten anything yet, and would matter for a caller
driving cm over a flaky link.

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

## Operational

**A supervised server.** The server is started on demand by whichever command needs it. That is what makes
cm need no setup, and it means the server's own lifetime is not managed: a crash is recovered by the next
command, which is fine, but there is no way to say "keep one running". A launchd/systemd unit would suit
someone who wants that, and would change nothing about the on-demand path.

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

**Configuration per session in a file.** Sessions are created by whatever starts them, with flags. A file
mapping session names to settings would move that decision away from the caller who has the context, and
`--env`, `--dir`, and `--on-restore` already cover it. The one exception already exists, because it has to:
persistence by name pattern, since a session's own creator cannot know it will be wanted after a reboot.
