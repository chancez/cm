package client

import (
	"testing"
	"time"

	"github.com/creack/pty"
)

// Suspending stops reading the terminal and resuming picks it up again, with nothing lost in between and
// nothing reported as a failure.
//
// The whole feature rests on this. While the overlay hands the terminal to `cm tui`, this process must not
// be reading it -- two readers on one terminal means the keystroke goes to whichever the kernel wakes
// second, which is the bug docs/tui.md measured from the other direction. And a suspension must not look
// like the terminal failing or the window closing, either of which ends the attachment.
func TestTerminalInputSuspendAndResume(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open() error = %v", err)
	}
	t.Cleanup(func() {
		ptmx.Close()
		tty.Close()
	})
	wrapped, err := OpenTTY(tty, tty)
	if err != nil {
		t.Fatalf("OpenTTY() error = %v", err)
	}
	t.Cleanup(func() { wrapped.Close() })

	in, err := newTerminalInput(wrapped)
	if err != nil {
		t.Fatalf("newTerminalInput() error = %v", err)
	}
	t.Cleanup(in.suspend)

	read := func(what string) string {
		t.Helper()
		select {
		case data, ok := <-in.data:
			if !ok {
				t.Fatalf("%s: the data channel was closed, which the attach loop reads as the window closing",
					what)
			}
			return string(data)
		case err := <-in.errs:
			t.Fatalf("%s: reported an error: %v", what, err)
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: nothing arrived within 5s", what)
		}
		return ""
	}

	if _, err := ptmx.Write([]byte("a")); err != nil {
		t.Fatalf("writing to the pty: %v", err)
	}
	if got := read("before suspending"); got != "a" {
		t.Errorf("read %q, want %q", got, "a")
	}

	// Suspended: the reader is gone, and neither channel has been closed or written to.
	in.suspend()
	select {
	case data, ok := <-in.data:
		t.Fatalf("suspending delivered %q (open=%v), want nothing: the reader should be gone", data, ok)
	case err := <-in.errs:
		t.Fatalf("suspending reported %v, want nothing: a cancellation is not a failure", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Bytes written while suspended wait in the pty rather than being lost, which is what makes handing the
	// terminal to a child and taking it back safe.
	if _, err := ptmx.Write([]byte("b")); err != nil {
		t.Fatalf("writing to the pty while suspended: %v", err)
	}
	if err := in.resume(); err != nil {
		t.Fatalf("resume() error = %v", err)
	}
	if got := read("after resuming"); got != "b" {
		t.Errorf("read %q after resuming, want %q: input typed while suspended was lost", got, "b")
	}

	// And suspending twice is not an error, so a caller need not track whether it already did.
	in.suspend()
	in.suspend()
}
