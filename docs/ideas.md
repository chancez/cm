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

**Session groups or tags.** With one session per terminal window, `cm list` grows with the day. Names
carry the meaning today (`review-api`, `build`), which works and is why this has not been urgent. A
`--tag` or a naming convention with prefix filters would make "everything for this project" addressable.
The cheap version already exists: `cm list --prefix`.

**Session rename.** A name is chosen when a session is created and cannot change, so a session that turns
out to be something else keeps a misleading name for its lifetime. The store keys on the name and the shim
socket is derived from it, so a rename is either a store migration plus a socket move, or a display name
kept separate from the identity. The second is much cheaper and probably right.

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

**Waiting on several sessions.** Fanning out to N agents means N waits, which shell-backgrounding handles
adequately (see `skills/cm/SKILL.md`). A `cm wait --any` or `--all` over a list would make the common
orchestration a single call, and would let a caller react to whichever finished first rather than polling.

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

**Remote sessions.** cm talks over a unix socket, deliberately: everything is local, which removes
authentication, transport security, and version negotiation from the design. Attaching to a session on
another machine is the single largest possible expansion and would touch every layer. Worth writing down
mainly to note that it is not a small change, and that `ssh host cm attach` already covers the common case
without cm knowing anything about the network.

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
