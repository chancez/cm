package server

import (
	"context"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// `cm attach --no-attach` on a running session leaves its size alone.
//
// It registered as a sizing client like any other, and a detached session under resize_policy = leader has
// no leader, so the first Open claims it. Measured in a sandbox: a detached 40x150 session went to 24x80, the
// size --no-attach sends, and its shell was signalled to redraw for a window that never existed.
func TestNoAttachDoesNotResizeARunningSession(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}

	sess, term := sessionForResumeSizing(t)

	// A window sizes the session and leaves, which leaves leadership unheld.
	window := sess.reserveClient()
	if err := svc.sizeForAttach(ctx, sess, window, &serverv1.Open{Rows: 40, Cols: 150}); err != nil {
		t.Fatalf("sizing the window: %v", err)
	}
	a, err := sess.attach(nil, window)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	sess.detach(a)

	leaving := sess.reserveClient()
	defer sess.releaseClient(leaving)
	if err := svc.sizeForAttach(ctx, sess, leaving, &serverv1.Open{Rows: 24, Cols: 80, NoAttach: true}); err != nil {
		t.Fatalf("sizing the no-attach client: %v", err)
	}

	state, err := sess.shim.State(ctx, &shimv1.StateRequest{})
	if err != nil {
		t.Fatalf("reading the pty size: %v", err)
	}
	term.mu.Lock()
	got := noAttachSizing{
		ptyRows:   state.Rows,
		ptyCols:   state.Cols,
		modelRows: term.rows,
		modelCols: term.cols,
	}
	term.mu.Unlock()
	want := noAttachSizing{ptyRows: 40, ptyCols: 150, modelRows: 40, modelCols: 150}
	if got != want {
		t.Errorf("after a no-attach Open: %+v\nwant %+v", got, want)
	}
}

// noAttachSizing is the session's size as the pty and the model each report it.
type noAttachSizing struct {
	ptyRows   uint32
	ptyCols   uint32
	modelRows uint16
	modelCols uint16
}
