package server

import (
	"context"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"

	"github.com/chancez/cm/internal/seq"
)

// An upgrade must not resize the session under the default policy.
//
// Reported as "terminals sometimes resize after an upgrade", on a session that had several clients and
// has one left. That is the shape, and the size the session is sitting at is not a mistake being
// corrected: releaseClientSize leaves leadership *unclaimed* when the leader detaches rather than
// transferring it, so the session deliberately keeps its size until somebody types. Reflowing a window
// nobody touched is the surprise that rule exists to avoid, and an upgrade is not a touch.
//
// So a resuming client registers its size, which it must, and does not acquire sizing it did not hold.
// Under ResizeSmallest it still applies, because there the size is a function of every attached client
// and the gap really did change it. See TestAnUpgradedClientStillConstrainsTheSmallestSize.
func TestAnUpgradeDoesNotReflowTheSurvivingWindow(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}

	sess, term := sessionForResumeSizing(t)
	// The default, stated rather than inherited: this is the policy nearly everyone runs.
	sess.resizePolicy = ResizeLeader

	// The wide window, which types and so owns sizing.
	wide := sess.reserveClient()
	if err := svc.sizeForAttach(ctx, sess, wide, &serverv1.Open{Rows: 40, Cols: 100}); err != nil {
		t.Fatalf("sizing the wide client: %v", err)
	}
	a, err := sess.attach(nil, wide)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	sess.noteClientInput(a.token)

	// The narrow window, which never types, so it does not take sizing and displays a session wider than
	// itself. That is the documented trade, not a bug.
	narrow := sess.reserveClient()
	if err := svc.sizeForAttach(ctx, sess, narrow, &serverv1.Open{Rows: 24, Cols: 80}); err != nil {
		t.Fatalf("sizing the narrow client: %v", err)
	}
	b, err := sess.attach(nil, narrow)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}

	// The wide window closes. Leadership is dropped rather than handed over, so the session stays at
	// 40x100 with only the narrow window watching it.
	sess.detach(a)
	if r, c := modelSizeOf(term); r != 40 || c != 100 {
		t.Fatalf("after the wide window left the session is %dx%d, want 40x100 held: "+
			"the rest of this test is about not disturbing that", r, c)
	}

	// The narrow window upgrades in place.
	sess.rememberOrder(clientPID+1, b.token)
	sess.detach(b)

	back := sess.reserveClient()
	if err := svc.sizeForAttach(ctx, sess, back, &serverv1.Open{
		Rows: 24, Cols: 80,
		ResumeFromSeq: new(uint64(1)),
	}); err != nil {
		t.Fatalf("sizing the resuming client: %v", err)
	}
	resumed, err := sess.attach(new(seq.Log(1)), back)
	if err != nil {
		t.Fatalf("attach() on resume error = %v", err)
	}
	defer sess.detach(resumed)

	rows, cols := modelSizeOf(term)
	registered := registeredSizeOf(t, sess, back)
	got := resumeLeaderState{
		modelRows:      rows,
		modelCols:      cols,
		registeredRows: registered.rows,
		registeredCols: registered.cols,
		leaderIsResumed: func() bool {
			sess.mu.Lock()
			defer sess.mu.Unlock()
			return sess.leader == back
		}(),
	}
	// The size is untouched by the upgrade, the client's own size is on the books for the next policy
	// decision, and the upgrade did not make this window the leader.
	want := resumeLeaderState{
		modelRows:       40,
		modelCols:       100,
		registeredRows:  24,
		registeredCols:  80,
		leaderIsResumed: false,
	}
	if got != want {
		t.Errorf("after an upgrade: %+v\nwant %+v\n"+
			"an upgrade that resizes reflows a window whose user did not ask for it, which is what "+
			"releaseClientSize leaves leadership unclaimed to avoid", got, want)
	}

	// And typing still moves sizing, so this is a delay rather than a client that can never size again.
	sess.noteClientInput(back)
	if r, c, _, _, ok := sess.claimLeadership(back); !ok || r != 24 || c != 80 {
		t.Errorf("claimLeadership() after typing = %dx%d ok=%v, want 24x80 ok=true: "+
			"a resuming client that registered no size cannot bring the session to its own", r, c, ok)
	}
}

// resumeLeaderState is what the session believes after a client upgraded under ResizeLeader.
type resumeLeaderState struct {
	modelRows       uint16
	modelCols       uint16
	registeredRows  uint16
	registeredCols  uint16
	leaderIsResumed bool
}

// modelSizeOf reports the size the terminal model was last resized to, which tracks the pty.
func modelSizeOf(term *fakeTerminal) (rows, cols uint16) {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.rows, term.cols
}

// registeredSizeOf reports what the session recorded for one client.
func registeredSizeOf(t *testing.T, s *Session, tok *attachToken) clientSize {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.clientSizes[tok]
	if cs == nil {
		t.Fatal("no size entry for this token")
	}
	return clientSize{rows: cs.rows, cols: cs.cols}
}
