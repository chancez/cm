package server

import (
	"context"
	"testing"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// The snapshot a fresh attach replays must be serialized at the size the attaching client will
// display it at, which means the resize has to happen before the screen is taken.
//
// Taking it the other way round produces two symptoms that look unrelated and are the same bug. The
// snapshot describes lines wrapped for the old width, so a client of a different width wraps them
// again and rows arrive spliced: the tail of one line appears on the end of the previous row. And the
// resize that follows makes the shell redraw, so anything its SIGWINCH handler emits is generated
// *after* the screen was serialized and interleaves with the replay. A zsh TRAPWINCH setting the
// title that way put a literal "]2;" on screen, an OSC 2 whose ESC had already been consumed by the
// bytes ahead of it.
//
// Tested through Service.Attach rather than Session, and that is the point. This ordering was fixed
// once, in "Fix attach ordering, and two client display bugs", and regressed when the configurable
// resize policy moved the sizing block below sess.attach to get at the attach token. The test written
// with that fix drives sess.Resize and sess.attach itself, in the correct order, so it kept passing
// while the service did the opposite. Only a test that calls the RPC can see which order the service
// picks.
func TestAttachResizesBeforeSnapshotting(t *testing.T) {
	term := &fakeTerminal{rows: 24, cols: 80, restore: []byte("SNAPSHOT")}
	mgr, st, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		term.rows, term.cols = rows, cols
		return term, nil
	})
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("order", "sleep 5"))
	rec.State = "running"
	rec.Rows, rec.Cols = 24, 80
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, ok := mgr.Get("order"); !ok {
		t.Fatal("session was not adopted")
	}

	// A client attaching at a size the session does not already have, which is the only case where
	// the ordering is observable.
	svc := NewService(mgr)
	stream := newHeldFakeStream(ctx, openReq(&serverv1.Open{
		Session: "order",
		Rows:    40,
		Cols:    120,
	}))
	done := make(chan error, 1)
	go func() { done <- svc.Attach(ctx, stream) }()

	// Wait for the screen to have been taken, then stop the attach.
	deadline := time.Now().Add(5 * time.Second)
	for {
		term.mu.Lock()
		taken := term.restoredAt > 0
		term.mu.Unlock()
		if taken {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the screen was never serialized")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stream.closeStream()
	<-done

	term.mu.Lock()
	gotRows, gotCols := term.restoreRows, term.restoreCols
	term.mu.Unlock()

	if gotRows != 40 || gotCols != 120 {
		t.Errorf("screen serialized at %dx%d, want the client's 40x120: the resize landed after the snapshot",
			gotRows, gotCols)
	}
}

// Reserving a sizing slot before attaching means there is a window where the attach can fail with the
// reservation already made, so it has to be given back.
//
// Left behind, the entry is enough to hold sizing under the default policy: registerClientSize hands
// leadership to an attaching client only when nothing else holds it, so a stale reservation makes the
// session keep sizing itself to a window that never arrived, and no later client can take it. The
// symptom would be a session stuck at the size of a failed attach.
func TestFailedAttachReleasesItsSizingSlot(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("released", "exit 0"))
	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// A reservation that is then handed back, which is what the service does when attach fails.
	tok := sess.reserveClient()
	sess.registerClientSize(tok, 40, 120, 0, 0, false)
	sess.releaseClient(tok)

	sess.mu.Lock()
	sizes := len(sess.clientSizes)
	leader := sess.leader
	sess.mu.Unlock()

	if sizes != 0 {
		t.Errorf("clientSizes holds %d entries after release, want 0", sizes)
	}
	if leader != nil {
		t.Error("the released token is still the leader, so no later client can size the session")
	}

	// And a client attaching afterwards takes sizing, which is the behavior the leak would deny it.
	next := sess.reserveClient()
	if _, _, _, _, resize := sess.registerClientSize(next, 24, 80, 0, 0, false); !resize {
		t.Error("the next client did not get sizing, want the stale reservation not to hold it")
	}
}
