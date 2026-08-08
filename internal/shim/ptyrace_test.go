package shim

import (
	"errors"
	"sync"
	"testing"
)

// Querying or resizing a session while its pty is being closed must not touch a stale descriptor.
//
// The shim answers Size and Resize on RPC goroutines while the shell can exit at any moment, so
// these genuinely race in normal operation: the server asks for a session's state, and the session
// ends while the question is in flight.
//
// The bug was not a crash. pty.Setsize and pty.GetsizeFull reach the fd through os.File.Fd(), which
// returns the raw descriptor with no synchronization, unlike Read and Write which are refcounted
// internally. Once the pty was closed that number was stale, and the kernel is free to hand the same
// number to the next file opened, so the ioctl could land on an unrelated fd. The race detector
// caught it against the closing goroutine.
//
// Requires -race to detect the unsynchronized access; without it the test only checks that the calls
// return an error rather than succeeding against a closed fd.
func TestPtyIoctlsRaceWithClose(t *testing.T) {
	// Repeated, because the window between the last ioctl and the close is small.
	for range 50 {
		sess, err := Start(Config{
			Session: "ptyrace",
			Command: []string{"/bin/sh", "-c", "exit 0"},
			Rows:    24, Cols: 80,
		})
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}

		var wg sync.WaitGroup
		// Hammer the ioctl paths while the shell exits underneath them and the pump releases the pty.
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 20 {
					// Errors are expected once the session ends. What must not happen is an ioctl on
					// a descriptor that has been closed and possibly reused.
					_, _, _ = sess.Size()
					_ = sess.Resize(24, 80, 0, 0)
					_ = sess.ResizeSignal(24, 80, 0, 0)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.releasePty()
		}()
		wg.Wait()

		// After release every ioctl must report the session is over rather than acting on the fd.
		//
		// ErrSessionOver rather than seqlog.ErrClosed. The distinction is not cosmetic: the server has
		// to tell "this session ended" from "something failed", and reusing the output log's error made
		// a resize on a just-exited session surface as "output log is closed", which looked like a
		// genuine fault and failed `cm run` for a command that had succeeded.
		if _, _, err := sess.Size(); !errors.Is(err, ErrSessionOver) {
			t.Fatalf("Size() after release error = %v, want ErrSessionOver", err)
		}
		if err := sess.Resize(24, 80, 0, 0); !errors.Is(err, ErrSessionOver) {
			t.Fatalf("Resize() after release error = %v, want ErrSessionOver", err)
		}
	}
}
