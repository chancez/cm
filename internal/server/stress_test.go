package server

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/chancez/cm/internal/seqlog"
)

// These are stress tests, not example-based ones.
//
// Every bug this package has shipped that took real time to find was a lifetime race: a reader dereferencing a
// subscriber after it was unregistered, a pty ioctl running against a closed file, an exit status replaced by an
// RPC error in a window during attach. None of those reproduce from a single ordered sequence of calls, which is
// why the example-based tests missed them.
//
// The shape here is deliberately uniform: many goroutines calling the methods a real client would, while the
// session ends underneath them. Assertions are about invariants rather than particular values, since the whole
// point is that the interleaving is not fixed. Run under -race, which is where these earn their keep.
//
// Anything that panics, deadlocks, or reports a data race is a bug. A method returning an error because the
// session is over is not.

// stressRounds is how many concurrent operations each test issues.
//
// Enough to interleave, not so many that the suite slows noticeably. These run in well under a second each.
const stressRounds = 50

// Attaching and detaching while a session ends must not panic or deadlock.
//
// The exit-status bug lived exactly here: attach raced the session ending, and in two separate windows the
// status was replaced by an RPC error. The fix was to send Opened and Exited on the ended path, which only
// matters when the two overlap.
func TestConcurrentAttachDetachWhileSessionEnds(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	ctx := context.Background()

	// A session that ends on its own shortly, so attachments land on both sides of the transition.
	rec := startShimInRuntimeDir(t, dirs, "racing", "sleep 0.2")
	if err := mgr.store.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("racing")
	if !ok {
		t.Fatal("session was not adopted")
	}

	var wg sync.WaitGroup
	for range stressRounds {
		wg.Add(1)
		go func() {
			defer wg.Done()

			att, err := sess.attach(nil)
			if err != nil {
				// Expected once the session is over. The contract is that it fails cleanly rather than
				// returning a half-built attachment.
				return
			}
			// Read a little, which is what a real client does and what exercised the reader-lifetime bug.
			// A short deadline so a goroutine that finds nothing to read still detaches.
			readCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			_, _ = att.reader.Next(readCtx)
			cancel()
			sess.detach(att)
		}()
	}
	wg.Wait()

	// The session ends, and says so exactly once with a status.
	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not finish within 10s, which suggests a deadlock")
	}

	ended, _ := sess.Ended()
	if !ended {
		t.Error("Ended() = false after Done() closed")
	}
	// Clients back to zero: every attachment that succeeded was also detached. A leak here is what keeps a
	// session alive forever, or destroys an owned one early.
	if n := sess.Clients(); n != 0 {
		t.Errorf("Clients() = %d after every goroutine detached, want 0", n)
	}
}

// Reading, resizing, and writing while a session ends must fail cleanly rather than crash.
//
// This is the pty-lifetime case. os.File.Fd bypasses the runtime's refcounting, so an ioctl issued from one
// goroutine can run against a descriptor another goroutine has closed, which is not something the race detector
// catches on its own -- it needs the operations to actually overlap.
func TestConcurrentOperationsWhileSessionEnds(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "busy", "sleep 0.2")
	if err := mgr.store.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("busy")
	if !ok {
		t.Fatal("session was not adopted")
	}

	var wg sync.WaitGroup
	for i := range stressRounds {
		wg.Add(3)

		// Writes, which reach the pty.
		go func(i int) {
			defer wg.Done()
			_ = sess.Write(ctx, []byte("echo "+strconv.Itoa(i)+"\n"))
		}(i)

		// Resizes, which are the ioctl path.
		go func(i int) {
			defer wg.Done()
			rows := uint32(20 + i%10)
			_ = sess.Resize(ctx, rows, 80, 0, 0)
		}(i)

		// Metadata reads, which take the same locks the pump writes under.
		go func() {
			defer wg.Done()
			_, _ = sess.Metadata()
			_ = sess.Command()
			_ = sess.Reported()
			_ = sess.CommandRuns()
			_ = sess.LastSeq()
			_, _ = sess.Ended()
		}()
	}
	wg.Wait()

	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not finish within 10s, which suggests a deadlock")
	}
}

