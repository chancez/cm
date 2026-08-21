package client

import (
	"testing"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// A display-only client must leave the terminal's mode alone, so ctrl-c still becomes a signal.
//
// `cm read --follow` used OpenTTY, which calls term.MakeRaw, and raw mode clears ISIG: the tty layer
// then stops turning ctrl-c into SIGINT and delivers 0x03 as a byte instead. A follower is read-only, so
// it read that byte and dropped it, leaving the command unkillable from the terminal running it. That is
// worse than an ordinary hang because `cm read --raw --follow` is the documented way to capture a
// session's real byte stream, so the tool for diagnosing escape-sequence bugs could not be exited.
//
// Asserted on the termios flags rather than by sending a signal: what went wrong is a terminal mode, and
// reading it back is direct evidence where spawning a process and racing a signal would not be.
func TestOpenTTYCookedLeavesSignalsEnabled(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open() error = %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	before, err := unix.IoctlGetTermios(int(tty.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatalf("reading termios before: %v", err)
	}
	if before.Lflag&unix.ISIG == 0 {
		t.Fatalf("a fresh pty already had ISIG clear, so this test proves nothing")
	}

	ttyWrap, err := OpenTTYCooked(tty, tty)
	if err != nil {
		t.Fatalf("OpenTTYCooked() error = %v", err)
	}

	after, err := unix.IoctlGetTermios(int(tty.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatalf("reading termios after: %v", err)
	}
	if after.Lflag&unix.ISIG == 0 {
		t.Error("ISIG is clear after OpenTTYCooked, so ctrl-c would not raise SIGINT")
	}
	if *after != *before {
		t.Errorf("termios changed: Lflag %#x -> %#x, Iflag %#x -> %#x",
			before.Lflag, after.Lflag, before.Iflag, after.Iflag)
	}

	// Still usable for everything a follower needs.
	if !ttyWrap.IsTerminal() {
		t.Error("IsTerminal() = false, want true for a pty")
	}
	if err := ttyWrap.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
}

// The interactive path must keep clearing ISIG, since an attached session has to receive ctrl-c as a
// byte rather than have the client killed by it. Asserted so the fix above cannot be "corrected" into
// applying everywhere.
func TestOpenTTYDisablesSignalsForAnInteractiveAttach(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open() error = %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	ttyWrap, err := OpenTTY(tty, tty)
	if err != nil {
		t.Fatalf("OpenTTY() error = %v", err)
	}
	defer ttyWrap.Close()

	after, err := unix.IoctlGetTermios(int(tty.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatalf("reading termios: %v", err)
	}
	if after.Lflag&unix.ISIG != 0 {
		t.Error("ISIG is set after OpenTTY, so ctrl-c would not reach the session")
	}
}
