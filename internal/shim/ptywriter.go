package shim

import (
	"io"
	"sync"
)

// ptyWriter is the pty's ordering point: one writer for a stream that has several things to say.
//
// docs/architecture.md requires exactly one writer per shared byte stream, and names two, the pty and
// each client's terminal. The terminal half has been enforced in code since a window title landed inside
// nvim's SGR. This is the pty half, and it has several writers by construction: a client's typing, a
// client's answer to a query cm proxied, cm's own emulator replies, and the in-band resize reports, each
// arriving at Session.Write on its own goroutine.
//
// The tty layer was believed to serialize them, and a test measured that it did. It does on darwin. It
// does not on Linux, which is what makes the difference invisible until someone runs the suite there: a
// write larger than the slave's input buffer, 65536 bytes by default, cannot be accepted in one syscall,
// os.File.Write loops over the remainder, and another goroutine's write lands in the gap between
// syscalls. Measured in the Linux test image at 3 interleaved writes in 120 runs of a 262148-byte
// payload against 4000 short ones, against 0 in 90 runs on darwin.
//
// What it costs is an escape sequence cut in half. An OSC 52 clipboard reply was measured at 18008
// bytes; a resize report inserted into the middle of one aborts the OSC, and the rest of the base64
// prints as text in whatever program was running. That is the same failure as the window title, on the
// other stream, so it gets the same fix rather than a second theory.
//
// Held across the whole Write, which is the point: os.File.Write is a loop, and the invariant is that
// nobody else writes until it finishes. A program that has stopped reading blocks it with the mutex
// held, so other writers queue behind it, and that is not a new hazard: before this they blocked on
// their own writes into the same full buffer.
type ptyWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// Write delivers p to the pty, and nothing else writes until it is done.
func (p *ptyWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.w.Write(b)
}
