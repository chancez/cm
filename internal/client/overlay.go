package client

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/chancez/cm/internal/ansi"
)

// overlay is cm's own UI inside an attached session: a few rows at the bottom of the terminal, opened by
// the prefix key, from which any cm command can be run.
//
// Why it exists: with a full-screen program in the session there was no way to reach cm at all. Naming
// the session in front of you meant leaving nvim or Claude Code, or opening another window and looking
// the session up, and the commands people want here -- bind, switch, tag -- are exactly the ones whose
// argument is "the session I am looking at".
//
// It is deliberately not a second terminal UI. `cm tui` is a bubbletea program that owns the terminal; an
// attached client cannot hand the terminal over without a cancellable input reader, which internal/client
// does not have (see docs/tui.md, where the same wall was hit from the other side). So this paints rows
// at the bottom, through the client's screen so nothing lands mid-sequence, and every command it runs is
// the real `cm` binary in a child process.
//
// Closing repaints from cm's terminal model rather than restoring what was overwritten. The client does
// not hold the session's content, which is the same reason the outage notice forces a repaint: see
// Attach. Overwriting the bottom rows of a program's screen and putting them back is not something a
// client can do alone.
type overlay struct {
	// out paints, and must be the client's screen: an overlay row written straight to the terminal is the
	// bug internal/client.screen exists to prevent.
	out io.Writer
	// size reports the terminal's current size. Zeros mean it could not be determined, and nothing is
	// painted then: a row number guessed wrong would write into the middle of the session.
	size func() (rows, cols uint16)
	// enabled is false for anything not painting a terminal, which is a follower streaming bytes to a
	// pipe. Same distinction the outage notice and the gap repaint draw.
	enabled bool
	// readOnly reports that this client's input is dropped by the server, so nothing here may claim to
	// have sent anything to the session.
	readOnly bool

	// prefix and detach are named in the help line and matched while armed, where pressing either a
	// second time forwards it to the program. That is the only way to reach a key cm intercepts: ctrl-\
	// has never reached a pty from a cm client, so SIGQUIT was unreachable inside a session.
	prefix KeySpec
	detach KeySpec

	// session is what the bar shows, and it is a label rather than a reference: what a command acts on is
	// carried in the child's CM_SESSION by the runner.
	session string
	// canPick reports that the caller can hand the terminal to the full picker, which every caller that is
	// not `cm attach` cannot.
	canPick bool

	log *slog.Logger

	// mode is what is on screen.
	mode overlayMode
	// line is what has been typed at the prompt.
	line []rune
	// prompt names what the line is for, so a field asking for a name does not look like a command line.
	prompt promptKind
	// pick is the chooser, set while mode is overlayPick.
	pick *picker
	// confirm is a command held until one keypress approves it, and confirmWhat describes it.
	confirm     []string
	confirmWhat string
	// helping is the help on screen, which is armed with a reference under it rather than a mode of its
	// own: every action key still works from there, so a key you have just read about does what it says.
	helping bool
	// status replaces the hints on the bar: what a command printed, or why one was refused.
	status string
	// body is a command's output, under the bar.
	body []string
	// painted is how many rows are dirty, so closing erases exactly those. Counted rather than
	// recomputed, since the terminal may have been resized since the paint.
	painted int
}

// overlayMode is what the overlay is showing.
type overlayMode int

const (
	// overlayClosed is not on screen at all.
	overlayClosed overlayMode = iota
	// overlayArmed is the prefix pressed, waiting for an action key.
	overlayArmed
	// overlayPrompt is text being typed: a cm command, or a name.
	overlayPrompt
	// overlayPick is a session being chosen from a list.
	overlayPick
	// overlayConfirm is a command waiting for one key to approve it.
	overlayConfirm
	// overlayRunning is a command dispatched and not yet finished.
	overlayRunning
	// overlayResult is what a command printed, dismissed by any key.
	overlayResult
)

// promptKind is what the typed line means.
//
// A distinction worth having rather than one prompt for everything: the first version of this made every
// action a command line, so binding a name meant typing "bind" as well as the name. Typing a name is
// unavoidable, since the name is new text; typing the verb that consumes it is not.
type promptKind int

