package server

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// An evicted client must leave, and its session must survive.
//
// This is the property the whole command rests on, and getting it wrong turns a detach into a kill.
// Weaker than it was: while sessions could be owned, an eviction mistaken for a client that vanished
// destroyed the shell, and this test was the seam-level guard for that. Nothing kills a session on
// disconnect now, so what remains to assert is that the attach returns and the client is told why.
func TestEvictedClientLeavesAndSessionSurvives(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("evictown", "sleep 5"))
	rec.State = "running"
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	svc := NewService(mgr)
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Held open, so the attachment is still live when the eviction arrives. A stream that ended would
	// detach on its own and the test would pass without exercising anything.
	stream := newHeldFakeStream(streamCtx,
		openReq(&serverv1.Open{Session: "evictown", Rows: 24, Cols: 80}),
	)

	attachDone := make(chan error, 1)
	go func() { attachDone <- svc.Attach(streamCtx, stream) }()

	sess := waitForClients(t, mgr, "evictown", 1)

	if got := sess.EvictClients(); got != 1 {
		t.Errorf("EvictClients() = %d, want 1", got)
	}

	// The attach must return on its own, without the stream closing. That is the point of the eviction
	// channel: a client blocked waiting for output from a quiet session has to be woken, and a flag
	// checked on the next byte would leave an idle session appearing to hang.
	select {
	case err := <-attachDone:
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Attach() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		stream.closeStream()
		t.Fatal("the attach did not return after its client was evicted, so `cm detach` would hang")
	}

	// The client was told, rather than just having its stream closed. Without this the client reads a
	// clean close as the server going away and reconnects, silently undoing the detach.
	var told bool
	for _, resp := range stream.sent() {
		if resp.GetDetached() != nil {
			told = true
		}
	}
	if !told {
		t.Errorf("server sent %d messages and none was Detached, so the client would treat the "+
			"eviction as an outage and reattach", len(stream.sent()))
	}

	// The session survived, which is the difference between a detach and a kill.
	if sess, ok := mgr.Get("evictown"); !ok {
		t.Error("the session was destroyed by an eviction, want it still running")
	} else if ended, _ := sess.Ended(); ended {
		t.Error("the session ended after an eviction, want it still running")
	}
}

// Evicting a session with nothing attached is a satisfied request, not an error.
//
// What makes `cm detach` safe to call without checking first, and what a teardown script relies on.
func TestEvictClientsWithNoClientsIsZero(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("evictnone", "sleep 5"))

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	if got := sess.EvictClients(); got != 0 {
		t.Errorf("EvictClients() = %d, want 0 for a session with no clients", got)
	}
}

// Every client is evicted, and the count says how many.
//
// A bool would not distinguish one window letting go from four, which is the thing a caller clearing a
// session wants to know.
func TestEvictClientsCountsEveryClient(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("evictmany", "sleep 5"))

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	var atts []attachment
	for range 3 {
		att, err := sess.attach(nil)
		if err != nil {
			t.Fatalf("attach() error = %v", err)
		}
		atts = append(atts, att)
	}

	if got := sess.EvictClients(); got != 3 {
		t.Errorf("EvictClients() = %d, want 3", got)
	}

	// Each attachment's own channel closed, so every client is woken rather than only the first.
	for i, att := range atts {
		select {
		case <-att.evict:
		default:
			t.Errorf("attachment %d was not evicted, so its client would keep running", i)
		}
	}

	// A second call is idempotent rather than a double close, which would panic. A retry against a
	// session whose clients have not finished tearing down is the ordinary case for that.
	if got := sess.EvictClients(); got != 0 {
		t.Errorf("second EvictClients() = %d, want 0: an already-evicted client was counted twice", got)
	}

	for _, att := range atts {
		sess.detach(att)
	}

	// Detaching drops the channels, so an evicted-and-torn-down session holds nothing.
	sess.mu.Lock()
	left := len(sess.evicts)
	sess.mu.Unlock()
	if left != 0 {
		t.Errorf("%d eviction channels left after every client detached, want 0", left)
	}
}

// waitForClients waits until a session reports the expected client count.
//
// Polled rather than slept, since the attach registers on its own goroutine. Waiting for the state
// instead of racing it is what keeps this deterministic.
func waitForClients(t *testing.T, mgr *Manager, name string, want int64) *Session {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sess, ok := mgr.Get(name); ok && sess.Clients() == want {
			return sess
		}
		time.Sleep(5 * time.Millisecond)
	}
	sess, ok := mgr.Get(name)
	if !ok {
		t.Fatalf("session %q is not registered", name)
	}
	t.Fatalf("session %q has %d clients, want %d", name, sess.Clients(), want)
	return nil
}
