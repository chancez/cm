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

## What it does not do

**Switching.** Enter always attaches. Run inside a session it therefore nests, and the detach key
belongs to the outermost client, so a detach lands somewhere unexpected. Said in the status line rather
than refused, since nesting works and is occasionally wanted. `cm switch` is the command for a window
already on a session.

**A preview pane.** The selected session's screen beside the list needs `internal/vt` rendering into a
viewport, and is where the escape-sequence traps in `docs/testing.md` live. Deferred deliberately.

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

**Filter state is checked before the bindings.** The actions are single letters and the filter is a text
field, so bindings checked first make typing a name run commands: `n` creates a session, `x` offers to
kill one. This one bit in the sandbox before the test existed.

**The child's argv carries this picker's directories.** A sandboxed picker whose child resolved its own
would attach to the developer's real sessions while looking isolated.

Each has a test in `internal/tui/model_test.go` or `cmd/cm/tui_test.go`, and each test was confirmed to
fail with the behavior mutated out.

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