const (
	// promptCommand is a whole cm command line, the escape hatch for the long tail.
	promptCommand promptKind = iota
	// promptName is a name for this session, which becomes `cm bind <name>`.
	promptName
)

// maxOverlayRows bounds the block, on top of never taking more than half the terminal.
//
// A cap at all because a command like `cm list` on a machine with thirty sessions would otherwise cover
// the screen it is supposed to be an overlay on. Whatever is cut is said so rather than silently
// dropped, since the alternative is a truncated list read as a complete one.
const maxOverlayRows = 12

// overlayResponse is what the client must do after the overlay consumed a read.
//
// Everything the overlay cannot do itself, which is anything involving the connection: it holds no
// stream and no server client on purpose, so the one place that talks to a server stays the attach loop.
type overlayResponse struct {
	// Send goes to the session's pty.
	//
	// Two things arrive here. A deliberate forward of a key cm intercepts, which is how SIGQUIT is
	// reachable. And anything in the read the overlay does not recognize as a keypress, which matters more
	// than it looks: a terminal's reply to a query the *program* asked arrives in this same stream, and an
	// overlay that swallowed it would leave that program blocked on an answer that never comes.
	Send []byte
	// Run is a cm command to dispatch, as an argv without the leading "cm".
	//
	// Dispatched by the caller rather than run here, and asynchronously, because this is called from the
	// attach loop: a command run inline would freeze the session's output for as long as it took, and
	// `cm doctor` takes over a second.
	Run []string
	// List asks the caller to fetch the session list, which arrives back through overlay.sessions.
	//
	// The overlay holds no server client, so it cannot fetch its own list: the one place that talks to a
	// server is the attach loop. Asked for rather than cached, because a picker showing sessions that ended
	// minutes ago is worse than a brief "listing sessions...".
	List bool
	// OpenPicker asks the caller to hand the terminal to cm's session picker.
	//
	// The overlay's own list is a handful of rows under a program that is still drawing, which is the right
	// shape for choosing among a few sessions and the wrong one for reading their output. This is the way
	// out to the full picker, with its filter and its preview pane, and it is a keypress rather than the
	// default because it covers the screen.
	OpenPicker bool
	// SwitchTo names a session this client should move to.
	//
	// A reference the caller acts on rather than a `cm switch` command, because the client can already do
	// this: outcomeSwitch keeps the process, the terminal and the input reader, so switching costs no
	// re-exec and no reattach. Spelled as an ID for the reason pickItem.Ref gives.
	SwitchTo string
	// Detach ends the attachment.
	Detach bool
	// Repaint reports that the overlay left the screen and the rows it covered need repainting from cm's
	// model. Set on every close, since anything painted was over the session's own content.
	Repaint bool
}

// active reports whether the overlay is on screen, in which case it owns the keyboard.
func (o *overlay) active() bool { return o.mode != overlayClosed }

// open arms the overlay, which is what the prefix key does.
func (o *overlay) open() {
	o.mode = overlayArmed
	o.reset()
	o.paint()
}

// reset clears everything an action left behind, so one keypress cannot inherit the last one's state.
func (o *overlay) reset() {
	o.line = o.line[:0]
	o.prompt = promptCommand
	o.status = ""
	o.body = nil
	o.pick = nil
	o.confirm = nil
	o.confirmWhat = ""
	o.helping = false
}

