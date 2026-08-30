package server

import (
	"context"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/shim"
)

// A client that upgrades in place keeps its size on the session's books.
//
// A resume suppresses the resize, deliberately: the pty already matches, since a resume is the same
// terminal coming back, and resizing makes the shell redraw for nothing. Skipping the whole of
// sizeForAttach to get that also skipped registerClientSize, which is the only writer of a client's rows
// and cols, so every client that upgraded was left recorded as 0x0.
//
// Asserted through the consequence rather than the field. Both policies that read a size treat zero as
// "has not reported one", so under resize_policy = smallest an upgraded window stopped constraining the
// session: the smaller window upgrades, drops out of the calculation, and the session grows to the other
// window's size with the small window's shell still displaying it. Two clients at different sizes is the
// minimum that shows it, which is why one client hid this.
func TestAnUpgradedClientStillConstrainsTheSmallestSize(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}

	sess, term := sessionForResumeSizing(t)
	sess.resizePolicy = ResizeSmallest

	// The small window, and the one that will upgrade. Its size is the constraint that has to survive.
	small := sess.reserveClient()
	if err := svc.sizeForAttach(ctx, sess, small, &serverv1.Open{Rows: 24, Cols: 80}); err != nil {
		t.Fatalf("sizing the first client: %v", err)
	}
	a, err := sess.attach(nil, small)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}

	// The large window, which is what the session would grow to if the small one stopped counting.
	large := sess.reserveClient()
	if err := svc.sizeForAttach(ctx, sess, large, &serverv1.Open{Rows: 40, Cols: 100}); err != nil {
		t.Fatalf("sizing the second client: %v", err)
	}
	b, err := sess.attach(nil, large)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(b)

	// The upgrade: the same window's stream drops and its replacement resumes. Deliberately at a
	// different size from the one it left with, so a size that is registered and one that is acted on
	// are distinguishable. A real re-exec comes back at the size it left at; a terminal resized during
	// the gap is what makes that not always true.
	//
	// Its detach is also what makes the session grow to the large window's size, which is the state the
	// resume has to correct rather than the state it can assume.
	sess.rememberOrder(clientPID, a.token)
	sess.detach(a)

	back := sess.reserveClient()
	if err := svc.sizeForAttach(ctx, sess, back, &serverv1.Open{
		Rows: 30, Cols: 90,
		// What makes this a resume, and it is read from the request rather than passed alongside it, so
		// this drives the same decision the RPC handler does.
		ResumeFromSeq: new(uint64(1)),
	}); err != nil {
		t.Fatalf("sizing the resuming client: %v", err)
	}
	resumed, err := sess.attach(new(seq.Log(1)), back)
	if err != nil {
		t.Fatalf("attach() on resume error = %v", err)
	}
	defer sess.detach(resumed)

	rows, cols, ok := smallestFor(t, sess)
	term.mu.Lock()
	modelRows, modelCols := term.rows, term.cols
	term.mu.Unlock()

	got := resumeSizing{
		smallestRows: rows,
		smallestCols: cols,
		haveSmallest: ok,
		modelRows:    modelRows,
		modelCols:    modelCols,
	}
	// The resuming client counts at the size it reported, and the session is brought back to it: the
	// large window alone had grown it to 40x100 while this one was away.
	want := resumeSizing{
		smallestRows: 30,
		smallestCols: 90,
		haveSmallest: true,
		modelRows:    30,
		modelCols:    90,
	}
	if got != want {
		t.Errorf("after an upgrade: %+v\nwant %+v\n"+
			"a zero smallest means the resuming client was recorded as 0x0, so it no longer constrains "+
			"the session and the window it is displayed in will be sized for another one", got, want)
	}
}

// resumeSizing is what the session believes after a client upgraded in place.
type resumeSizing struct {
	smallestRows uint16
	smallestCols uint16
	haveSmallest bool
	modelRows    uint16
	modelCols    uint16
}

// smallestFor reports what ResizeSmallest would settle on right now.
func smallestFor(t *testing.T, s *Session) (rows, cols uint16, ok bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.smallestLocked()
}

// sessionForResumeSizing is sessionForSizing with the terminal handed back, so a test can read the size
// the model was left at.
func sessionForResumeSizing(t *testing.T) (*Session, *fakeTerminal) {
	t.Helper()
	rec := startShimFor(t, shim.Config{
		Session: "resumesizing",
		Command: []string{"/bin/sh", "-c", "sleep 30"},
		Rows:    24, Cols: 80,
	})
	term := &fakeTerminal{restore: []byte("SCREEN")}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess, term
}
