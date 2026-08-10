# Testing terminal behavior

cm sits between a program that speaks escape sequences and a terminal that answers them. Most of its
bugs live in that conversation, and the conversation is the part hardest to observe: a wrong result
usually looks like a clean pass rather than a failure.

Everything here is a lesson from a specific incident. The two that produced most of it:

- A DA1 reply printed beside a zsh prompt as `62;52;c` after quitting vim.
- `;rgb:2828/2c2c/3434` and a wedged prompt after `wallfacer -h`.

Both were the same defect, cm answering a terminal query while a real terminal was also answering,
and both took far longer to find than they should have. What follows is what would have found them
faster.

## Pick the right control

The single most expensive mistake in the incidents above: comparing cm against **bare kitty** and
concluding "not a cm bug" when `wallfacer -h` was clean without cm.

That control was wrong. Bare kitty has no multiplexer in the path at all, so it cannot distinguish
"cm is innocent" from "cm does something a multiplexer should not". The right control is **another
multiplexer**: zmx, in the same terminal, with the same shell and the same command. Run that and the
asymmetry was immediate, cm leaked 2 of 3 runs and zmx 0 of 3.

The rule: a control has to differ from the failing case in **one** thing, and that thing should be
the code under suspicion. If the control removes the entire layer, a pass tells you nothing about the
layer.

A throwaway terminal can host both at once, one window each, with the identical command driven into
both and the outputs compared.

## Test the states, not just the happy path

Client count is a state variable that changes behavior, and it is easy to test exactly one value of
it. Every change touching what reaches the pty or the client needs all of these:

| clients | why it differs |
| --- | --- |
| 0 | `cm run`, a detached session. cm is the only possible answerer, so a mistake here is a **hang**, not an artifact. |
| 1 interactive | The normal case. The terminal answers, so cm must not. |
| 1 read-only | `cm read --follow`, `cm attach --read-only`. Input is **dropped** (see `recvLoop`), so this terminal cannot answer and cm still must. |
| many | Every client sees the query, so N terminals answer it N times. |

The read-only row is the one that catches a plausible-looking wrong fix. "Is a client attached"
(`Clients() > 0`) reads correctly and is wrong, because it counts followers; the predicate has to be
"is a client attached that can answer". Mutating to `Clients() > 0` is a good check that a test suite
actually distinguishes them.

`readOnly` is also known at two different times: `registerClientSize` learns it, but a **resuming**
client skips that call entirely. A test that only ever attaches fresh will not notice.

The many-clients row was a real bug, found only once it was measured: two kitty windows on one
session answered a single `CSI c` twice, as `\x1b[?62;52;c\x1b[?62;52;c`. It had been reasoned about
and left untested through two rounds of fixes for the same family of bug, which is the argument for
walking the table rather than testing the state in front of you.

The narrow-versus-broad distinction there is worth copying. Dropping *every* non-typing input from
the non-answering clients fixes the duplicate and breaks the mouse, and it passes any test that only
counts replies. When a fix restricts something, test what must still get through.

## Drive the pty directly, not a real program

It is tempting to test by running the program that exposed the bug. Do not, unless the bug is
genuinely about that program.

`printf 'A\033[6nB'` in a `/bin/sh -c` is enough to make a session emit a cursor position request.
That is a complete reproduction of the trigger, with no dependency on vim's or wallfacer's startup
probes, which can change and silently take the test's meaning with them.

Two further reasons from experience. A test that shells out to `wallfacer` stops testing anything the
day wallfacer stops querying OSC 11. And a real program's behavior is confounded: `wallfacer -h`
happened not to send DA1 at all, so one early "verification" of the vim fix exercised nothing while
appearing to pass.

Use a real program to *find* a bug and to confirm the fix in a terminal at the end. Use the pty to
*test* it.

## Send valid sequences, and know which form you are matching

Three encoding traps, each of which has produced a test that passed while asserting nothing:

**Go escapes in shell strings.** `printf 'X\033[cY'` inside backticks sends real bytes because the
shell expands `\033`. A Go string `"\\x1b[c"` in a needle is a literal backslash-x and matches
nothing, ever.

**Caret notation on echo.** A pty in its default mode has `echoctl`, so a control character written
to it comes back rendered: `\x1b[2;1R` echoes as `^[[2;1R`. A test asserting the *absence* of the raw
form passes unconditionally. `internal/server/query_test.go` names the two forms separately for this
reason.