// feed offers the overlay a read of keystrokes and reports what the client must do.
func (o *overlay) feed(data []byte) overlayResponse {
	var resp overlayResponse

	// Nothing to apply and nothing to repaint. The caller opens the overlay and then feeds whatever
	// followed the prefix key in the same read, which is usually nothing, and open has already painted:
	// measured with the transcript hook, that wrote the whole block to the terminal twice per keypress.
	if len(data) == 0 {
		return resp
	}

	for len(data) > 0 {
		// While armed, the two keys cm intercepts are checked before anything is decoded, so pressing
		// either one a second time forwards it. Only at the front of what is left, so this cannot claim a
		// byte in the middle of a sequence being passed through.
		if o.mode == overlayArmed {
			if i, n := o.prefix.find(data); i == 0 {
				o.forwardKey(o.prefix, &resp)
				data = data[n:]
				continue
			}
			if i, n := o.detach.find(data); i == 0 {
				o.forwardKey(o.detach, &resp)
				data = data[n:]
				continue
			}
		}

		key, n := decodeKey(data)
		consumed := data[:n]
		data = data[n:]

		switch key.Kind {
		case keyPassThrough:
			// Forwarded rather than dropped: see overlayResponse.Send. Nothing cm binds looks like this,
			// so the bytes belong to the program whether they are an event or an answer.
			resp.Send = append(resp.Send, consumed...)
		case keyIgnore:
			// A key release or a repeat of one cm handled, which a terminal reporting event types sends
			// after every press. Dropping these is what stops the overlay closing the instant the prefix
			// key is let go.
		case keyEscape:
			o.back(&resp)
		case keyCancel:
			o.close(&resp)
		default:
			o.handleKey(key, &resp)
		}

		if o.mode == overlayClosed {
			// Everything after the keystroke that closed the overlay belongs to the session, in the order
			// it was typed. Dropping it would lose a keystroke on every use, since the terminal delivers a
			// fast two-key sequence as one read.
			resp.Send = append(resp.Send, data...)
			return resp
		}
	}

	o.paint()
	return resp
}

// back steps out of whatever is on screen, and out of the overlay itself from the top.
//
// Escape rather than ctrl-c, and the two now differ: escape goes up one level, ctrl-c leaves. An accidental
// `s` or `?` should not throw you out of the overlay, which is what a single meaning for both keys did.
func (o *overlay) back(resp *overlayResponse) {
	switch {
	case o.helping:
		// Back to the hints, still armed. What the user asked for: reading the help is not a reason to lose
		// the overlay.
		o.helping = false
		o.body = nil
		o.status = ""
	case o.mode == overlayArmed:
		o.close(resp)
	default:
		// A list, a prompt, a confirmation or a command's output. All of them are one step in, so escape
		// returns to the action keys rather than to the session.
		o.mode = overlayArmed
		o.reset()
	}
	// Not painted here: feed paints once after the read it is handling, and painting again would write the
	// block to the terminal twice for one keypress.
}

// handleKey applies one keypress in the current mode.
func (o *overlay) handleKey(key overlayKey, resp *overlayResponse) {
	switch o.mode {
	case overlayArmed:
		o.armedKey(key, resp)
	case overlayPrompt:
		o.promptKey(key, resp)
	case overlayPick:
		o.pickKey(key, resp)
	case overlayConfirm:
		o.confirmKey(key, resp)
	case overlayRunning:
		// Ignored rather than queued. A keystroke typed while a command runs would otherwise act on the
		// result screen that is about to appear, which is not what the user was answering.
	case overlayResult:
		// Any key dismisses, and the key is not acted on: the user is closing a message, and treating that
		// keystroke as a new action would run something they did not choose. Escape does not arrive here at
		// all; it steps back to the action keys instead.
		o.close(resp)
	}
}

