package server

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A socket nothing listens on is free at once.
//
// The case that must not wait: a shim that died without cleaning up leaves its socket file behind, and the
// next shim unlinks it when it binds. Treating the file's existence as "busy" would stall every create
// after a crash for the full timeout.
func TestWaitForSocketFreeReturnsImmediatelyForAStaleFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmsock")
	if err != nil {
		t.Fatalf("making a temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	socket := filepath.Join(dir, "s.sock")
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatalf("writing a stale socket file: %v", err)
	}

	start := time.Now()
	if err := waitForSocketFree(context.Background(), socket, 5*time.Second); err != nil {
		t.Fatalf("waitForSocketFree on a stale file = %v, want nil", err)
	}
	// Generous, since the point is "did not poll to the timeout" rather than a latency budget.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s for a stale socket file, want an immediate return", elapsed)
	}
}

// A path that was never a socket at all is free.
func TestWaitForSocketFreeReturnsImmediatelyForAMissingFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmsock")
	if err != nil {
		t.Fatalf("making a temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	if err := waitForSocketFree(context.Background(), filepath.Join(dir, "absent.sock"), 5*time.Second); err != nil {
		t.Fatalf("waitForSocketFree on a missing path = %v, want nil", err)
	}
}

// A live listener blocks until it closes, then the wait returns.
//
// This is the bug's mechanism at the unit level. A shim stays reachable for a grace period after its shell
// exits so its exit status can still be collected; creating a session under the same name inside that window
// used to spawn a shim that could not bind, which died with "already served by a live shim". Waiting here
// makes the create path outlast the old shim instead of racing it.
func TestWaitForSocketFreeWaitsForALiveListenerToClose(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmsock")
	if err != nil {
		t.Fatalf("making a temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	socket := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listening on %s: %v", socket, err)
	}
	defer ln.Close()

	// Closed from another goroutine, which is what the real shim's exit looks like from here.
	closed := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		ln.Close()
		close(closed)
	}()

	start := time.Now()
	if err := waitForSocketFree(context.Background(), socket, 5*time.Second); err != nil {
		t.Fatalf("waitForSocketFree = %v, want nil once the listener closed", err)
	}
	elapsed := time.Since(start)

	select {
	case <-closed:
	default:
		t.Fatal("waitForSocketFree returned while the listener was still up, " +
			"so a replacement shim would fail to bind")
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("returned after %s, want at least the 200ms the listener stayed up", elapsed)
	}
}

// A listener that never closes produces an error naming the timeout.
//
// Reported rather than spawning anyway: a shim that will not exit is a real problem, and spawning into it
// fails with a socket error that says nothing about the cause.
func TestWaitForSocketFreeTimesOutOnAListenerThatStays(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmsock")
	if err != nil {
		t.Fatalf("making a temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	socket := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listening on %s: %v", socket, err)
	}
	defer ln.Close()

	err = waitForSocketFree(context.Background(), socket, 150*time.Millisecond)
	if err == nil {
		t.Fatal("waitForSocketFree = nil for a listener that never closed, want an error")
	}
	if want := "still answering after 150ms"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// A cancelled context ends the wait.
//
// The create path's context carries the client's timeout, so a caller that gives up must not leave this
// polling for the full release timeout.
func TestWaitForSocketFreeHonorsContextCancellation(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmsock")
	if err != nil {
		t.Fatalf("making a temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	socket := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listening on %s: %v", socket, err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	if err := waitForSocketFree(ctx, socket, time.Minute); err != context.Canceled {
		t.Errorf("waitForSocketFree = %v, want context.Canceled", err)
	}
}
