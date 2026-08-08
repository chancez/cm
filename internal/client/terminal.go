package client

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// resetSequence is a full terminal reset (RIS).
//
// Deliberately blunt. A session may have left the terminal in any combination of alternate
// screen, mouse reporting, bracketed paste, and keyboard protocol modes, and unsetting them
// individually means keeping a list that silently rots as programs adopt new modes. A reset
// costs a repaint that the shell prompt covers anyway.
const resetSequence = "\x1bc"

// clearSequence clears the screen and homes the cursor, used before painting restored state
// so leftover output from the client's own shell does not sit behind the session.
const clearSequence = "\x1b[2J\x1b[H"

// TTY owns the local terminal's state for the duration of an attachment.
type TTY struct {
	in  *os.File
	out *os.File

	// prevState is the terminal mode to restore on exit. Nil when stdin is not a
	// terminal, in which case no mode was changed and none should be restored.
	prevState *term.State
	// isTTY records whether output is a terminal, so escape sequences are not written
	// into a pipe.
	isTTY bool
	// closed makes Close idempotent.
	closed bool
}

// OpenTTY puts the terminal into raw mode.
//
// When stdin is not a terminal, which happens when input is piped, no mode is changed at
// all. Calling tcsetattr with a state that was never read would apply uninitialized
// settings to whatever stdin actually is.
func OpenTTY(in, out *os.File) (*TTY, error) {
	t := &TTY{
		in:    in,
		out:   out,
		isTTY: term.IsTerminal(int(out.Fd())),
	}

	if !term.IsTerminal(int(in.Fd())) {
		return t, nil
	}

	// Read the current mode before changing anything so exit can restore exactly what was
	// there, rather than a guess at what is normal.
	prev, err := makeRawPreservingSignals(int(in.Fd()))
	if err != nil {
		return nil, fmt.Errorf("putting terminal into raw mode: %w", err)
	}
	t.prevState = prev
	return t, nil
}

// makeRawPreservingSignals enters raw mode but keeps Ctrl-\ deliverable as a byte.
//
// term.MakeRaw is most of what is needed, but VQUIT must also be disabled: otherwise the
// tty layer turns Ctrl-\ into SIGQUIT for the client process instead of delivering 0x1C,
// so the detach key would kill the client rather than detach it. VLNEXT is disabled for the
// same class of reason, so Ctrl-V reaches the session rather than being consumed locally.
func makeRawPreservingSignals(fd int) (*term.State, error) {
	prev, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}

	termios, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		term.Restore(fd, prev)
		return nil, err
	}
	termios.Cc[unix.VQUIT] = posixVDisable
	termios.Cc[unix.VLNEXT] = posixVDisable
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, termios); err != nil {
		term.Restore(fd, prev)
		return nil, err
	}
	return prev, nil
}

// Size reports the terminal's current size. Zeros mean it could not be determined, which is
// normal when output is not a terminal.
func (t *TTY) Size() (rows, cols uint16) {
	ws, err := unix.IoctlGetWinsize(int(t.out.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return ws.Row, ws.Col
}

// Clear clears the screen, used before painting restored session state. It does nothing
// when output is not a terminal, where escape bytes would just corrupt the stream.
func (t *TTY) Clear() error {
	if !t.isTTY {
		return nil
	}
	_, err := t.out.WriteString(clearSequence)
	return err
}

// IsTerminal reports whether output is a real terminal. Callers use this to distinguish a
// closed window, which should end an attachment, from exhausted piped input, which should
// not.
func (t *TTY) IsTerminal() bool { return t.isTTY }

// Write sends bytes to the terminal.
func (t *TTY) Write(p []byte) (int, error) { return t.out.Write(p) }

// Read reads from the terminal.
func (t *TTY) Read(p []byte) (int, error) { return t.in.Read(p) }

// Close restores the terminal.
//
// The reset happens before restoring the mode so the escape sequence is not reinterpreted
// under different settings. Calling Close more than once is safe and does nothing after the
// first: emitting the reset twice would leave a stray character on screen, since a terminal
// that does not recognize the sequence prints its trailing byte.
func (t *TTY) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true

	var errs []error

	// Only reset when a terminal is actually attached. Writing escape bytes into a pipe
	// would corrupt whatever is consuming the output.
	if t.isTTY {
		if _, err := t.out.WriteString(resetSequence); err != nil {
			errs = append(errs, err)
		}
	}
	if t.prevState != nil {
		// Restoring discards input that arrived but was not read, so keystrokes typed
		// during teardown do not leak into the shell that started the client.
		if err := term.Restore(int(t.in.Fd()), t.prevState); err != nil {
			errs = append(errs, err)
		}
		t.prevState = nil
	}
	return errors.Join(errs...)
}
