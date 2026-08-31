# The session picker

`cm tui` lists sessions, attaches to one, and gets the terminal back when it detaches. For a window
that has no session yet: `cm ls` says what exists and `cm attach` goes somewhere, and this is the part
between them.

## Two writers, one terminal

cm's hardest rule is one writer per shared byte stream, and a bubbletea program is a second writer on
the terminal a client paints sessions to. They are separated in time, not merged.

`tea.Exec` is the mechanism. Given an `ExecCommand`, bubbletea stops its input reader, stops its
renderer, restores the terminal, runs the command to completion, and resumes. So while the list is up
`internal/tui` owns the terminal and no client holds it; while an attachment is live, the reverse.

This is why the picker is in `internal/tui` and not `cmd/cm`:
`TestCommandLayerWritesNoEscapeSequences` fails on an escape literal in `cmd/cm`, and the invariant
behind it is that only a package that knows whether the stream is mid-sequence writes to a terminal.

## The attachment is a child process, and that is not a style choice

The first version called `client.Attach` in-process, which is tidier: it returns a `Result` on detach,
so the loop needed no subprocess. It was also broken, and only a real terminal showed it.

**Measured:** after the first detach, exactly one keystroke disappeared per attachment. Attach, detach,
then a single `/` did nothing, while every key after it worked. A control picker that never attached
gave a clean 1,0,1,0 on four help toggles; the same picker after one attachment lost the next key and
then behaved.

The cause is documented in the code that causes it. `internal/client.readInput` leaves its reader
blocked in the kernel on purpose, because a blocked read cannot be cancelled, and `cm attach` gets away
with it by exiting immediately afterwards. A picker does not exit, so the leftover reader sits on the
terminal beside bubbletea's; it wins one keystroke, discards it, and parks forever on its channel send.

Closing the descriptor does not fix it, which was the second attempt: the attachment was given its own
`/dev/tty` rather than `os.Stdin`, on the theory that closing a file unblocks a read on it. Go defers
the real `close(2)` on a descriptor with an outstanding read until that read finishes, so the read stays
satisfiable and still eats a key. Same symptom, measured again.

A child process takes its leftovers with it when it exits. So `cm tui` runs `cm attach <ref>` with this
process's stdin and stdout, which bubbletea has already released, and the child finds the terminal
exactly as a shell would hand it over.

Two consequences, both improvements:

- **stderr is captured** rather than left on the terminal, because `cm attach` reports how the
  attachment ended and those bytes would land on a frame bubbletea is about to repaint. The captured
  text becomes the status line verbatim, so the wording lives in one place.
- **Upgrades work.** `cm upgrade` re-execs a client, and a client that is its own process can be
  replaced without touching the picker. The in-process version could not have had this: exec would have
  replaced the list.

The cost is a process per attachment, about 23ms, paid once when you press enter.

The alternative worth naming: give `internal/client` a cancellable input reader, which would fix the
leak at its source and let the picker attach in-process. That changes the input path of every client for
the benefit of one caller, so it is not done here.

## The output pane

Under the list is the selected session's last output, which is what a session is actually recognisable
by: half of them are named by the server, so the name says nothing, and several usually share a
directory. `p` closes it, `--preview=false` starts without it, and closing it stops the reads.

**Content comes from Read's plain form**, which renders the terminal model's cells to text. The raw form
would carry the program's own escape sequences into a frame bubbletea composes, which is cm's largest
family of bugs. Measured against a session that printed SGR, a CR overwrite, a tab, a BEL and a NUL:
`cm read` returned clean text, with the overwrite and the tab already resolved by the model.

It is sanitized anyway. `internal/ansi.Strip` removes escapes, CR, BS and BEL; the pane also drops the
rest of C0 and expands tabs, since a tab moves the cursor to a stop the width accounting cannot see and
shifts everything after it. Belt and braces on purpose: "the plain form should already be clean" is what
the other instances of this bug also had.

**The pane below the list, not beside it.** A session's output is lines written for a terminal's full
width, and a narrow column beside the list would truncate all of them. The list's rows are wide too.
Lines are truncated rather than wrapped, since a wrapped line takes a row the pane counted on.

**A late answer is discarded.** A read is in flight while the cursor moves, so its answer can describe a
session that is no longer selected. Painting it is worse than an empty pane: the content is real, it is
just an answer to a question nobody asked. The pane holds *which* session it is waiting for and drops
anything else, clears its lines when the cursor moves, and labels itself with the session it is waiting
for rather than with the selection.

**The list is sized to its rows, and the pane takes the rest.** bubbles pads a list to whatever height
it is told, so three sessions on a tall window left ten blank lines above the pane. What the list spends
on its own title and count is asked of its paginator rather than assumed: the first version guessed
four, and a wrong guess there does not look like a bad constant, it looks like a list showing one
session out of three.

