package server

import (
	"context"
	"testing"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func TestDecideClose(t *testing.T) {
	idle := closeFacts{promptSeen: true}
	for _, tc := range []struct {
		name  string
		facts closeFacts
		want  closeAction
	}{
		{name: "idle at a prompt", facts: idle, want: closeEnd},
		{name: "another window shows it", facts: closeFacts{promptSeen: true, others: 1}, want: closeKeep},
		// Another window wins even over a running command, since that window is still using the session.
		{name: "another window shows it running", facts: closeFacts{promptSeen: true, others: 1, running: true}, want: closeKeep},
		{name: "a command running", facts: closeFacts{promptSeen: true, running: true}, want: closeDetach},
		// A local ssh at a remote prompt: the remote's markers say idle, the local frame says otherwise.
		{name: "inside a command frame", facts: closeFacts{promptSeen: true, framed: true}, want: closeDetach},
		{name: "a program reports work", facts: closeFacts{promptSeen: true, reportedBusy: true}, want: closeDetach},
		// Nothing says a shell without integration is idle, so it is treated as busy.
		{name: "no shell integration", facts: closeFacts{}, want: closeDetach},
	} {
		if got := decideClose(tc.facts); got != tc.want {
			t.Errorf("%s: decideClose(%+v) = %v, want %v", tc.name, tc.facts, got, tc.want)
		}
	}
}

// closeTestService is a service over one adopted session with clients from pids 1000, 1001 and so on, its
// shell a sleep that never reports a prompt.
func closeTestService(t *testing.T, name string, clients int) (*Service, *Session) {
	t.Helper()
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor(name, "sleep 30"))
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, live := mgr.Get(name)
	if !live {
		t.Fatal("session not adopted")
	}
	for i := range clients {
		tok := sess.reserveClient()
		sess.noteClientIdentity(tok, "v1", int32(1000+i), capability.Set{})
		if _, err := sess.attach(nil, tok); err != nil {
			t.Fatalf("attach() error = %v", err)
		}
	}
	return NewService(mgr), sess
}

func TestEvictClientPIDAsksOnlyThatClient(t *testing.T) {
	_, sess := closeTestService(t, "evictpid", 3)

	asked, remaining := sess.EvictClientPID(1001)
	if asked != 1 || remaining != 2 {
		t.Errorf("EvictClientPID(1001) = %d asked, %d remaining; want 1, 2", asked, remaining)
	}
	// The one already asked is not counted again, nor as remaining.
	asked, remaining = sess.EvictClientPID(1001)
	if asked != 0 || remaining != 2 {
		t.Errorf("EvictClientPID(1001) again = %d asked, %d remaining; want 0, 2", asked, remaining)
	}
	// A pid holding no client, which is a window whose client left before the close arrived.
	asked, remaining = sess.EvictClientPID(4242)
	if asked != 0 || remaining != 2 {
		t.Errorf("EvictClientPID(4242) = %d asked, %d remaining; want 0, 2", asked, remaining)
	}
}

// A window closed while another shows the same session leaves it alone. This is the case the window-close
// watcher could not tell from a last window: it killed the session out from under the other one.
func TestCloseKeepsASessionAnotherWindowShows(t *testing.T) {
	svc, _ := closeTestService(t, "shared", 2)

	resp, err := svc.Close(context.Background(), &serverv1.CloseRequest{Session: "shared", ClientPid: 1000})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got, want := resp.Outcome, serverv1.CloseOutcome_CLOSE_OUTCOME_KEPT; got != want {
		t.Errorf("Close() = %v, want %v", got, want)
	}
	if _, live := svc.mgr.Get("shared"); !live {
		t.Error("the session ended, want it running for the other window")
	}
}

// A shell that has never reported a prompt may be running anything, so a close detaches it.
func TestCloseDetachesAShellWithNoIntegration(t *testing.T) {
	svc, sess := closeTestService(t, "plain", 1)

	resp, err := svc.Close(context.Background(), &serverv1.CloseRequest{Session: "plain", ClientPid: 1000})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got, want := resp.Outcome, serverv1.CloseOutcome_CLOSE_OUTCOME_DETACHED; got != want {
		t.Errorf("Close() = %v, want %v", got, want)
	}
	if _, remaining := sess.EvictClientPID(0); remaining != 0 {
		t.Errorf("%d clients remain, want the closed window's detached", remaining)
	}
}

func TestCloseOfASessionNotRunning(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	recordSession(t, st, store.Session{ID: "ended", State: store.StateExited})

	resp, err := NewService(mgr).Close(context.Background(), &serverv1.CloseRequest{Session: "ended"})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got, want := resp.Outcome, serverv1.CloseOutcome_CLOSE_OUTCOME_NOT_RUNNING; got != want {
		t.Errorf("Close() = %v, want %v", got, want)
	}
}

// A window that borrowed a session by name releases the name and leaves the session, as cm kill by that
// name does.
func TestCloseByABorrowedNameUnbinds(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	recordSession(t, st, store.Session{ID: "owner", State: store.StateRunning})
	if err := st.Bind(ctx, store.Binding{Name: "borrowed", SessionID: "owner", OnKill: store.KillUnbind}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	resp, err := NewService(mgr).Close(ctx, &serverv1.CloseRequest{Session: "borrowed"})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got, want := resp.Outcome, serverv1.CloseOutcome_CLOSE_OUTCOME_UNBOUND; got != want {
		t.Errorf("Close() = %v, want %v", got, want)
	}
	rec, err := st.Get(ctx, "owner")
	if err != nil {
		t.Fatalf("Get() error = %v, want the session kept", err)
	}
	if rec.State != store.StateRunning {
		t.Errorf("State = %v, want running", rec.State)
	}
}

func TestCloseRequiresASession(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	if _, err := NewService(mgr).Close(context.Background(), &serverv1.CloseRequest{}); err == nil {
		t.Error("Close() with no session = nil error, want one")
	}
}
