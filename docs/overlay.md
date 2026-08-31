# The overlay

`ctrl-]` opens a few rows at the bottom of an attached session from which any cm command can run. This
file records why it exists, what it is not, and the four decisions in it that look arbitrary without
their reasons.

## What it is for

With a full-screen program in the session, cm was unreachable. Naming the session in front of you meant
leaving nvim or Claude Code, or opening another window and looking the session up first, and the commands
people actually want at that moment -- `bind`, `switch`, `tag` -- are the ones whose argument is "the
session I am looking at".

Keys, all configurable through `prefix_key`:

```
ctrl-]        open
ctrl-] s      switch: choose a session from a list
ctrl-] k      kill: choose one, then y to confirm
ctrl-] b      name this session: type just the name
ctrl-] d      detach
ctrl-] q      send ctrl-\ to the program
ctrl-] ctrl-] send ctrl-] to the program
ctrl-] t      the full picker: cm tui, with its filter and preview pane
ctrl-] :      any cm command, for the long tail
ctrl-] ?      help
escape        back one level; from the top, out of the overlay
ctrl-c        out of the overlay from anywhere

in a list:    type to filter, ctrl-j/ctrl-k or arrows to move, enter to choose
```

## The way out to the full picker

`t` hands the terminal to `cm tui`, which is the answer to the overlay's own list being deliberately
small: six rows under a program that is still drawing is right for choosing among a few sessions and
useless for reading their output. The picker has the filter, the state column and the preview pane.

Three things had to exist for that keypress to work.

**A cancellable reader.** `internal/client` left its reader blocked in the kernel on purpose, and a child
process would have competed with it for every keystroke. `terminalInput` can be suspended, which is what
docs/tui.md called the right answer when it hit the same wall from the other side.

**A switch action in the picker**, since attaching from a session nests and the detach key then belongs to
the outer client -- the picker's own startup notice has always warned about exactly that. `s` moves the
window instead. The binding is enabled by the caller supplying a way to switch, so it is absent rather
than inert when there is nobody to move.

**A way for the choice to come back.** The picker writes the chosen session to a file the client named on
its argv, and the client then switches with machinery it already had. The obvious alternative, having the
picker call the Switch RPC itself, has a race: the server would push the switch to a client that is blocked
on the child, and the repaint afterwards discards the stream, so the window would silently not move. A file
the parent reads after the child has exited has no such window.

## Escape goes up, ctrl-c goes out

Reported about the help, and it generalizes: reading what the keys are is not a reason to lose the overlay.
So escape steps back one level -- out of a list, a prompt, a confirmation, a command's output, or the help --
and only the top level closes. ctrl-c leaves from anywhere, which is what keeps the extra level from being a
trap.

The help is armed with a reference under it rather than a mode of its own, so every action key still works
while it is up: pressing the key you have just read about does what it says instead of only dismissing the
help.

## Choosing beats typing, and that is why there is a picker

The first version made every action a command line, and it was unpleasant to use for a reason worth
writing down: it asked people to *type things they were looking at*. Naming a session is unavoidable
typing, since the name is new text. Choosing a session that already exists is not, and neither is typing
the verb -- `bind` is the keypress `b`.

So `s` and `k` open a chooser (`internal/client/overlaypick.go`) and `b` asks for a name and nothing else.
`:` survives for the long tail, `tag` and `doctor` and `kill --all`, but it is no longer how the common
things are done.

The chooser is filter-first, like fzf: printable keys narrow the list, and moving is ctrl-j/ctrl-k, or
ctrl-n/ctrl-p, or the arrows. Bare j and k cannot also mean movement when every key filters, and a filter
is what makes twenty sessions usable in six rows.

Following fzf that far has one consequence worth stating: **ctrl-j is 0x0a**, so LF cannot also mean enter.
Return is CR, which is what a terminal sends for it, and fzf makes exactly the same trade. The kitty
protocol forms of these keys are decoded too, because a program that turned on report-all-keys makes the
terminal send even ctrl-c as CSI 99;5u -- without those cases the overlay's own keys stop working under
exactly the full-screen programs it exists for.

