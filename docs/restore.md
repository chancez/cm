# Restoring a screen on reattach

Reattaching repaints the screen and puts the cursor where the shell left it, so the session
looks as though nothing happened. This is done by serializing terminal state into escape
sequences and replaying them, which is a port of zmx's `serializeTerminalState`
(`zmx/src/util.zig`).

Almost every detail below is a bug someone already hit. The obvious implementation looks
correct and is wrong in ways that only appear in specific circumstances, so the reasoning is
recorded rather than just the result.

## Two phases

Scrollback is emitted first as bare content, then the viewport is cleared, then the viewport is
emitted along with terminal state.

A single pass that emits everything with a cursor position leaves the cursor wrong: the
scrollback lines shift where the viewport begins. This is zmx issue 31.

## Boundaries

Scrollback ends on the row *above* the active area, at that row's **last** column. Two details,
each load-bearing:

- A linear selection ending at column 0 covers only that single cell, so the rest of the final
  row is dropped.
- Ending at the active area's first row rather than above it duplicates a row the viewport
  phase emits again.

## Clearing the viewport scrolls, it does not erase

Between the phases the viewport is scrolled away with SU (`\x1b[<rows>S`), not erased with ED
(`\x1b[2J`).

ED is the obvious choice and it destroys content. After emitting scrollback, the rows still
visible *are* the tail of that scrollback, so erasing them in place removes real lines. This is
invisible when the terminal is tall relative to the content and loses lines when it is short,
which is why the regression test uses a 5-row terminal with 20 lines.

No explicit newline is needed before the scroll. The formatter puts `\r\n` between lines but not
after the last one, so the cursor is still on that line and the scroll moves it along with
everything else. Adding a newline as well leaves a blank row at the boundary.

## Resize before snapshotting

The terminal model is resized to the attaching client's size *before* the snapshot is taken.

Reversed, the snapshot describes lines wrapped for the old width and the client wraps them
again, so the screen arrives mangled. Only visible when a client attaches at a size the session
does not already have.

This has been broken twice, and both times it produced two symptoms that read as unrelated bugs:

- **Rows arrive spliced.** A soft wrap is serialized as a hard `\r\n`, because the formatter has
  no option to preserve wrap state. That is harmless *only* when the model already matches the
  client, which is what this ordering guarantees. Measured against a 60-column session attached
  from a 100-column window: the broken order renders `...for your changes.` and `Lines starting"`
  on separate rows, the correct one renders the line whole.
- **A literal `]2;` on screen.** The resize makes the shell redraw, so anything its SIGWINCH
  handler emits is generated *after* the screen was serialized and interleaves with the replay
  rather than being part of it. A zsh `TRAPWINCH` setting the title is enough: the OSC 2 arrives
  with its `ESC` already consumed by the bytes ahead of it.

The second regression came from the configurable resize policy, which decides with an attach
token and could only get one from `attach`, so the sizing block moved below it. The fix is
`reserveClient`, which hands out a token before attaching; the ordering is not restorable by
moving the call alone.

### The window that fix opens, and what fell into it

Reserving before attaching means there is an interval where a client has a size entry but no
attachment, and the resize deliberately happens inside it. Two things are therefore true at once
in that interval: the shell has just been told to redraw, and the client that caused it has no
output stream and no input channel yet.

Anything the shell asks the terminal in that interval had no answerer. The answerer election counted
the reservation, so cm stayed silent because a client looked present, while the client never saw
the question because it was not subscribed when the query went past. The querying program then
consumed the *next* reply to arrive, which belonged to some later query, and the leftover landed
in zsh's line editor: a branch name from a title report and `;rgb:2828/2c2c/3434` from an OSC 11.
Under vi mode the reply's leading `ESC` also dropped the editor into command mode, so a following
`v` opened the stray text in a scratch buffer.

That was fixed by distinguishing **attached from reserved**, since sizing must count a reservation,
which is the entire reason it exists, while answering must not.

**The election is gone now, and with it this whole failure mode.** cm answers every query its own
model can answer, whatever is attached, and asks a client only for the queries no model can answer.
Nothing about this window can leave a query unanswered any more, because a reservation being
ineligible no longer means silence: cm answers regardless. What remains true, and is still why the
reserve-then-resize ordering exists, is that a reservation has no stream to carry a *proxied*
question, so one is never sent to it. See `docs/architecture.md`.

The test that shipped with the first fix could not catch the second break. It drives
`sess.Resize` and `sess.attach` itself, in the correct order, and asserts on the session, so it
passed while the service did the opposite of what its own comment said. The ordering is only
observable through `Service.Attach`, and the assertion has to be on the size the model held *when
the screen was taken*: a late resize leaves the model looking correct afterwards while the bytes
already captured describe the old width.

## What is deliberately excluded