**Cost, measured.** One extra Read per second, on the connection the picker already holds. Server CPU
over 30 seconds while previewing a session printing a line a second: 0.16s with the pane open, 0.16s
with it closed, so it is below the noise of feeding that session at all. `cm read --lines 20` costs
about 3ms of work on top of a bare invocation's 23ms, and does not grow with a session's output because
the model retains a bounded scrollback.

## What it does not do

**Switching.** Enter always attaches. Run inside a session it therefore nests, and the detach key
belongs to the outermost client, so a detach lands somewhere unexpected. Said in the status line rather
than refused, since nesting works and is occasionally wanted. `cm switch` is the command for a window
already on a session.

**Colour in the pane.** The plain form of Read drops attributes along with everything else, so the
output is monochrome. Keeping SGR and nothing else would mean parsing the raw form and re-emitting only
the sequences that cannot move a cursor, which is a filter that has to be right rather than nearly
right. Not attempted yet.

## Choices worth knowing

**Attaching by ID, never by name.** A row is drawn from a list up to a second old, and a name is a
binding that `cm bind` or `cm switch` can move in that window. Worse, `cm attach` creates a session for
a name that holds nothing, so a name unbound since the refresh would silently make a new shell.
`Manager.Open` refuses a stale ID rather than creating for it, so an ID either finds the session that
was on screen or fails.

**A confirmation captures its target.** The list refreshes while the question is on screen. When the
selected session ends on its own there is nothing to put the cursor back on, so it stays where it is
and a different session is under it: a kill that re-read the selection at `y` would end that one.

**The cursor tracks a session, not a row.** Restoring by index moves the selection whenever anything
older ends, which under a one second poll means it walks away while being aimed at.

**The pane's own reads stop when it closes**, so the default picker costs what it did before it existed.

**Filter state is checked before the bindings.** The actions are single letters and the filter is a text
field, so bindings checked first make typing a name run commands: `n` creates a session, `x` offers to
kill one. This one bit in the sandbox before the test existed. `ctrl-u` is the same trap from the other
side: it is delete-to-start in a text field, so a filter being typed keeps it.

**`ctrl-u` and `ctrl-d` move half a page**, as in vim and less. The list already turns whole pages on
`u`, `d`, `f` and `b` from its own KeyMap, so the control pair is what was missing. Half of the list's
`PerPage` rather than half the window, since the pane and the expanded help both take rows off the list.
The cursor is stepped a row at a time rather than dropped on an index, because the list is paginated
rather than scrolled: `CursorDown` turns the page when it runs off the end of one, and setting an index
would leave the paginator behind. Neither key wraps, so a key pressed to move the selection a little
cannot move it to the far end of the list, where the next `enter` or `x` would act on it.

They are described in the expanded help rather than on the short line, and inside the list's own
navigation column rather than in a column of their own. Neither help line is truncated to the window,
and the layout measures the footer by counting newlines, so anything that overflows either costs the
list a row or is silently cut off the right edge. Measured in a 100 column terminal: the expanded help
reached column 89 before these keys, a column of its own took it to 118 and cut the filter and quit
columns off, and the navigation column absorbs them for nothing.

**The child's argv carries this picker's directories.** A sandboxed picker whose child resolved its own
would attach to the developer's real sessions while looking isolated.

Each has a test in `internal/tui/model_test.go`, `internal/tui/preview_test.go` or
`cmd/cm/tui_test.go`, and each test was confirmed to fail with the behavior mutated out. Two mutations
were inconclusive first time and are worth knowing about: removing either of the two guards that stop
reads while the pane is closed changes nothing, because they are redundant, and a mutation that removed
the sanitizer left an unused import so it never compiled, which `go test` reports as a failure and looks
like success.

An action's error is held apart from the poll's. Every action asks for a refresh immediately afterwards,
and while one field served both, a successful refresh cleared the action's error microseconds after it
was set: a failed kill reported nothing at all. Found by a test harness faithful enough to run the
refresh the runtime would have run.

## Cost, measured

bubbletea v2.0.9 with bubbles v2.2.1 and lipgloss v2.0.6 takes `bin/cm` from 23.61 MB to 26.52 MB,
**+2.91 MB, 12.3%**. That matters more than for an ordinary CLI because the binary re-execs itself per
session, so every shim carries it, but it is shared read-only text rather than per-process memory. For
scale, `docs/rpc.md` weighs linking gRPC at a 34 MB increase.

v2 over v1, which measured 1.2 MB smaller in a standalone probe: v2's input layer understands the kitty
keyboard protocol, and the picker is a terminal program that will itself be run inside multiplexers. The
v2 modules are at `charm.land/*` rather than `github.com/charmbracelet/*`.

**The list polls.** The server has no watch RPC, so `List` runs once a second on the picker's one
long-lived connection, chained off each answer so there is never more than one in flight. A request on
an open connection is nothing like the 23ms a fresh `cm` invocation costs. A subscription would be
better and is a server change, not a picker change.

## Naming

`cm tui` describes the implementation rather than the purpose, which is worth revisiting. Bare `cm`
prints help today and could open the picker instead once this has earned it.