The list also keeps its height as the filter narrows it, padding with blank rows. The block is anchored to
the bottom of the screen, so a list that shrinks moves every row under the reader's eyes and repaints the
program's content between keystrokes. It grows once, when the sessions replace "listing sessions...", and
then holds still.

Two costs taken deliberately. It is a second list-and-filter implementation next to the one bubbletea
already gives cm, justified only by having to fit under a program that is still drawing; it stays one
column with no preview, and anything richer is what handing over to `cm tui` is for. And its rows are
formatted by hand: the label column is padded to the widest label *in view*, and directories are shortened
from the *left*, because sessions in one project share every leading segment and a row cut from the right
shows the same prefix on every line.

`detach_key` is untouched: detaching is still one press of `ctrl-\`. The two keys are live at once, and
`cm attach` refuses a configuration where they are the same key rather than picking a winner, since
whichever lost would be silently unreachable.

## SIGQUIT was unreachable, and this gives it back

Worth stating separately because nobody had noticed. `inputGate.feed` matches the detach key and
`KeySpec.Find` drops everything after it, so no `0x1c` has ever reached a pty from a cm client. Inside a
cm session, `ctrl-\` could not raise SIGQUIT at all unless you set `detach_key = none`.

Pressing an intercepted key twice forwards it, so this change adds a way to reach both keys rather than
taking one away. It is also the one thing in the overlay that no cm command could do, which is why the
action table is not purely a command line.

## It is not a second TUI

`cm tui` is a bubbletea program that owns the terminal. An attached client cannot hand the terminal over
and take it back: `internal/client.readInput` deliberately leaves a read blocked in the kernel, and
`docs/tui.md` records what that costs from the other direction -- one keystroke eaten per handoff, and a
second `/dev/tty` did not fix it because Go defers the real `close(2)` until the outstanding read
finishes. A cancellable reader would fix it for both, and is still the right eventual answer.

So the overlay paints rows instead, through `internal/client.screen` like everything else cm puts on a
terminal, and runs commands as child processes.

## Commands are the real cm binary

`cm bind`, `cm switch` and `cm tag` already resolve the session from `CM_SESSION`, so the overlay sets it
for the child and stays ignorant of what any command does. New commands work in the overlay the day they
are added.

The child is given `CM_RUNTIME_DIR` and `CM_STATE_DIR` explicitly rather than by inheritance. A client
started with `--runtime-dir` has nothing in its environment saying so, and a child falling back to the
default would talk to a different server: that is the sandbox in `.agents/skills/cm-sandbox` exactly, and
the failure would be a command that quietly did nothing to the session in front of you. `CM_SESSION` is
overridden for the same class of reason: a client attached from *inside* another session carries that
outer session's reference, so every command would have named the wrong session.

Dispatched to a goroutine, not run inline. The attach loop is what paints the session, and a command run
inline would freeze the terminal for as long as it took. Refused outright: `attach`, `a`, `tui`, `wait`,
`follow`, `shim`, anything with `--follow`, and `run` without `-d`. Each says what to do instead, because
a command that appears to hang for ten seconds and then reports giving up teaches people the overlay is
unreliable.

## Closing repaints, and why not something cheaper

The rows the overlay covers hold the program's screen, and the client does not have that content: cm's
terminal model does. So closing drops the resume position and reconnects, which repaints from the model.
That is not new machinery -- `attach.go` does the same after an outage notice overwrote the bottom row,
and a detected output gap does it too -- but it does mean one reconnect per use of the overlay.

Two cheaper repaints were considered and not built:

- **The alternate screen buffer.** `ESC [ ?1049h` and `l` would let the terminal restore the screen
  itself, with no repaint from cm at all. It breaks in exactly the case this feature is for: a program
  already on the alternate screen has no second buffer to save, so the restore brings back the *primary*
  screen and the program's display is gone.
- **A `Repaint` event on the attach stream**, so the server re-sends the restore blob. `Session.attach`
  takes the snapshot and the subscription atomically under one lock; doing that mid-stream means
  reproducing that atomicity or replaying bytes across the snapshot, which is the two-sequence-space
  family that has already cost three bugs. Worth doing if the reconnect proves costly, and not before.

## A forwarded key must not ride a dying stream

Found in a real terminal and worth the paragraph, because the unit tests all passed. Closing the overlay
repaints, and repainting closes the connection immediately, so a keystroke the overlay forwarded on that
stream may never reach the server. Measured: forwarding `ctrl-\` to a foreground `sleep` killed it on
some attempts and not others.

So bytes the overlay forwards go into the client's `pending` buffer -- the same one that holds keystrokes
typed during an outage -- and the reconnect flushes them straight after Open. Deterministic, and verified
5 out of 5 in a real terminal after the change against a fail-sometimes before it.

The general form is worth remembering: anything that returns `outcomeReconnect` in the same breath as
sending on the stream it is about to close has this bug.

## Reading input while holding the keyboard

The subtle part, and the one with the most history behind it. While the overlay is open it takes every
read of terminal input, and that stream carries more than keys: a program inside the session may have
asked the terminal a question, and the answer arrives here. Swallowing one leaves that program blocked
forever, which is cm's largest bug family.

The rule, in `decodeKey`: **a sequence that could be an answer is forwarded, and a sequence that can only
be a keypress is dropped.** Cursor position reports, OSC colour replies, kitty graphics responses, focus
events and mouse reports all reach the session. CSI `u`, CSI `~`, SS3 and the arrow forms do not.

Key releases are part of that. A terminal told to report event types sends a release after every press,
including the release of the prefix key itself, so an overlay that read `CSI 93;5:3u` as a keypress closed
the instant the key came up.

Known cost, stated rather than hidden: the overlay does not hold bytes back the way `inputGate` does, so a
sequence split across two reads is misread -- an escape arriving alone closes the overlay and the tail is
forwarded without it. That window is one read wide and only while the overlay is open, against a holdback
that would delay every keystroke typed at the prompt.

## Painting

Everything `outageNotice` learned, for the same reasons: one write per block, absolute row addressing with
each row cleared first, DECSC and DECRC around the whole thing, and a width one column short of the
terminal because writing the last column leaves it in pending wrap and one more byte then scrolls the
session's screen out from under the model about to repaint it.

Two things a real terminal showed that no test would have. The terminal drew its own cursor on top of the
bar, because the paint restores the program's cursor and that lands on a row the overlay covers: the cursor
is hidden while the overlay is up, and shown again on close. It has to be shown *explicitly*, since the
repaint replays cm's model and the model carries no cursor visibility -- `internal/vt` emits no `?25h` or
`?25l` at all, which is a gap in restore as much as here. And the bar is padded to the full width, because
a highlight that stops where its text does reads as a stray line of the program's output rather than as
cm's.

Three shades, and they are *attributes* rather than colours. The bar is reverse video, the rows under it add
faint, and a chooser's selected row drops the faint again. Each therefore follows the terminal's own
foreground and background: a 256-colour grey that reads well on a dark theme is unreadable on a light one,
and cm does not know which it is looking at. A terminal that ignores faint shows one shade for the whole
block, which is legible and is what this looked like before.

Every row is padded to the width so a shade covers the pane. A highlight that stops where its text does
reads as a stray line of the program's output rather than as cm's, which is what the first version looked
like.

One paint per keypress, which needed fixing: the caller opens the overlay and then feeds whatever
followed the prefix key, and both painted, so the transcript hook showed the whole block reaching the
terminal twice for one press.

The block is capped at half the terminal and 12 rows. What it cuts, it says: a truncated `cm list` read as
a complete one is a wrong answer, not a short one.

The session's own bytes are still written verbatim and never withheld, so a program that draws while the
overlay is up paints over it. The overlay repaints after each chunk of session output and after a resize,
so it heals rather than being left half erased. A reserved row through DECSTBM margins, which is what tmux
does, would avoid the fight; it also changes the scrolling region a program is using, which is a larger
commitment than this feature has earned.