- **Synchronized output (DECSET 2026)** is suppressed across serialization and restored
  afterwards. It is a handshake between a program and the terminal currently showing it;
  replaying it leaves the new client withholding renders until its own timeout fires, so the
  session looks frozen on attach.
- **Tabstops.** Restoring them emits sequences that move the cursor after it has been
  positioned.
- **The palette.** It belongs to the client's terminal, and replaying it overrides the user's
  theme.

## What has to be added by hand

- **The title**, as OSC 2. libghostty tracks it but its formatter has no title option and never
  emits OSC 0/1/2, so without this a reattaching client shows its own process name. OSC 2 does
  not move the cursor, so appending it after content is safe. (zmx issue 224.)
- **The directory**, as OSC 7, emitted without the NUL sentinel libghostty appends to the
  stored value. Forwarding the NUL is not cosmetic: kitty writes it into its session file and
  then cannot parse its own state back. (zmx issue 222.)
- **Which screen the content belongs to**, when it is the main screen: `?1049l` followed by a
  clear, in front of everything else. See below.

## A main-screen blob has to say so

The formatter emits only modes that *differ* from libghostty's defaults, and the main screen is the
default, so a main-screen blob carried no `?1049l` at all. The alternate-screen direction was
already explicit, because `?1049h` appears in the mode preamble whenever the model is there, so only
one of the two directions said where its content belonged.

The consequence is that cm cannot see the client's terminal, and a repaint assumed the terminal was
wherever the model was. A client whose terminal was on the alternate screen therefore had the
repaint painted onto that screen, over a program's own display, while the real main screen
underneath kept whatever was on it. The next `?1049l` any program sent popped the terminal back to
that stale screen and discarded everything cm had painted. The symptom was quitting nvim and being
left at a prompt with the whole nvim window still above it, which reads as cm failing to clear the
screen: the screen it failed to leave is one cm never knew the client was on.

Two details, both load-bearing:

- **The clear comes after the switch, not before.** A client clears its terminal before writing the
  blob, so that clear lands on whichever screen the terminal is on: the alternate one gets wiped,
  the switch to main follows, and main still holds its stale contents. Measured exactly that way,
  the restored screen came back with the stale line still on top. This mirrors the other direction
  rather than adding anything, since `?1049h` is defined as save cursor, switch, *and* clear. That
  `?1049l` does not clear is the asymmetry that produced the bug.
- **It is unconditional, not gated on whether the session ever used the alternate screen.** "Did
  this session use it" is the wrong question, because the state being corrected belongs to the
  client: a terminal can be left on the alternate screen by a program that ran before cm was in the
  path, and after a server restart the model does not remember either way.

Safe in the common case, measured both ways rather than assumed. Against libghostty the contents are
byte-identical with and without it, and in a real kitty all 60 lines of scrollback survive it with
the visible screen unchanged. It is also prepended *after* the "nothing to restore" check rather
than written into the buffer, since a client clears its terminal whenever the blob is non-empty, and
a blob that is never empty would have a client wiping the window it attaches to.

## Replayed modes belong to the program, not to cm

Serialization sets `modes: true`, so the blob opens with every terminal mode that differs from
libghostty's defaults: the alternate screen, mouse tracking, bracketed paste, and the rest. This is
correct and it is not optional. A full-screen program that is still running enabled those modes and
is waiting on them, and it will not re-enable them because a new client arrived. Filtering them out
gives a reattached TUI a dead mouse, or repaints it onto the main screen and destroys the client's
scrollback.

The consequence worth knowing before diagnosing anything: **that state describes the program, so it
is not a signal about cm.** Whether a session replays `?1049h` and mouse tracking is decided by what
is running in it and how it was started, and two invocations of the same program can differ. Measured
on three sessions running Claude Code: `claude agents` serializes
`?25l ?1000h ?1002h ?1003h ?1004h ?1006h ?1049h ?2004h ?2031h`, while plain `claude` in two separate
sessions serializes `?25l ?1004h ?2004h ?2031h` in both, setting neither the alternate screen nor any
mouse mode. The first grabs the mouse, so the wheel scrolls the program's own history; the second
leaves scrolling to the terminal. Both are right, and neither is cm doing anything.

That difference reads as a cm bug from the outside, because the symptom is "scrolling behaves
differently in this session", and it cost a long debugging session that found three causes in a row
that were not causes. Two mistakes are worth naming, since both look like evidence:

- Mode state is emitted **once, as a preamble**. Grepping the whole of `cm history --format vt` for
  `?1000h` also counts the sequence appearing as literal text anywhere in the scrollback. Diagnosing
  this from inside the affected session is enough to trigger it: the grep reported 19 occurrences of
  `?1000h` in a session whose actual mode state set none, because the investigation had printed the
  sequence into that session's own scrollback. Read the first bytes, not a match count.
- `cm list` showing `running(claude)` is the last command the shell reported via OSC 133, not a live
  process. It says nothing about whether that program is still there.

