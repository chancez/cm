package server

import (
	"context"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// endedButRegistered puts the server in the state a just-finished command produces, and returns a service
// reading from it.
//
// The window is real but narrow, so it is constructed rather than raced for. A session discards its terminal
// model the moment its shell exits, and a separate goroutine removes it from the registry afterwards. In
// between it is still registered while having nothing left to render, which is the state below: ended, still in
// the map, no model, with the output on disk where the shim left it.
//
// Waiting for the real ordering would make these tests the flaky thing they exist to prevent.
func endedButRegistered(t *testing.T, name, output string) *Service {
	t.Helper()

	newTerm, _ := replayTerminal(t)
	mgr, st, dirs := newTestManager(t, newTerm)
	mgr.SetPersistPolicy(testPolicy())
	ctx := context.Background()

	logPath := dirs.SessionLog(name)
	// The command's output, on disk as the shim writes it: appended unbuffered as it arrives, so it is
	// already there when the shell exits.
	writeSavedLog(t, logPath, output)

	rec := startShimFor(t, shimConfigFor(name, "sleep 5"))
	rec.ID = name
	rec.LogPath = logPath
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, ok := mgr.Get(name)
	if !ok {
		t.Fatal("session was not adopted")
	}

	// Exactly what finish() does before watch() gets to deregister: mark the outcome and drop the model.
	sess.mu.Lock()
	sess.ended = true
	sess.exitCode = 0
	sess.term = nil
	sess.mu.Unlock()

	return NewService(mgr)
}

// Reading a session that has ended but not yet left the registry must fall back to the log on disk.
//
// The bug this pins down: `cm run` printed nothing at all in roughly one run in twenty-five. It waits for the
// command, sees it finished, and reads the output, which lands in the window above -- the session is still
// registered, so the live path was taken, and its model was already gone, so it rendered empty. Nothing was
// lost, which is what made it look like flakiness rather than a bug: reading again a moment later worked,
// because by then the session had been deregistered and the disk path was taken.
func TestReadFallsBackToDiskWhenTheSessionHasEnded(t *testing.T) {
	svc := endedButRegistered(t, "ended-read", "command output\r\n")

	resp, err := svc.Read(context.Background(), &serverv1.ReadRequest{
		Session: "ended-read",
		Unwrap:  true,
	})
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("Read() returned no data for a session whose output is on disk")
	}
	if got := string(resp.Data); !strings.Contains(got, "command output") {
		t.Errorf("Read() = %q, want it to contain the command's output", got)
	}
}

// History has the same window and the same fix, since both render from a model the session no longer has.
func TestHistoryFallsBackToDiskWhenTheSessionHasEnded(t *testing.T) {
	svc := endedButRegistered(t, "ended-history", "historic output\r\n")

	resp, err := svc.History(context.Background(), &serverv1.HistoryRequest{
		Session: "ended-history",
	})
	if err != nil {
		t.Fatalf("History() error = %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("History() returned no data for a session whose output is on disk")
	}
	if got := string(resp.Data); !strings.Contains(got, "historic output") {
		t.Errorf("History() = %q, want it to contain the session's output", got)
	}
}
