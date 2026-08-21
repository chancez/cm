package server

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fillBacklog dials a listener until it refuses, leaving the connections open, and returns them so
// the caller can decide when to let the queue drain.
//
// This is how a live-but-busy shim is constructed rather than waited for. The kernel stops accepting
// new connections once the accept queue is full, which is the state the bug depended on and which is
// otherwise reachable only by luck under parallel load.
//
// The bound has to clear Linux's queue, which is much deeper than darwin's: measured at 128
// connections on darwin and 4097 on Linux, so 1024 was enough on one platform and silently not on the
// other. A test that cannot fill the queue proves nothing, so it says so rather than passing.
func fillBacklog(t *testing.T, socket string) []net.Conn {
	t.Helper()

	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			c.Close()
		}
	})

	const limit = 8192
	for i := 0; i < limit; i++ {
		c, err := net.Dial("unix", socket)
		if err != nil {
			return held
		}
		held = append(held, c)
	}
	t.Fatalf("dialed %s %d times without it refusing, so the backlog could not be filled",
		socket, limit)
	return nil
}

// refusedByFullQueue reports whether a dial error is the kernel declining because the accept queue is
// full, which darwin and Linux spell differently: ECONNREFUSED and EAGAIN respectively.
func refusedByFullQueue(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EAGAIN)
}

// What each dial outcome actually reports, and which of them socketAbsent may act on.
//
// Asserted because the classification was wrong in the destructive direction: a comment in probeShim
// stated that "ENOENT or ECONNREFUSED mean nothing is there", and the second half is false. Pinned
// here so a platform difference shows up as this test failing rather than as a live session being
// declared dead.
//
// Deliberately not asserting an exact errno for anything but a missing path, because darwin and Linux
// disagree and only the agreement is safe to build on. Measured: a plain file gives ENOTSOCK on darwin
// and ECONNREFUSED on Linux, and a full accept queue gives ECONNREFUSED on darwin and EAGAIN on Linux.
// What matters is that socketAbsent says "gone" for the first case only.
func TestSocketAbsentClassifiesDialErrors(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmsock")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	defer os.RemoveAll(dir)

	// A path that does not exist: the one conclusive answer on both platforms.
	_, missingErr := net.Dial("unix", filepath.Join(dir, "missing.sock"))
	if !errors.Is(missingErr, fs.ErrNotExist) {
		t.Errorf("missing path gave %v, want ErrNotExist", missingErr)
	}
	if !socketAbsent(missingErr) {
		t.Error("socketAbsent(missing path) = false, want true: a session whose socket is gone " +
			"would never be reaped")
	}

	// A real socket whose listener has gone, which is what a shim killed with SIGKILL leaves. Not
	// conclusive by errno, since a busy listener can produce the same thing.
	stale := filepath.Join(dir, "stale.sock")
	sl, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	if ul, ok := sl.(*net.UnixListener); ok {
		// Without this the file is unlinked on close and the case disappears.
		ul.SetUnlinkOnClose(false)
	}
	sl.Close()
	_, staleErr := net.Dial("unix", stale)
	if socketAbsent(staleErr) {
		t.Errorf("socketAbsent(stale socket) = true for %v, want false: the same error comes from a "+
			"live listener whose queue is full, so acting on it strands a shell", staleErr)
	}

	// A live listener whose accept queue is full. The whole point: this is indistinguishable from the
	// stale socket above by errno alone.
	busy := filepath.Join(dir, "busy.sock")
	bl, err := net.Listen("unix", busy)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer bl.Close()
	fillBacklog(t, busy)
	_, busyErr := net.Dial("unix", busy)
	if !refusedByFullQueue(busyErr) {
		t.Fatalf("a full backlog gave %v, want ECONNREFUSED or EAGAIN; this test proves nothing "+
			"without the listener actually refusing", busyErr)
	}
	if socketAbsent(busyErr) {
		t.Error("socketAbsent(live listener with a full queue) = true, want false: a refusal from a " +
			"live shim would mark its session dead and strand the shell")
	}
}

