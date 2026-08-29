package server

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/store"
	"github.com/chancez/cm/internal/transport"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// storeAtClose records what the store held at the instant the listener closed.
//
// A wrapper around the listener rather than a poll of the socket, because polling cannot see this
// window. The gap between closing the listener and writing the resume points is sub-millisecond, so a
// dialing loop misses it and the test passes with the bug present, which is worse than having no test.
// Close is the exact instant `cm server restart` can observe, so reading the store here asks precisely
// the question the next server would ask.
type storeAtClose struct {
	net.Listener
	st   *store.Store
	name string

	once sync.Once
	// rec is what the store held when Close ran, and err is why it could not be read.
	rec store.Session
	err error
	// closed reports whether Close ran at all, so a test can tell "the store was empty at the close"
	// from "there was no close to observe". Those print identically otherwise, since an unread rec holds
	// the same zeros the bug produces.
	closed bool
}

func (l *storeAtClose) Close() error {
	// Read before delegating, since the listener is what stops answering and the read must reflect the
	// moment before anything else can notice.
	l.once.Do(func() {
		l.rec, l.err = l.st.Get(context.Background(), l.name)
		l.closed = true
	})
	return l.Listener.Close()
}

// waitServing blocks until a server on this socket answers an RPC.
//
// Distinct from waitSocket, which only dials: a test that binds its own listener and hands it to Serve
// has a socket that accepts connections before ttrpc has registered it, so a dial proves nothing about
// whether the server is running yet.
func waitServing(t *testing.T, socket string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, cl, err := transport.DialServer(socket)
		if err == nil {
			_, lastErr = cl.List(context.Background(), &serverv1.ListRequest{})
			conn.Close()
			if lastErr == nil {
				return
			}
		} else {
			lastErr = err
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no server answered on %s within 10s: %v", socket, lastErr)
}

// A restart must persist resume points before the socket stops accepting.
//
// This is the ordering `cm server restart` depends on, and it was wrong. Serve called srv.Shutdown()
// before mgr.Close(), and ttrpc.Shutdown closes its listeners as its first step, so the socket stopped
// accepting while the resume points were still unwritten. `cm server restart` decides the old server is
// gone by dialing that socket, so it started the replacement inside the window, and the new server's
// Reconcile read stale positions, or zeros for a session whose points had never been written at all.
//
// The consequence appears in no error. Every adopted session came back through the "output gap detected"
// repaint instead of resuming, which reads as the gap detection doing its job rather than as a resume
// point that was never saved. Found in a live restart where sessions were adopted with from_seq=0 while
// the store held positions in the millions, 26ms after "shutting down on request".
func TestRestartPersistsResumePointsBeforeReleasingTheSocket(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("ordered", "echo ORDERED; sleep 30"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, live := mgr.Get("ordered")
	if !live {
		t.Fatal("session not adopted")
	}
	// Output is consumed so there is a non-zero position to lose. Without this the test would pass for
	// the wrong reason, since zero is also what the bug leaves behind.
	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	readUntil(t, att.reader, "ORDERED")
	sess.detach(att)

	want, wantClient := sess.resumePoints()
	if want == 0 {
		t.Fatal("resume point is 0 after consuming output, so this test could not detect the bug")
	}

	base, err := net.Listen("unix", filepath.Join(dirs.Runtime, "restartorder.sock"))
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	l := &storeAtClose{Listener: base, st: st, name: "ordered"}

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- Serve(serveCtx, l, NewService(mgr)) }()

	// Wait until the server has taken ownership of the listener before asking it to stop. Cancelling
	// immediately is a race the test loses about 6 runs in 20 on Linux: ttrpc's Serve checks whether it
	// is already shutting down *before* registering the listener, so a context cancelled first makes it
	// return straight away having never registered anything, and the later Shutdown then has no listener
	// to close. The wrapper's Close never runs, `rec` keeps its zero value, and the assertion reads that
	// as resume points having been lost. Confirmed by counting: every failure had zero Closes and a store
	// holding the correct (9,9) by the end, so the production ordering was right the whole time.
	//
	// A real client call rather than a dial or a sleep, since the socket is bound by the test's own
	// net.Listen and therefore answers a dial before ttrpc has registered it. A completed RPC proves the
	// accept loop is running, which is the state that makes the shutdown path observable.
	waitServing(t, base.Addr().String())

	cancel()
	if err := <-served; err != nil {
		t.Errorf("Serve() error = %v", err)
	}

	if l.err != nil {
		t.Fatalf("reading the store as the listener closed: %v", l.err)
	}
	// Checked before the values, because an unobserved close leaves the same zeros the bug does, so
	// without this the test can fail while reporting a symptom that never happened.
	if !l.closed {
		t.Fatal("the listener was never closed, so nothing was observed: this test cannot say " +
			"whether resume points were written before the socket was released")
	}
	// Compared as a pair rather than field by field: the two positions count the same output in
	// different spaces, and checking one while the other is wrong is how they diverged before.
	// store.Session itself is not comparable, since it carries a tag map.
	// The field types are what keep the pair honest now: one space each, so a future edit cannot
	// quietly compare the wrong two numbers.
	type resumePoint struct {
		LastSeq   seq.Shim
		ClientSeq seq.Log
	}
	got := resumePoint{l.rec.LastSeq, l.rec.ClientSeq}
	if want := (resumePoint{want, wantClient}); got != want {
		t.Errorf("as the listener closed, store held %+v, want %+v.\n"+
			"A replacement server starting at this instant adopts from the wrong position, so its "+
			"clients repaint instead of resuming.", got, want)
	}
}