// armedKey is the action table, and it is deliberately small.
//
// Every entry either cannot be done by a command (detaching, forwarding a key) or is a shortcut into the
// prompt for one that can. Nothing here reimplements a cm command: `cm bind` and `cm switch` already
// resolve the session from CM_SESSION, and the runner sets it, so the overlay can stay ignorant of what
// they do.
func (o *overlay) armedKey(key overlayKey, resp *overlayResponse) {
	if key.Kind != keyRune {
		// Enter or backspace while armed. Nothing to do with either, and closing is the honest answer:
		// leaving the overlay armed would put the *next* keystroke into cm long after the user moved on.
		o.close(resp)
		return
	}

	// Any action taken from the help screen leaves the help behind, which is what makes reading about a key
	// and then pressing it work.
	if key.Rune != '?' {
		o.helping = false
		o.body = nil
	}

	switch key.Rune {
	case 'd':
		resp.Detach = true
		o.close(resp)
	case 's':
		// A chooser rather than a prompt. Typing the name of a session you can see listed is the friction
		// that made the first version of this unpleasant to use.
		o.startPick("switch to", pickSwitch, resp)
	case 'k':
		o.startPick("kill", pickKill, resp)
	case 'b':
		// A name is new text, so this one really does need typing. Only the name, though: the verb is the
		// keypress.
		o.mode = overlayPrompt
		o.prompt = promptName
		o.line = o.line[:0]
	case 't':
		if !o.canPick {
			o.status = "this client cannot open the picker"
			o.mode = overlayResult
			return
		}
		resp.OpenPicker = true
		// Closed rather than left up: the picker takes the whole screen, so the bar has nowhere to be, and
		// what comes back is either a switch or a repaint.
		o.close(resp)
	case ':':
		o.mode = overlayPrompt
		o.prompt = promptCommand
		o.line = o.line[:0]
	case 'q':
		// A mnemonic for the detach key, which on the default is ctrl-\ and so SIGQUIT. Same effect as
		// pressing the detach key while armed, and worth having twice: this one is discoverable from the
		// help line, and the other is the tmux habit.
		o.forwardKey(o.detach, resp)
	case '?':
		if o.helping {
			// A toggle, as it is in `cm tui`: the key that opened the help closes it. Escape does too, but a
			// reader who opened the help with ? reaches for ? to put it away.
			o.helping = false
			o.body = nil
			o.status = ""
			return
		}
		o.status = fmt.Sprintf("help -- ? or escape goes back, %s twice sends it to the program",
			o.prefix.Name)
		o.body = []string{
			"s  switch session    b  name this session",
			"k  kill a session    d  detach",
			"t  the full picker, with a preview pane",
			"q  send " + o.detach.Name + " to the program",
			":  any cm command    ctrl-c  leave the overlay",
			"in a list: type to filter, ctrl-j/ctrl-k to move, enter to choose",
		}
		// Still armed, so every key above works from here. Only escape is special, and it returns to the
		// hints rather than closing.
		o.helping = true
	default:
		// An unbound key closes rather than waiting for a second guess, which is what a prefix armed
		// forever would be: the next keystroke would go to cm long after the user forgot they pressed it.
		o.status = fmt.Sprintf("no action for %q", string(key.Rune))
		o.mode = overlayResult
	}
}

// promptKey edits the command line.
func (o *overlay) promptKey(key overlayKey, resp *overlayResponse) {
	switch key.Kind {
	case keyEnter:
		args, err := o.promptArgs()
		switch {
		case err != nil:
			o.status = err.Error()
			o.mode = overlayResult
		case len(args) == 0:
			o.close(resp)
		default:
			resp.Run = args
			o.status = "running " + strings.Join(args, " ")
			o.body = nil
			o.mode = overlayRunning
		}
	case keyBackspace:
		if len(o.line) > 0 {
			o.line = o.line[:len(o.line)-1]
		}
	case keyKillLine:
		o.line = o.line[:0]
	default:
		o.line = append(o.line, key.Rune)
	}
}

// promptArgs turns what was typed into a command, according to what the prompt was asking for.
func (o *overlay) promptArgs() ([]string, error) {
	text := strings.TrimSpace(string(o.line))
	if text == "" {
		return nil, nil
	}
	if o.prompt == promptName {
		// Not split like a command line: a name is one argument, and quoting rules would only be a way to
		// get a confusing error. cm validates it, which is where that check belongs.
		return []string{"bind", text}, nil
	}
	return splitCommandLine(text)
}

// startPick opens the chooser and asks the caller for the session list.
func (o *overlay) startPick(prompt string, action pickAction, resp *overlayResponse) {
	o.mode = overlayPick
	o.line = o.line[:0]
	o.body = nil
	o.status = ""
	o.pick = &picker{prompt: prompt, action: action, loading: true}
	resp.List = true
}

// sessions fills the chooser with what the server reported, which the caller fetched.
func (o *overlay) sessions(items []pickItem, err error) {
	if o.mode != overlayPick || o.pick == nil {
		// The overlay moved on while the list was in flight, which an escape does. Dropped rather than
		// painted over whatever is on screen now.
		o.log.Debug("a session list arrived after its picker closed", "err", err, "items", len(items))
		return
	}
	o.pick.loading = false
	if err != nil {
		o.pick.err = err.Error()
		o.paint()
		return
	}
	o.pick.items = items
	o.paint()
}

