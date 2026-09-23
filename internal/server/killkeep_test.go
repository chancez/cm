package server

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chancez/cm/internal/store"
)

// `cm kill --keep` stops the shell and leaves everything there is to look at afterwards.
//
// All four parts are asserted because each is dropped by a different line of the ordinary kill path, and
// three of them are invisible until someone goes looking: the row, the names that reach it, the saved
// output, and the recorded outcome.
func TestKillWithKeepLeavesTheSessionToLookAt(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "inspectme", "sleep 30")
	rec.LogPath = dirs.SessionLog("inspectme")
	rec.PersistRequested = true
	recordSession(t, st, rec)
	if err := os.WriteFile(rec.LogPath, []byte("output worth keeping\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, live := mgr.Get("inspectme"); !live {
		t.Fatal("session was not adopted, so the live path is not under test")
	}

	if _, err := mgr.Kill(ctx, "inspectme", KillOptions{Keep: true}); err != nil {
		t.Fatalf("Kill() with keep error = %v", err)
	}

	got := waitForEndedRecord(t, st, "inspectme")
	if got.State != store.StateExited {
		t.Errorf("State = %q, want %q: the shell was stopped, so the outcome is recorded",
			got.State, store.StateExited)
	}
	if _, err := os.Stat(rec.LogPath); err != nil {
		t.Errorf("Stat(%s) error = %v, want the saved output kept", rec.LogPath, err)
	}
	// Reachable by name, not only by ID. Releasing the names would leave `cm history inspectme` unable to
	// find what was kept, which is the only reason to keep it.
	if _, err := mgr.Resolve(ctx, "inspectme"); err != nil {
		t.Errorf("Resolve() error = %v, want the names kept", err)
	}
}

// The same for a session with nothing saved, which is the case the forget-on-exit rule is about.
//
// Separate from the test above rather than folded into it, and this is the trap it avoids: that one records
// a log path, and watch() only forgets an interactive session that saved nothing. So it passes with the
// keep flag ignored in watch() entirely, and proves nothing about that rule. Here every condition the rule
// needs is present: no log, nothing asked to persist, and something watched the session.
func TestKillWithKeepSurvivesTheForgetExitedRule(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "nolog", "sleep 30")
	rec.LogPath = ""
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, live := mgr.Get("nolog")
	if !live {
		t.Fatal("session was not adopted, so the live path is not under test")
	}
	// Set directly rather than by attaching: only the Attach RPC marks a session watched, and it is a
	// client asking for a screen repaint that does it, which a manager-level attachment does not do.
	sess.noteWatched()

	if _, err := mgr.Kill(ctx, "nolog", KillOptions{Keep: true}); err != nil {
		t.Fatalf("Kill() with keep error = %v", err)
	}

	if got := waitForEndedRecord(t, st, "nolog"); got.State != store.StateExited {
		t.Errorf("State = %q, want %q", got.State, store.StateExited)
	}
}

// waitForEndedRecord polls for the state watch() writes once the shell is gone.
//
// Polled rather than assumed: Kill returns when the shim's connection closes, and the record is written by
// the goroutine watching the session, so the two are ordered only by the shell's exit.
func waitForEndedRecord(t *testing.T, st *store.Store, id string) store.Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := st.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get() error = %v, want the record kept", err)
		}
		if rec.State != store.StateRunning {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("record still %q after 10s, want an ended state", rec.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