Three rendering complaints *were* cm, and they are worth knowing so the paragraph above does not read
as "rendering is never cm". Two are a different mechanism from mode replay: not what cm repeats to a
client, but what cm *answers* a program. See "The answers cm does not take from its model" in
`docs/architecture.md`.

The third is mode replay, and it is the exception to "that state describes the program". A blob for a
session on the *main* screen said nothing about the screen at all, so a repaint could paint onto a
client's alternate screen and leave a full-screen program's display behind after it quit. What
distinguishes it from the false leads above is that the missing mode is one no program ever set:
the question is not which modes the program turned on, but whether the blob describes the screen it
belongs to. See "A main-screen blob has to say so" above.

For those two the discriminator is the shape of the window rather than the program. Scrolling both halves
of a split needed a **vertical** split, because a full-width scroll region never routes through margins
at all. A window that keeps its old height needed a split to **close**, because a program that has
stopped listening for resizes looks fine until one happens.

`cm history --format vt` is how to read the state, since it renders through the same formatter with
`modes: true`. Its first bytes are the modes the next fresh client would receive.

## Only a fresh client receives any of this

A resuming client gets no blob at all. `attach` returns serialized state only when `resumeFrom` is
nil; a client that reconnects with a position gets the bytes it missed and nothing else, because it
already has the screen and the modes that came with it.

So a mode that reaches one client need not reach another, and reattaching is not a way to
resynchronize a client whose terminal has drifted from the model. The client's own reset on detach
(`RIS`, `internal/client/terminal.go`) is what cleans a terminal up, and a connection that dies
without detaching skips it.

## Prompt markers

Every chunk of output has `redraw=0` forced into OSC 133;A prompt markers.

`redraw=1` tells the terminal the shell will repaint its own prompt, so the terminal clears
those lines on resize and waits. Through a multiplexer that repaint arrives in the inner pty's
coordinates and the prompt never comes back. (zmx issue 111.)

## Testing

Serialization is verified by round trip: serialize, replay into a fresh terminal, compare
contents line by line. Upstream has no VT round-trip test of its own.

Test on a terminal **shorter** than the content. A tall terminal hides both the missing-newline
and the ED-versus-SU bugs completely. When diagnosing, dump per-row contents using single-row
selections; reasoning about coordinates was actively misleading, and the row dump was what made
the actual behavior visible.

## Terminator choice

The pwd report is BEL-terminated rather than ST-terminated (`ESC` backslash). With ST, a real
kitty rendered the *following* OSC as literal text. A control test with BEL on the same stream
was clean, so this is not a guess. The value is a URI and cannot contain a BEL, so nothing needs
escaping.

## Known limitations

Kitty graphics and OSC 8 hyperlink targets are absent from a restored screen, because
libghostty's formatter does not re-emit them. Both work in live output. zmx has the same gap
and the fix belongs upstream.

Soft wraps are serialized as hard `\r\n`. The formatter's only wrap-related option is `unwrap`,
which drops the break entirely, and that is not a substitute: the cursor position emitted with
the screen is absolute, so unwrapping content that then occupies fewer rows leaves the cursor a
row low and typing lands on the wrong line. Measured, and the reason the ordering above is the
fix rather than an `unwrap: true`. It only matters when the model and the client disagree about
width, which resizing first prevents.

A screen replayed from a persisted log carries modes no program is waiting on. `replayPersisted`
serializes through the same function, but it builds a throwaway emulator from the log of a session
whose processes are gone, so an alternate screen or mouse tracking it reconstructs is stale by
construction rather than possibly-live. The argument for replaying modes above does not apply there,
and the neighbouring code already draws this distinction for a sibling case: pending output is
discarded after the replay because it answers queries from a program that no longer exists. Not
currently handled, and unmeasured, so it is recorded here rather than fixed on the strength of
reading the code.

## Two sequence numbers, and why conflating them corrupted the prompt

Output is rewritten on the way through, to force `redraw=0` into prompt markers, and that rewrite
makes the data longer. Two counters therefore exist and must not be mixed:

- `lastSeq` counts the **shim's** bytes. It is the position to resubscribe from after a server
  restart, and the shim knows nothing about the rewrite.
- The client log numbers the **rewritten** bytes, since that is what clients read.

Using one as a position in the other desynchronizes them by however much the rewrite added, nine
bytes per prompt for `;redraw=0`. A client then begins reading part-way into an escape sequence,
loses its leading ESC, and the remainder renders as literal text: the symptom was `[183D` printed
beside the prompt after every reattach.

This is worth spelling out because the bug survived a lot of plausible-looking testing. The
rewriter preserves bytes exactly, including across every possible split point, and it has tests
proving that. libghostty's parser was fine. The stored state was fine. What was wrong was one
number used in the wrong coordinate space, two lines away from the rewrite. Bisecting by disabling
the rewriter is what located it, after several hypotheses about terminators, cursor positions, and
consecutive OSC sequences were each disproved by experiment.