// pickKey applies one keypress to the chooser and acts on a choice.
func (o *overlay) pickKey(key overlayKey, resp *overlayResponse) {
	switch o.pick.key(key) {
	case pickedItem:
		it, ok := o.pick.selected()
		if !ok {
			return
		}
		switch o.pick.action {
		case pickSwitch:
			if it.Current {
				// Refused rather than performed. Switching to the session you are in is a no-op that looks
				// like a broken keypress, and the reconnect it would cost is a visible repaint for nothing.
				o.status = "already attached to " + it.Label
				o.mode = overlayResult
				return
			}
			resp.SwitchTo = it.Ref
			o.close(resp)
		case pickKill:
			// One key of confirmation, because a mistyped filter plus enter would otherwise end someone's
			// shell. The label rather than the ID, since that is what the user recognizes.
			o.mode = overlayConfirm
			o.confirm = []string{"kill", it.Ref}
			o.confirmWhat = "kill " + it.Label
			o.pick = nil
		}
	}
}

// confirmKey approves or abandons a held command.
//
// Only y approves. Any other key abandons, rather than only escape: the safe answer has to be the easy one,
// and a user who reaches this screen by accident presses something arbitrary to get out of it.
func (o *overlay) confirmKey(key overlayKey, resp *overlayResponse) {
	if key.Kind == keyRune && (key.Rune == 'y' || key.Rune == 'Y') {
		resp.Run = o.confirm
		o.status = "running " + strings.Join(o.confirm, " ")
		o.confirm, o.confirmWhat = nil, ""
		o.body = nil
		o.mode = overlayRunning
		return
	}
	o.close(resp)
}

// finish reports a dispatched command's outcome, which the attach loop delivers when the child exits.
func (o *overlay) finish(out string, err error) {
	if o.mode != overlayRunning {
		// The overlay was closed while the command ran, by a detach or by the session ending. Its output
		// has nowhere to go, and logging it is better than painting over whatever is on screen now.
		o.log.Debug("a cm command finished after its overlay closed", "err", err, "output", out)
		return
	}
	o.status = "cm " + string(o.line)
	o.body = overlayBody(out, err)
	o.mode = overlayResult
	o.paint()
}

// overlayBody turns a command's output into the rows under the bar.
//
// An error is shown as well as the output, not instead of it: cm commands print the useful part to
// stdout and the reason to stderr, and a runner that captured only one of them would show either a bare
// exit status or a success message for a failure.
func overlayBody(out string, err error) []string {
	// Sanitized on purpose, though a cm command's output should hold no escapes. This block is written
	// into a screen a program is drawing on, and an escape byte here is the family of bugs cm has had six
	// times. "Should be clean" is what every one of those also had.
	text := strings.TrimRight(string(ansi.Strip([]byte(out))), "\n")
	var lines []string
	// Guarded rather than split unconditionally, since splitting "" yields one empty line and a command
	// that printed nothing would then get a blank row it did not ask for.
	if text != "" {
		for _, line := range strings.Split(text, "\n") {
			lines = append(lines, plainRow(line))
		}
	}
	if err != nil {
		lines = append(lines, plainRow("error: "+err.Error()))
	}
	return lines
}

// plainRow strips what is left of a line after ansi.Strip: the C0 bytes it keeps, and tabs.
func plainRow(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	col := 0
	for _, r := range s {
		switch {
		case r == '\t':
			// Expanded rather than forwarded. A tab moves the cursor by an amount this code cannot see, so
			// the row would run past the width it was clipped to and wrap, which scrolls the screen.
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case r < 0x20 || r == 0x7f:
			// Dropped. internal/ansi removes the three that appear in practice; a form feed or a NUL here
			// is as bad as an escape and carries nothing a person reads.
		default:
			b.WriteRune(r)
			col++
		}
	}
	return b.String()
}

