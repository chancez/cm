package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/shim"
	"github.com/chancez/cm/internal/store"
)

// startShimInRuntimeDir starts a shim on the socket path a real one would use.
//
// startShimFor puts its socket in a private temp dir, which is fine for tests that dial it directly and
// wrong here: the diagnosis works by scanning the runtime directory, so a shim outside it is invisible and
// every assertion would pass or fail for the wrong reason. A real shim's socket comes from
// paths.Dirs.ShimSocket, and that is what this reproduces.
func startShimInRuntimeDir(t *testing.T, dirs paths.Dirs, name, script string) store.Session {
	t.Helper()

	socket := dirs.ShimSocket(name)
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	l, err := shim.Listen(socket)
	if err != nil {
		t.Fatalf("shim.Listen() error = %v", err)
	}
	sess, err := shim.Start(shimConfigFor(name, script))
	if err != nil {
		l.Close()
		t.Fatalf("shim.Start() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = shim.Serve(ctx, l, shim.NewService(sess))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	waitSocket(t, socket)

	return store.Session{
		Name: name, ShimSocket: socket, State: store.StateRunning,
		Rows: 24, Cols: 80,
	}
}

// findingsByKind indexes a diagnosis for assertions.
func findingsByKind(fs []Finding) map[string][]Finding {
	out := make(map[string][]Finding)
	for _, f := range fs {
		out[f.Kind] = append(out[f.Kind], f)
	}
	return out
}

// A healthy installation reports nothing.
//
// Worth asserting first: a diagnostic that cries wolf on a working setup is worse than none, since it trains
// the reader to ignore it.
func TestDiagnoseFindsNothingWhenHealthy(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "healthy", "sleep 5")
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got, err := mgr.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Diagnose() = %+v, want nothing on a healthy installation", got)
	}
}

// A live shim with no session record is an orphan: it holds a pty and a shell nothing can reach.
//
// This is the failure that motivated the command. It is silent and cumulative, since an orphan is by
// definition absent from `cm list`, and the symptom when enough accumulate is an unrelated program failing
// to allocate a terminal.
func TestDiagnoseFindsAnOrphanedShim(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "orphaned", "sleep 5")
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// The record goes away while the shim keeps running, which is what a crash between spawning and
	// recording, or a deleted state directory, leaves behind.
	if err := st.Delete(ctx, "orphaned"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	got, err := mgr.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	byKind := findingsByKind(got)
	orphans := byKind[FindingOrphanShim]
	if len(orphans) != 1 {
		t.Fatalf("Diagnose() = %+v, want exactly one orphan-shim", got)
	}
	if orphans[0].Session != "orphaned" {
		t.Errorf("session = %q, want %q", orphans[0].Session, "orphaned")
	}
	// The pids are the actionable part: they are what lets a reader confirm what is being reported.
	if orphans[0].ShimPID == 0 || orphans[0].ShellPID == 0 {
		t.Errorf("finding = %+v, want both pids reported", orphans[0])
	}
	if !orphans[0].Fixable {
		t.Error("an orphaned shim was reported as not fixable, want it repairable")
	}
}

// Repairing an orphan stops it, and leaves a healthy session alone.
//
// The second half matters more than the first. A repair that also touched live sessions would be a worse bug
// than the leak it fixes, so the test keeps one of each and checks both outcomes.
func TestRepairStopsOrphansAndSparesHealthySessions(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	healthy := startShimInRuntimeDir(t, dirs, "keepme", "sleep 5")
	if err := st.Create(ctx, healthy); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	orphan := startShimInRuntimeDir(t, dirs, "dropme", "sleep 5")
	if err := st.Create(ctx, orphan); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := st.Delete(ctx, "dropme"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	found, err := mgr.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	done := mgr.Repair(ctx, found)
	if len(done) != 1 {
		t.Fatalf("Repair() did %d things, want exactly one: %v", len(done), done)
	}

	// The orphan is gone: its socket no longer answers.
	if _, alive := probeShimState(ctx, orphan.ShimSocket); alive {
		t.Error("the orphaned shim is still answering after a repair")
	}
	// And the healthy one is untouched, which is the property that makes this safe to run on a live machine.
	if _, alive := probeShimState(ctx, healthy.ShimSocket); !alive {
		t.Error("a healthy session's shim was stopped by a repair, want it left alone")
	}
}

// A socket file with nothing behind it is reported, and removing it is a fix.
//
// Left behind by a shim that died without unlinking, and it matters because the socket is what a server
// checks before binding: a stale one makes a session look present when it is not.
func TestDiagnoseFindsAStaleSocket(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	ctx := context.Background()

	// A socket-shaped file nothing listens on, which is exactly what a dead shim leaves.
	stale := dirs.ShimSocket("ghost")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := mgr.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	byKind := findingsByKind(got)
	if len(byKind[FindingStaleSocket]) != 1 {
		t.Fatalf("Diagnose() = %+v, want one stale-socket", got)
	}
	if got := byKind[FindingStaleSocket][0].Session; got != "ghost" {
		t.Errorf("session = %q, want %q: the name comes from the socket path", got, "ghost")
	}

	mgr.Repair(ctx, got)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("the stale socket survived a repair")
	}
}

// A record promising a shim that is not there is reported, and deliberately not repaired.
//
// Reconcile already marks these dead at startup and expiry removes them on its own schedule, so deleting the
// record here would duplicate that policy in a second place and could discard a session whose shim is merely
// slow to answer.
func TestDiagnoseReportsARecordWithNoShim(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{
		Name:       "vanished",
		ShimSocket: "/nonexistent/shim-vanished.sock",
		State:      store.StateRunning,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := mgr.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose() error = %v", err)
	}
	byKind := findingsByKind(got)
	missing := byKind[FindingMissingShim]
	if len(missing) != 1 {
		t.Fatalf("Diagnose() = %+v, want one missing-shim", got)
	}
	if missing[0].Fixable {
		t.Error("missing-shim was reported as fixable, want it left to Reconcile and expiry")
	}
	if done := mgr.Repair(ctx, got); len(done) != 0 {
		t.Errorf("Repair() acted on a missing-shim: %v", done)
	}
}

// A missing runtime directory is not a problem to report.
//
// A fresh installation has no directory and no sessions, and a diagnostic that fails there would be noise on
// the one path where nothing can be wrong yet.
func TestDiagnoseHandlesAMissingRuntimeDir(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	ctx := context.Background()

	if err := os.RemoveAll(dirs.Runtime); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}

	got, err := mgr.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose() error = %v, want a missing runtime dir to be fine", err)
	}
	if len(got) != 0 {
		t.Errorf("Diagnose() = %+v, want nothing", got)
	}
}
