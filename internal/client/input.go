package client

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/muesli/cancelreader"
)

// terminalInput reads the terminal on its own goroutine, and can be stopped so that something else may
// read the same terminal.
//
// The stopping is the whole point, and it is the wall this hit from the other side. internal/client's
// reader used to be a plain loop over tty.Read, left blocked in the kernel on purpose because a blocked
// read cannot be cancelled -- `cm attach` exits immediately afterwards, so nothing noticed. `cm tui` then
// wanted to call into this package and could not: the leftover reader stayed on the terminal and stole
// exactly one keystroke per attachment, measured, and closing the descriptor did not help because Go
// defers the real close until the outstanding read finishes. docs/tui.md records that, and the conclusion
// there was that a cancellable reader is the right answer for both directions.
//
// This is that answer, and it is what lets the overlay hand the terminal to a child at all: without it a
// child would compete with this reader for every keystroke, and the loser is whichever the kernel wakes
// second.
//
// github.com/muesli/cancelreader was already in the module graph, indirectly, through bubbletea. It sets
// the descriptor non-blocking and polls it alongside a pipe it can write to, which is how a read in
// progress is interrupted rather than abandoned.
type terminalInput struct {
	// data carries what was read, and errs why reading stopped. Both outlive a suspension, because the
	// attach loop selects on them and a channel replaced mid-flight is a channel somebody is still
	// waiting on.
	data chan []byte
	errs chan error

	// tty is what to read. Nil in a test that feeds the channels directly, which is also what makes
	// suspend and resume no-ops there.
	tty *TTY

	mu sync.Mutex
	// reader is the current cancellable reader, nil while suspended.
	reader cancelreader.CancelReader
	// stopped is closed by the goroutine as it exits, so suspend can wait for it. A suspension that
	// returned before the reader was gone would leave two readers on one terminal, which is the bug this
	// type exists to prevent.
	stopped chan struct{}
}

// newTerminalInput starts reading the terminal.
func newTerminalInput(tty *TTY) (*terminalInput, error) {
	in := &terminalInput{
		// Buffered so a burst of keystrokes, or a paste, does not block the reader while the loop is busy
		// writing to the terminal.
		data: make(chan []byte, 16),
		errs: make(chan error, 1),
		tty:  tty,
	}
	if err := in.resume(); err != nil {
		return nil, err
	}
	return in, nil
}

// newTestInput wraps channels a test writes to directly, with no reader behind them.
func newTestInput(data chan []byte, errs chan error) *terminalInput {
	return &terminalInput{data: data, errs: errs}
}

// suspend stops reading and waits for the reader to be gone.
//
// Returns once nothing in this process is reading the terminal, which is what a caller about to hand the
// terminal to a child process needs to know. Idempotent, so a caller does not have to track whether it
// already suspended.
func (in *terminalInput) suspend() {
	in.mu.Lock()
	reader, stopped := in.reader, in.stopped
	in.reader, in.stopped = nil, nil
	in.mu.Unlock()

	if reader == nil {
		return
	}
	reader.Cancel()
	// Waited on rather than assumed: Cancel only asks, and the goroutine may be mid-copy of a read that
	// already returned bytes.
	<-stopped
	reader.Close()
}

// resume starts reading again after a suspension, or for the first time.
func (in *terminalInput) resume() error {
	if in.tty == nil {
		// A test's channels, which need no reader.
		return nil
	}

	in.mu.Lock()
	defer in.mu.Unlock()
	if in.reader != nil {
		return nil
	}

	reader, err := cancelreader.NewReader(in.tty.in)
	if err != nil {
		return fmt.Errorf("preparing to read the terminal: %w", err)
	}
	stopped := make(chan struct{})
	in.reader, in.stopped = reader, stopped
	go in.read(reader, stopped)
	return nil
}

// read forwards terminal input until it fails or is cancelled.
func (in *terminalInput) read(reader cancelreader.CancelReader, stopped chan struct{}) {
	defer close(stopped)

	buf := make([]byte, inputReadSize)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			// Copied, since the buffer is reused on the next read.
			data := make([]byte, n)
			copy(data, buf[:n])
			select {
			case in.data <- data:
			case <-stopped:
				// Unreachable while this goroutine is the only closer, and here so a future change that
				// closes stopped elsewhere cannot deadlock a suspension behind a full channel.
				return
			}
		}
		if err != nil {
			if errors.Is(err, cancelreader.ErrCanceled) {
				// A deliberate stop, so the channels stay open and nothing downstream learns of it. Closing
				// data would be read as the window having gone, which ends the session's client.
				return
			}
			select {
			case in.errs <- err:
			default:
				// One error is enough to end an attachment, and a second would block here forever.
			}
			// Deliberately not closed on an error either: the error channel is what the loop acts on, and
			// closing data would race that with an "input ended" reading of the same event.
			return
		}
	}
}

// interrupted reports whether an error from the terminal is a cancellation rather than a real failure.
//
// Exists because the reader is cancelled while the terminal is handed to a child, and a cancellation must
// not be reported as the terminal having failed: that ends the attachment.
func interrupted(err error) bool {
	return errors.Is(err, cancelreader.ErrCanceled) || errors.Is(err, os.ErrClosed)
}