// rows renders the block, top to bottom, clipped to the terminal.
//
// Split from painting so the layout can be asserted without a terminal, which is most of what there is
// to get wrong: a row one too long puts the terminal into pending wrap, and one more byte then scrolls
// the session's screen out from under the model that is about to repaint it.
func (o *overlay) rows(rows, cols int) []string {
	if rows <= 0 || cols <= 0 || o.mode == overlayClosed {
		return nil
	}
	// One short of the width, for the pending-wrap reason above. The same arithmetic as the outage
	// notice, which learned it the hard way.
	width := cols - 1
	if width <= 0 {
		return nil
	}

	// Never more than half the screen, and never more than maxOverlayRows. A block that covered the
	// program it is overlaying would make the overlay the problem.
	budget := min(max(rows/2, 1), maxOverlayRows)
	// Padded to the full width, so the bar's inverse video spans the pane rather than ending where the text
	// does. A short highlighted run reads as a stray line of output; a full-width one reads as cm's.
	out := []string{pad(clip(o.bar(), width), width)}

	// The chooser sizes itself to what is left, so its window can follow the cursor. Everything else is a
	// fixed block that gets truncated below.
	body := o.body
	if o.mode == overlayPick && o.pick != nil {
		body = o.pick.body(budget - 1)
	}

	for i, line := range body {
		if len(out) >= budget {
			// Said rather than silently cut. A truncated list read as a complete one is the failure worth
			// avoiding: `cm list` showing four sessions when there are nine is a wrong answer, not a short one.
			out[len(out)-1] = clip(fmt.Sprintf("... and %d more lines, run it in a shell to see them",
				len(body)-i+1), width)
			break
		}
		out = append(out, clip(line, width))
	}
	return out
}

// bar is the top row's text, which says what the overlay is for as well as what it is doing.
func (o *overlay) bar() string {
	label := o.session
	if label == "" {
		label = "session"
	}
	switch {
	case o.status != "":
		return fmt.Sprintf(" cm %s | %s ", label, o.status)
	case o.mode == overlayConfirm:
		return fmt.Sprintf(" cm %s | %s? y to confirm, any other key cancels ", label, o.confirmWhat)
	case o.mode == overlayPick:
		return fmt.Sprintf(" cm %s | %s: %s", label, o.pick.prompt, string(o.pick.filter))
	case o.mode == overlayPrompt && o.prompt == promptName:
		return fmt.Sprintf(" cm %s | name: %s", label, string(o.line))
	case o.mode == overlayPrompt:
		return fmt.Sprintf(" cm %s : %s", label, string(o.line))
	default:
		return fmt.Sprintf(" cm %s | s switch  b name  k kill  t picker  d detach  q %s  ? help ",
			label, o.detach.Name)
	}
}

