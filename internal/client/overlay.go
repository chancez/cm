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

	log *slog.Logger

	// mode is what is on screen.
	mode overlayMode
	// line is what has been typed at the prompt.
	line []rune
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
	// overlayPrompt is a cm command being typed.
	overlayPrompt
	// overlayRunning is a command dispatched and not yet finished.
	overlayRunning
	// overlayResult is what a command printed, dismissed by any key.
	overlayResult
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
	o.line = o.line[:0]
	o.status = ""
	o.body = nil
	o.paint()
}

// feed offers the overlay a read of keystrokes and reports what the client must do.
func (o *overlay) feed(data []byte) overlayResponse {
	var resp overlayResponse

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

// handleKey applies one keypress in the current mode.
func (o *overlay) handleKey(key overlayKey, resp *overlayResponse) {
	switch o.mode {
	case overlayArmed:
		o.armedKey(key, resp)
	case overlayPrompt:
		o.promptKey(key, resp)
	case overlayRunning:
		// Ignored rather than queued. A keystroke typed while a command runs would otherwise act on the
		// result screen that is about to appear, which is not what the user was answering.
	case overlayResult:
		// Any key dismisses, and the key is not acted on: the user is closing a message, and treating that
		// keystroke as a new action would run something they did not choose.
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

	switch key.Rune {
	case 'd':
		resp.Detach = true
		o.close(resp)
	case ':':
		o.mode = overlayPrompt
		o.line = o.line[:0]
	case 'b':
		o.mode = overlayPrompt
		o.line = []rune("bind ")
	case 's':
		o.mode = overlayPrompt
		o.line = []rune("switch ")
	case 'q':
		// A mnemonic for the detach key, which on the default is ctrl-\ and so SIGQUIT. Same effect as
		// pressing the detach key while armed, and worth having twice: this one is discoverable from the
		// help line, and the other is the tmux habit.
		o.forwardKey(o.detach, resp)
	case '?':
		o.status = fmt.Sprintf("%s twice sends it to the program, %s sends %s",
			o.prefix.Name, "q", o.detach.Name)
		o.body = []string{
			"d  detach            :  run a cm command",
			"b  bind <name>       s  switch <session>",
			"?  this help         escape  close",
		}
		o.mode = overlayResult
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
		args, err := splitCommandLine(string(o.line))
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
	out := []string{clip(o.bar(), width)}
	for i, line := range o.body {
		if len(out) >= budget {
			// Said rather than silently cut. A truncated list read as a complete one is the failure worth
			// avoiding: `cm list` showing four sessions when there are nine is a wrong answer, not a short one.
			out[len(out)-1] = clip(fmt.Sprintf("... and %d more lines, run it in a shell to see them",
				len(o.body)-i+1), width)
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
	case o.mode == overlayPrompt:
		return fmt.Sprintf(" cm %s : %s", label, string(o.line))
	default:
		return fmt.Sprintf(" cm %s | d detach  : command  b bind  s switch  q %s  ? help ",
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
	b.WriteString("\x1b7")
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
			b.WriteString("\x1b8")
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