**Replies are not requests.** `CSI c` is a query; `CSI ? 62 ; 22 c` is its answer, and both reach
output because a shell echoes a reply at the prompt. Matching a whole family by final byte will strip
or misclassify real output. libghostty additionally answers its own DA2 and kitty-keyboard *replies*
with identical bytes, so those are fixpoints; see `internal/vt/query_test.go`.

Sequences worth covering when touching query handling, because they behave differently: DA1/DA2/DA3
(`CSI c`, `CSI > c`, `CSI = c`), DSR (`CSI 5n`, `CSI 6n`), XTVERSION (`CSI > q`), the kitty keyboard
query (`CSI ? u`), DECRQM (`CSI ? ... $p`), DECID (`ESC Z`), and, on the other side of the line, the
ones only a real terminal can answer: OSC 10/11 colors, OSC 52 clipboard, XTGETTCAP, XTWINOPS
(`CSI 14t`, `CSI 18t`). Getting the second group wrong turns an artifact into a hang.

## `cm read --raw` is not the byte stream

This cost several wrong conclusions in a row, so it is worth stating flatly.

`cm read --raw` calls `ReadVT`, which **re-serializes the terminal model**. It is what the screen
renders to in VT form, not what the program emitted. A session whose log genuinely contained OSC 11
and OSC 133 showed **zero** OSC sequences under `--raw`, which looked like proof that cm had eaten
them.

The tell: prompt markers are always present in a real interactive session's output. If `--raw` shows
none, you are reading a rendering.

To see actual bytes:

- `cm read --raw --follow` writes what arrives to stdout unfiltered. Redirect it to a file.
- Instrument the code and log `%q` at the hop you care about. This is what finally found the
  injection: logging every byte `drainPending` wrote produced `\x1b[2;1R` immediately.

Prefer instrumentation over inference. Several hours went into measuring latency, fragmentation,
termios, and environment, all of which came back identical to zmx, because the corrupting byte was the
answer to a *different query* than the one being measured. One log line at the right place beat all
of it.

## Prove the harness can see the failure

A negative result from an unverified harness is worthless, and terminal features negotiate, so a
harness that does not answer queries will make a program skip the mode whose bug you are chasing.

Before believing "it does not reproduce":

- Add a control that must fail and confirm it does. Injecting a bare reply at an idle prompt
  reproduces the garbage in any terminal, so it proves the observation works.
- Check the precondition actually held. One vim run emitted no DA1 whatsoever, so that run said
  nothing about a DA1 fix either way.
- Beware `script(1)` and `send-text`: neither answers queries, so a mode that needs an answer never
  turns on.

## Real-terminal checks that are worth the trouble

Unit tests at the seam cover the decision; a real terminal covers the wiring. Both, for anything
touching escape sequences.

Use a throwaway terminal instance, never the developer's own. Watch for these, all of which have
bitten:

- **Confirm config isolation, do not assume it.** An *empty* `CM_CONFIG` means **unset** to cm, so it
  falls through to the developer's real config file. A sandbox wrapper did exactly that until it was
  caught here, and nothing failed because nothing asserted on it. Point `CM_CONFIG` at a nonexistent
  file, and check anyway: `cm config` inside the sandbox must show `detach_key ctrl-\` and not the
  developer's setting.
- **`CM_SESSION` is inherited**, so a bare `cm attach` in a sandbox window retargets the session that
  launched it instead of creating one. The symptom is `clients=2` and `state=running(cm)`. Unset it in
  the wrapper.
- **A wedged prompt eats subsequent commands.** This bug leaves zsh in vi command mode, and every
  later `text` call is absorbed into that buffer, so a test file never appears and the result looks
  like a crash. The starship prompt character flips to its vi-command-mode form. Recover with ESC then ctrl-c, or start a fresh
  session; note that input sent immediately after recovery tends to lose its first characters.
- **A TUI owns the terminal.** While vim or a full-screen program runs, shell hooks do not fire and
  titles come from the program. That is correct, not a bug.

## Run the tests you think you are running

`go test ./...` runs e2e; `go test -short ./...` skips it (`skipIfShort`). A stale e2e test asserting
a design that has since been replaced will sit green under `-short` indefinitely. That happened in
this work: the first fix's e2e test still asserted its approach after the second fix inverted it, and
only a pre-merge full run caught it.

Before believing a change is done: `go test ./...`, then `-race` on `./internal/... ./cmd/...`, then
`mise run test-linux` if anything platform, shell, or path related moved. And when a change replaces
an approach rather than extending it, reread the tests written for the old one; they do not fail just
because they are now wrong about the design.
