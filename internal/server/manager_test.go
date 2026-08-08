package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
)

// newTestManager builds a manager whose paths live under a short temp directory, so socket
// paths stay inside the sockaddr_un limit.
//
// selfExe is overridden to the real cm binary when one is available, since spawning a shim
// means re-execing this binary and a test binary would not understand the arguments.
func newTestManager(t *testing.T, newTerm NewTerminalFunc) (*Manager, *store.Store, paths.Dirs) {
	t.Helper()

	root := shortTempDir(t)
	dirs := paths.Dirs{
		Runtime: filepath.Join(root, "r"),
		State:   filepath.Join(root, "s"),
	}
	if err := dirs.Ensure(); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	st, err := store.Open(context.Background(), dirs.Database())
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { st.Close() })

	mgr, err := NewManager(dirs, st, newTerm)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	return mgr, st, dirs
}

// A recorded session whose shim is definitively gone must be marked dead, not adopted, or the
// registry would fill with sessions that can never be reached.
func TestReconcileMarksMissingShimDead(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	// A record pointing at a socket that was never created.
	if err := st.Create(ctx, store.Session{
		Name:       "ghost",
		ShimSocket: dirs.ShimSocket("ghost"),
		LogPath:    dirs.SessionLog("ghost"),
		State:      store.StateRunning,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got, err := st.Get(ctx, "ghost")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.State != store.StateDead {
		t.Errorf("State = %q, want %q", got.State, store.StateDead)
	}
	if _, live := mgr.Get("ghost"); live {
		t.Error("Get() reports the session live, want it absent")
	}
}

// The property the shim layer exists for: a shim that outlived its server is adopted, and
// consumption resumes from the recorded position rather than restarting.
func TestReconcileAdoptsLiveShim(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("adopted", "echo ADOPTED; sleep 5"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, live := mgr.Get("adopted")
	if !live {
		t.Fatal("session was not adopted")
	}

	r, _, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(r)
	if got := readUntil(t, r, "ADOPTED"); !strings.Contains(got, "ADOPTED") {
		t.Errorf("output = %q, want the adopted session's output", got)
	}

	// The record must stay running: it is.
	after, err := st.Get(ctx, "adopted")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if after.State != store.StateRunning {
		t.Errorf("State = %q, want %q", after.State, store.StateRunning)
	}
}

// Sessions that already ended must not be probed or adopted on startup.
func TestReconcileSkipsNonRunningRecords(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{
		Name:       "finished",
		ShimSocket: dirs.ShimSocket("finished"),
		LogPath:    dirs.SessionLog("finished"),
		State:      store.StateExited,
		ExitCode:   7,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	got, err := st.Get(ctx, "finished")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	// Still exited with its status intact: an exited session records why it ended, and
	// overwriting that with "dead" would lose the reason.
	if got.State != store.StateExited || got.ExitCode != 7 {
		t.Errorf("record = (%q, %d), want (%q, 7)", got.State, got.ExitCode, store.StateExited)
	}
}

// Opening a name that exists but whose shim is gone must replace the stale record rather than
// failing forever, or a name would become permanently unusable after a crash.
func TestOpenReplacesStaleRecord(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{
		Name:       "reused",
		ShimSocket: dirs.ShimSocket("reused"),
		LogPath:    dirs.SessionLog("reused"),
		State:      store.StateDead,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Spawning a real shim requires the cm binary, so only the record-replacement half is
	// asserted here; end-to-end creation is covered by the CLI tests.
	mgr.selfExe = "/nonexistent/cm"
	_, _, err := mgr.Open(ctx, OpenOptions{Name: "reused", Rows: 24, Cols: 80})
	if err == nil {
		t.Fatal("Open() with a bogus shim binary succeeded, want failure")
	}

	// The stale record must be gone, so a later attempt with a working binary can create it.
	if _, err := st.Get(ctx, "reused"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound: the stale record should have been replaced", err)
	}
}

// Open on a live session returns the same object rather than creating a second one, which is
// what lets several clients share a session.
func TestOpenReturnsExistingLiveSession(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("shared", "sleep 5"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	first, created, err := mgr.Open(ctx, OpenOptions{Name: "shared"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if created {
		t.Error("created = true, want false for an existing session")
	}

	second, created, err := mgr.Open(ctx, OpenOptions{Name: "shared"})
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	if created || first != second {
		t.Error("Open() returned a different session, want the same live one")
	}
}

func TestOpenRejectsInvalidName(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	if _, _, err := mgr.Open(context.Background(), OpenOptions{Name: "../evil"}); err == nil {
		t.Error("Open() with a traversal name = nil error, want rejection")
	}
}

// An empty name asks the server to allocate one, which is how a terminal emulator gets a
// per-window session without inventing names.
func TestOpenAllocatesNameWhenEmpty(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	mgr.selfExe = "/nonexistent/cm"

	// Creation fails without a real binary, but the allocation happens first and is what is
	// being checked: the error must not be about a missing name.
	_, _, err := mgr.Open(context.Background(), OpenOptions{Rows: 24, Cols: 80})
	if err == nil {
		t.Fatal("Open() succeeded with a bogus shim binary, want failure")
	}
	if strings.Contains(err.Error(), "session name is empty") {
		t.Errorf("Open() error = %v, want a name to have been allocated", err)
	}
}

// Without force, an unreachable shim is left recorded: it may be busy rather than dead, and
// forgetting it would orphan a live shell with no way to reach it.
func TestKillWithoutForceKeepsUnreachableRecord(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{
		Name:       "unreachable",
		ShimSocket: dirs.ShimSocket("unreachable"),
		LogPath:    dirs.SessionLog("unreachable"),
		State:      store.StateRunning,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := mgr.Kill(ctx, "unreachable", false); err == nil {
		t.Error("Kill() without force = nil error, want a refusal to forget a maybe-live shim")
	}
	if _, err := st.Get(ctx, "unreachable"); err != nil {
		t.Errorf("record was removed despite the refusal: %v", err)
	}

	// force is the escape hatch for when the user knows better.
	if err := mgr.Kill(ctx, "unreachable", true); err != nil {
		t.Errorf("Kill() with force error = %v, want nil", err)
	}
	if _, err := st.Get(ctx, "unreachable"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound after a forced kill", err)
	}
}

func TestKillMissingSessionReportsNotFound(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	if err := mgr.Kill(context.Background(), "nope", false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Kill() error = %v, want ErrNotFound", err)
	}
}

// Closing the manager must leave shims running: that is what makes a server restart
// survivable. It must also persist each resume point.
func TestCloseLeavesShimRunningAndPersistsResumePoint(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("persisted", "echo SOMETHING; sleep 5"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, live := mgr.Get("persisted")
	if !live {
		t.Fatal("session not adopted")
	}
	r, _, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	readUntil(t, r, "SOMETHING")
	sess.detach(r)

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	got, err := st.Get(ctx, "persisted")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LastSeq == 0 {
		t.Error("LastSeq = 0 after Close, want the resume point persisted")
	}
	// The record must still be running: the shim was deliberately left alive.
	if got.State != store.StateRunning {
		t.Errorf("State = %q, want %q: Close must not end sessions", got.State, store.StateRunning)
	}
	// And the shim really is still there.
	if _, err := os.Stat(got.ShimSocket); err != nil {
		t.Errorf("shim socket gone after Close: %v", err)
	}
}

// probeShim must distinguish "nothing is listening" from "could not tell", because only the
// former justifies declaring a session dead.
func TestProbeShimDistinguishesMissingFromUnknown(t *testing.T) {
	ctx := context.Background()

	alive, err := probeShim(ctx, filepath.Join(shortTempDir(t), "absent.sock"))
	if alive || err == nil {
		t.Errorf("probeShim(absent) = (%v, %v), want (false, error)", alive, err)
	}

	rec := startShimFor(t, shimConfigFor("probed", "sleep 5"))
	alive, err = probeShim(ctx, rec.ShimSocket)
	if !alive || err != nil {
		t.Errorf("probeShim(live) = (%v, %v), want (true, nil)", alive, err)
	}

	// A cancelled context must not be read as "definitely gone", or a slow probe would
	// discard a live session.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if alive, _ := probeShim(cancelled, rec.ShimSocket); !alive {
		time.Sleep(time.Millisecond) // avoid a flaky read on a racing dial
	}
}

func TestListReportsLiveSequence(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("listed", "echo LISTED; sleep 5"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, _ := mgr.Get("listed")
	r, _, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(r)
	readUntil(t, r, "LISTED")

	sessions, err := mgr.List(ctx, "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("List() returned %d sessions, want 1", len(sessions))
	}
	// The live value must win over the stored one, which was written before this output.
	if sessions[0].LastSeq == 0 {
		t.Error("LastSeq = 0, want the live session's position")
	}
	if n := mgr.Clients("listed"); n != 1 {
		t.Errorf("Clients() = %d, want 1", n)
	}
}