// A reader closed while another goroutine is blocked in Next must wake it, not leak it.
//
// This reproduces one of the two seqlog lifetime bugs: unregistering a waiting subscriber left Next asleep
// forever, because nothing would ever signal a subscription that is no longer in the map. Verified by removing
// the wake in Reader.Close, which makes this test fail with the leak message below.
//
// The other bug of that pair, Close reading its subscriber outside the lock, is not caught here and is not meant
// to be: its window is a few instructions wide, and 250 rounds of this do not hit it. internal/seqlog's own
// tests do catch it, as a data race, which is the right layer for it. Recorded so the absence is not mistaken
// for coverage.
func TestConcurrentReaderCloseWhileBlocked(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	ctx := context.Background()

	// A session that produces nothing, so every reader is genuinely blocked rather than draining output.
	rec := startShimInRuntimeDir(t, dirs, "quiet", "sleep 30")
	if err := mgr.store.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("quiet")
	if !ok {
		t.Fatal("session was not adopted")
	}
	t.Cleanup(func() { _, _ = mgr.Kill(context.Background(), "quiet", true, 0) })

	var wg sync.WaitGroup
	for range stressRounds {
		att, err := sess.attach(nil)
		if err != nil {
			t.Fatalf("attach() error = %v", err)
		}

		reading := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			close(reading)
			// Blocks until the detach below closes the reader. Any error is fine; hanging is not, which is
			// what the second of the two bugs caused.
			//
			// A background context deliberately, with no deadline: a deadline would make this return on its
			// own and the test would pass without Close ever having woken anything.
			_, err := att.reader.Next(context.Background())
			if err == nil {
				return
			}
			if !errors.Is(err, io.EOF) && !errors.Is(err, seqlog.ErrClosed) &&
				!errors.Is(err, ErrSessionGone) && !errors.Is(err, context.Canceled) {
				t.Errorf("Next() error = %v, want EOF or a closed-log error", err)
			}
		}()

		// Detach while that read is in flight, which is the interleaving both bugs needed.
		<-reading
		sess.detach(att)
	}

	// Every reader woke. A timeout here is the goroutine leak the second bug caused.
	waited := make(chan struct{})
	go func() {
		wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("a blocked reader was never woken by Close, which leaks a goroutine per attachment")
	}

	if n := sess.Clients(); n != 0 {
		t.Errorf("Clients() = %d after every attachment was detached, want 0", n)
	}
}

// Concurrent metadata subscribers must all be served and all be removable.
//
// Subscription coalesces to a depth of one, so a publisher that finds a full channel drops the update rather
// than blocking. That is correct and makes the bookkeeping the interesting part: a subscriber removed while a
// publish is in flight is the case that can panic on a closed channel.
func TestConcurrentMetadataSubscribers(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "meta", "sleep 30")
	if err := mgr.store.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("meta")
	if !ok {
		t.Fatal("session was not adopted")
	}
	t.Cleanup(func() { _, _ = mgr.Kill(context.Background(), "meta", true, 0) })

	var wg sync.WaitGroup

	// Subscribers coming and going.
	for range stressRounds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := sess.subscribeMetadata()
			select {
			case <-sub.ch:
			case <-time.After(50 * time.Millisecond):
			}
			sess.unsubscribeMetadata(sub)
		}()
	}

	// Publishers running at the same time, as the pump does on every OSC 7 or OSC 2.
	for i := range stressRounds {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sess.publishMetadata(Metadata{Title: "title" + strconv.Itoa(i)})
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("metadata subscribers did not settle within 10s, which suggests a deadlock")
	}
}

// Killing a session while clients are attached must not hang or double-close.
//
// `cm kill` on a session someone is watching is ordinary, and the shutdown path closes the pty and the log
// while readers are still in them.
func TestConcurrentKillWithAttachedClients(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "doomed", "sleep 30")
	if err := mgr.store.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("doomed")
	if !ok {
		t.Fatal("session was not adopted")
	}

	var wg sync.WaitGroup
	// Readers that will be in flight when the kill lands.
	for range 10 {
		att, err := sess.attach(nil)
		if err != nil {
			t.Fatalf("attach() error = %v", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// No deadline, so this only returns when the kill closes the log. A timeout would let the test
			// pass without that having happened.
			for {
				if _, err := att.reader.Next(context.Background()); err != nil {
					return
				}
			}
		}()
	}

	if _, err := mgr.Kill(ctx, "doomed", true, 0); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}

	// Every reader returned rather than hanging on a closed log.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("readers did not return after Kill, which leaks a goroutine per client")
	}
}