// probeShim must not report a live shim as gone because its accept queue was momentarily full.
//
// This is the destructive half of the bug. probeShim's answer decides whether Reconcile marks a
// session StateDead on server startup, so a false negative leaves a real shell running with a pty
// nothing can reattach to, which is exactly the case AGENTS.md warns about under "clients=0 does not
// mean abandoned".
//
// Constructed rather than raced: the queue is filled deliberately, then drained on a timer, so the
// window the bug needed is present every run instead of once in twenty.
func TestProbeShimTreatsARefusalFromALiveListenerAsAlive(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmsock")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	defer os.RemoveAll(dir)

	socket := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer ln.Close()

	held := fillBacklog(t, socket)
	if len(held) == 0 {
		t.Fatal("the listener refused the first dial, so it was never accepting")
	}

	// Start draining shortly, as a busy shim getting back to its accept loop would. Well inside
	// socketRefusalGrace, so a correct probe retries and finds it.
	go func() {
		time.Sleep(20 * time.Millisecond)
		for _, c := range held {
			c.Close()
		}
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	alive, err := probeShim(context.Background(), socket)
	if !alive {
		t.Errorf("probeShim = (false, %v) for a live listener whose backlog was full, want alive; "+
			"Reconcile would mark this session dead and strand its shell", err)
	}
}

// A socket nothing will ever serve is still reported as not listening, so a genuinely dead session
// can be cleaned up.
//
// The control for the test above. Without it, "always report alive" would pass that one, and a dead
// session would be kept forever with no way to reap it.
func TestProbeShimStillReportsAStaleSocketAsGone(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmsock")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	defer os.RemoveAll(dir)

	socket := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	ln.Close()

	start := time.Now()
	alive, _ := probeShim(context.Background(), socket)
	elapsed := time.Since(start)

	if alive {
		t.Error("probeShim = true for a socket whose listener is gone, want false: a dead session " +
			"would never be reaped")
	}
	// Bounded so the retry cannot grow without this failing. Generous against the grace itself,
	// since the point is that it gives up rather than how promptly.
	if elapsed > 2*socketRefusalGrace {
		t.Errorf("probeShim took %s on a stale socket, want it to give up within about %s",
			elapsed, socketRefusalGrace)
	}
}

// waitForSocketFree must not report a path free while a live listener holds it, even when that
// listener is refusing connections.
//
// The milder half of the same bug, and the one the e2e suite surfaced: returning early let a
// replacement shim try to bind a path the old shim still held, which fails with a socket error that
// says nothing about the cause.
func TestWaitForSocketFreeWaitsThroughARefusalFromALiveListener(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmsock")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	defer os.RemoveAll(dir)

	socket := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	held := fillBacklog(t, socket)
	if len(held) == 0 {
		t.Fatal("the listener refused the first dial, so it was never accepting")
	}

	// The listener starts accepting shortly, keeps answering for a while, then closes for real. A
	// correct wait returns only after the close.
	//
	// Accept is what clears the refusal, and that is worth stating because the obvious alternative
	// does not work: closing the held client ends leaves the connections in the accept queue, so the
	// listener keeps refusing. Verified directly, since an earlier version of this test "drained" by
	// closing them and stayed refusing throughout, which made the test fail against a working fix.
	//
	// Accepting has to begin inside socketRefusalGrace, or the wait gives up on the sustained refusal
	// and returns while the listener is still up. That is correct behavior rather than a bug, since a
	// sustained refusal is how a stale socket is recognised, but it makes the timing load-bearing. It
	// begins as soon as the goroutine is scheduled rather than after a delay, which leaves the whole
	// grace as margin: the wait's first dial is refused whatever happens, since fillBacklog returned
	// before the wait started.
	//
	// The flag is set before the close, not after, and that ordering is what makes the check below
	// sound. This Close is the only thing that can unlink the socket, so a wait that saw the path gone
	// must have seen the flag too. Signalling afterwards is a race the test loses on its own: the
	// polling goroutine runs alongside this one and could observe the unlinked socket in the instant
	// between Close returning and the signal being sent, which reported a correct wait as an early
	// return. Measured at 2 runs in 40 on unmodified code, and the same window cost the sibling test in
	// socketfree_test.go 2 runs in 30.
	var closing atomic.Bool
	go func() {
		accepting := make(chan struct{})
		go func() {
			close(accepting)
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		<-accepting
		for _, c := range held {
			c.Close()
		}
		// Comfortably longer than the grace, so a return before the close can only mean the refusal
		// was misread rather than the grace legitimately expiring.
		time.Sleep(4 * socketRefusalGrace)
		closing.Store(true)
		ln.Close()
	}()

	if err := waitForSocketFree(context.Background(), socket, 5*time.Second); err != nil {
		t.Fatalf("waitForSocketFree = %v, want nil once the listener closed", err)
	}
	if !closing.Load() {
		t.Error("waitForSocketFree returned while the listener was still up, so a replacement " +
			"shim would fail to bind")
	}
}
