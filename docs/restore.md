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

## Known limitation

Kitty graphics and OSC 8 hyperlink targets are absent from a restored screen, because
libghostty's formatter does not re-emit them. Both work in live output. zmx has the same gap
and the fix belongs upstream.