// paint puts the block on screen.
func (o *overlay) paint() {
	if !o.enabled || o.mode == overlayClosed {
		return
	}
	rows, cols := o.size()
	if rows == 0 || cols == 0 {
		return
	}
	lines := o.rows(int(rows), int(cols))
	if len(lines) == 0 {
		return
	}

	// Every row a previous block used is written even when this one is shorter, blank where there is no
	// content, so a block that shrinks does not leave the top of a taller one on screen. That happened
	// going from a command's output back to the bar alone.
	total := max(o.painted, len(lines))
	blank := total - len(lines)

	// DECSC and DECRC around the whole block, so the cursor the program is using is exactly where it was.
	// Each row is addressed absolutely and cleared first, so nothing here can scroll and no row can hold
	// the tail of a longer one.
	var b strings.Builder
	// The cursor is hidden for as long as the overlay is up, and this is not cosmetic: the program's cursor
	// is restored below, and when that lands on a row the overlay covers the terminal draws its cursor on
	// top of the bar. Reported from a real terminal.
	//
	// Not moved to the prompt instead, which would look better: the session's bytes are written verbatim
	// and immediately, and a program that draws while the overlay is up does so from wherever the cursor
	// is. Leaving it in cm's bar would corrupt that program's output, which is a worse bug than a missing
	// cursor.
	b.WriteString("\x1b[?25l\x1b7")
	first := int(rows) - total + 1
	for i := range total {
		row := first + i
		if row <= 0 {
			continue
		}
		switch {
		case i < blank:
			fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", row)
		case i == blank:
			// The bar is inverse so it reads as cm's rather than as the program's own output.
			fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K\x1b[7m%s\x1b[0m", row, lines[i-blank])
		default:
			fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K%s", row, lines[i-blank])
		}
	}
	b.WriteString("\x1b8")
	// One write, so the screen either holds the block back or emits it whole rather than splitting it.
	fmt.Fprint(o.out, b.String())

	o.painted = len(lines)
}

// repaint redraws the block over whatever the session has written since.
//
// Called after the session writes while the overlay is open. The session's bytes go to the terminal
// verbatim and unconditionally, which is right -- they are what the user is waiting to see -- so a
// program that draws while the overlay is up will paint over it. Redrawing after each chunk means the
// overlay self-heals instead of being left half erased.
func (o *overlay) repaint() {
	if o.active() {
		o.paint()
	}
}

// close takes the overlay off screen and asks for a repaint.
func (o *overlay) close(resp *overlayResponse) {
	if o.mode == overlayClosed {
		return
	}
	o.mode = overlayClosed
	o.line = o.line[:0]
	o.status = ""
	o.body = nil

	if o.painted > 0 {
		if rows, cols := o.size(); rows > 0 && cols > 0 {
			var b strings.Builder
			b.WriteString("\x1b7")
			for i := 0; i < o.painted; i++ {
				if row := int(rows) - i; row > 0 {
					fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2K", row)
				}
			}
			// Shown again explicitly, because nothing else will: the repaint replays cm's terminal model, and
			// the model does not carry cursor visibility at all -- internal/vt emits no ?25h or ?25l. The cost
			// is that a program which had *hidden* its cursor gets it back until it hides it again, which is
			// the same gap a reattach to such a program already has. Recorded in docs/ideas.md.
			b.WriteString("\x1b8\x1b[?25h")
			fmt.Fprint(o.out, b.String())
		}
		// Erasing is not enough, and this is the part that is easy to leave out: the rows held the
		// program's content, cm's model is the only thing that knows what was there, and a blank row where
		// a status line belongs looks exactly like a bug in the program. The repaint is the caller's, since
		// only it can drop the resume position and reopen.
		resp.Repaint = true
		o.painted = 0
	}
}

// forwardKey sends a key cm intercepts to the session and closes, or explains why it could not.
//
// The one thing here that cannot be done by any cm command, and the reason the overlay is not purely a
// command line: cm has always eaten its detach key, so ctrl-\ never reached a pty from a cm client and
// SIGQUIT was unreachable inside a session. Pressing the key twice, or `q`, is how it gets there.
func (o *overlay) forwardKey(key KeySpec, resp *overlayResponse) {
	switch {
	case !key.live():
		// Reachable through `q` with detach_key = none, where there is no key to send. Said rather than
		// silently sending NUL, which is what the zero KeySpec's Byte would be.
		o.status = "no key is configured for that"
		o.mode = overlayResult
	case o.readOnly:
		o.status = "read-only: nothing was sent"
		o.mode = overlayResult
	default:
		o.log.Debug("forwarding a key cm intercepts to the session", "key", key.Name)
		// The control byte rather than whatever encoding the terminal used. A program inside the session
		// did not negotiate the kitty protocol with cm's client, and the pty's line discipline acts on the
		// byte: forwarding CSI 92;5u would put "[92;5u" on the command line instead of raising SIGQUIT.
		resp.Send = append(resp.Send, key.Byte)
		o.close(resp)
	}
}

// pad fills a row out to a width, so a highlighted bar covers the pane.
func pad(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

// clip cuts a row to a width in characters.
//
// Measured in runes rather than bytes so a multi-byte character is not cut in half, which puts a
// replacement character on screen and, worse, can leave a lone continuation byte the terminal counts as
// a cell. Double-width characters are not accounted for: a CJK name in the bar can therefore reach the
// last column, which costs a wrap and is why the width is already one short.
func clip(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	n := 0
	for i := range s {
		if n == width {
			return s[:i]
		}
		n++
	}
	return s
}

// overlayCommand is a finished command on its way back to the overlay.
type overlayCommand struct {
	out string
	err error
}

// overlaySessions is a fetched session list on its way to the chooser.
type overlaySessions struct {
	items []pickItem
	err   error
}
