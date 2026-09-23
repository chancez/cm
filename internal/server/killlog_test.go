package server

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/chancez/cm/internal/store"
)

// Killing a session removes the output it had persisted, not only its record.
//
// The leak this closes: Kill deleted the row and left the log file behind, and ExpireDeadSessions is the
// only other thing that removes a log, which walks records. Once the row was gone the file could never be
// collected. Measured on a real install before the fix, 149 logs in the state dir with 118 belonging to no
// session, 85 MB nothing would free. The symptom is a state directory that only grows, which points
// nowhere near `cm kill`.
//
// The live path is the one that matters, since every session a window closes goes through it.
func TestKillRemovesThePersistedLogOfALiveSession(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("logged", "sleep 5"))
	rec.LogPath = dirs.SessionLog("logged")
	rec.PersistRequested = true
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := os.WriteFile(rec.LogPath, []byte("output that was saved\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, live := mgr.Get("logged"); !live {
		t.Fatal("session was not adopted, so the live path is not under test")
	}

	if _, err := mgr.Kill(ctx, "logged", KillOptions{}); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}

	if _, err := os.Stat(rec.LogPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(%s) error = %v, want the log removed with the record", rec.LogPath, err)
	}
	if _, err := st.Get(ctx, "logged"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

// The same for a record whose shim cannot be reached, which is the other way a row and its log are
// dropped: --force forgets the session, so it has to forget all of it.
func TestForcedKillOfAnUnreachableShimRemovesItsLog(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	logPath := dirs.SessionLog("stranded")
	if err := st.Create(ctx, store.Session{
		ID:               "stranded",
		ShimSocket:       dirs.ShimSocket("stranded"),
		LogPath:          logPath,
		PersistRequested: true,
		State:            store.StateRunning,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := os.WriteFile(logPath, []byte("output that was saved\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := mgr.Kill(ctx, "stranded", KillOptions{Force: true}); err != nil {
		t.Fatalf("Kill() with force error = %v", err)
	}

	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(%s) error = %v, want the log removed with the record", logPath, err)
	}
	if _, err := st.Get(ctx, "stranded"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}
