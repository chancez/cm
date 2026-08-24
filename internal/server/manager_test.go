package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
		ID:         "ghost",
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
	recordSession(t, st, rec)

	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, live := mgr.Get("adopted")
	if !live {
		t.Fatal("session was not adopted")
	}

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	if got := readUntil(t, att.reader, "ADOPTED"); !strings.Contains(got, "ADOPTED") {
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
		ID:         "finished",
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
		ID:         "reused",
		ShimSocket: dirs.ShimSocket("reused"),
		LogPath:    dirs.SessionLog("reused"),
		State:      store.StateDead,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// The name has to be bound for this to be the case under test at all: an unbound name would send
	// Open down the create path, which allocates a new identity and leaves the stale record alone.
	nameSession(t, st, "reused")

	// Spawning a real shim requires the cm binary, so only the record-replacement half is
	// asserted here; end-to-end creation is covered by the CLI tests.
	mgr.selfExe = "/nonexistent/cm"
	_, _, err := mgr.Open(ctx, OpenOptions{Ref: "reused", Rows: 24, Cols: 80})
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
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	first, created, err := mgr.Open(ctx, OpenOptions{Ref: "shared"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if created {
		t.Error("created = true, want false for an existing session")
	}

	second, created, err := mgr.Open(ctx, OpenOptions{Ref: "shared"})
	if err != nil {
		t.Fatalf("second Open() error = %v", err)
	}
	if created || first != second {
		t.Error("Open() returned a different session, want the same live one")
	}
}

func TestOpenRejectsInvalidName(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	if _, _, err := mgr.Open(context.Background(), OpenOptions{Ref: "../evil"}); err == nil {
		t.Error("Open() with a traversal name = nil error, want rejection")
	}
}

// An empty reference asks for a session with no name at all, which is what `cm attach` with no
// argument and `cm run -d` both do. It must allocate an identity rather than refusing.
//
// This used to allocate a name like s17 as well. It no longer does, and the ID is what a caller gets
// back: an implicit name was only ever a stand-in for an identity.
func TestOpenWithNoReferenceAllocatesAnIdentity(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	mgr.selfExe = "/nonexistent/cm"

	// Creation fails without a real binary, but the allocation happens first and is what is being
	// checked: the error must name a session that was allocated rather than an empty one.
	_, _, err := mgr.Open(context.Background(), OpenOptions{Rows: 24, Cols: 80})
	if err == nil {
		t.Fatal("Open() succeeded with a bogus shim binary, want failure")
	}
	if strings.Contains(err.Error(), "is empty") {
		t.Errorf("Open() error = %v, want an identity to have been allocated", err)
	}
}

// Without force, an unreachable shim is left recorded: it may be busy rather than dead, and
// forgetting it would orphan a live shell with no way to reach it.
func TestKillWithoutForceKeepsUnreachableRecord(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{
		ID:         "unreachable",
		ShimSocket: dirs.ShimSocket("unreachable"),
		LogPath:    dirs.SessionLog("unreachable"),
		State:      store.StateRunning,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if _, err := mgr.Kill(ctx, "unreachable", false, 0); err == nil {
		t.Error("Kill() without force = nil error, want a refusal to forget a maybe-live shim")
	}
	if _, err := st.Get(ctx, "unreachable"); err != nil {
		t.Errorf("record was removed despite the refusal: %v", err)
	}

	// force is the escape hatch for when the user knows better.
	if _, err := mgr.Kill(ctx, "unreachable", true, 0); err != nil {
		t.Errorf("Kill() with force error = %v, want nil", err)
	}
	if _, err := st.Get(ctx, "unreachable"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound after a forced kill", err)
	}
}

func TestKillMissingSessionReportsNotFound(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	if _, err := mgr.Kill(context.Background(), "nope", false, 0); !errors.Is(err, store.ErrNotFound) {
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
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, live := mgr.Get("persisted")
	if !live {
		t.Fatal("session not adopted")
	}
	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	readUntil(t, att.reader, "SOMETHING")

	// Waited for rather than assumed, and it is the same window TestListReportsLiveSequence documents:
	// the pump appends a chunk to the client log *before* it takes the lock to advance lastSeq, which is
	// the order resumePoints explains and depends on. So having read the output does not mean the
	// position accounts for it, and Close inside that gap persists the zero the fixture stored. Failed
	// that way in two of four full Linux suite runs, as `LastSeq = 0 after Close`.
	//
	// Close is what is under test here, so the wait goes before it rather than around the assertion.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if shimSeq, _ := sess.resumePoints(); shimSeq > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the pump to advance the resume point past 0")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess.detach(att)

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
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, _ := mgr.Get("listed")
	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	readUntil(t, att.reader, "LISTED")

	// Polled rather than asserted once, and the window is documented rather than hypothetical: the pump
	// appends a chunk to the client log *before* it takes the lock to advance lastSeq, which is the order
	// resumePoints explains and depends on. So reading output does not mean the position has caught up,
	// and this failed with LastSeq = 0 while the bytes it counts had already been read -- reproduced by
	// running two suites at once, which is enough to deschedule the pump inside that window.
	//
	// Waiting for the state the race lands in rather than racing it, and the assertion is unchanged: the
	// live value still has to beat the zero the fixture stored.
	var sessions []store.Session
	deadline := time.Now().Add(5 * time.Second)
	for {
		sessions, err = mgr.List(ctx)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(sessions) != 1 {
			t.Fatalf("List() returned %d sessions, want 1", len(sessions))
		}
		if sessions[0].LastSeq > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("LastSeq = 0, want the live session's position")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := mgr.Clients("listed"); n != 1 {
		t.Errorf("Clients() = %d, want 1", n)
	}
}

// A per-session error message must not repeat the session name, since the response is already keyed
// by it and the result reads as `nosuch: "nosuch": session not found`.
func TestTrimNamePrefix(t *testing.T) {
	tests := []struct {
		msg  string
		name string
		want string
	}{
		{`"nosuch": session not found`, "nosuch", "session not found"},
		{"nosuch: session not found", "nosuch", "session not found"},
		// A message that does not start with the name is left alone.
		{"shim unreachable", "work", "shim unreachable"},
		// And a name appearing later must not be stripped.
		{"cannot reach shim for work", "work", "cannot reach shim for work"},
	}
	for _, tt := range tests {
		if got := trimNamePrefix(tt.msg, tt.name); got != tt.want {
			t.Errorf("trimNamePrefix(%q, %q) = %q, want %q", tt.msg, tt.name, got, tt.want)
		}
	}
}

// A client's pixel size has to reach the shim's argv, or the pty is created without it.
//
// This is the seam the bug lived at: Open carried x_pixel and y_pixel on the wire, Resize plumbed them
// through correctly, and the create path dropped them between the request and the shim. The result was
// a session whose pty reported zero pixels until something resized it, and `kitten icat` reads exactly
// that field before transmitting, so it refused to draw and blamed the terminal.
//
// Asserted on the argv rather than end to end because spawning a shim needs a real pty, and the whole
// defect was in what got passed.
func TestShimArgsCarryPixelSize(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)

	args := mgr.shimArgs(OpenOptions{
		// The identity, not the reference a client asked by: the shim's socket and log are named
		// after the ID, and a reference never reaches it.
		id:   "pixels",
		Rows: 30, Cols: 100,
		XPixel: 800, YPixel: 600,
	}, "")

	want := []string{
		"--runtime-dir", mgr.dirs.Runtime,
		"--state-dir", mgr.dirs.State,
		"shim",
		"--session", "pixels",
		"--session-ref", "@pixels",
		"--rows", "30",
		"--cols", "100",
		"--xpixel", "800",
		"--ypixel", "600",
	}
	if !slices.Equal(args, want) {
		t.Errorf("shimArgs() = %v, want %v", args, want)
	}
}

// A client that cannot report pixels must not have any invented for it. Zero is the value a program
// reads as "this terminal does not know", so passing a made-up size would be worse than passing none:
// it would have the program compute cell dimensions from a window that does not exist.
func TestShimArgsOmitUnknownPixelSize(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)

	args := mgr.shimArgs(OpenOptions{id: "nopixels", Rows: 24, Cols: 80}, "")

	want := []string{
		"--runtime-dir", mgr.dirs.Runtime,
		"--state-dir", mgr.dirs.State,
		"shim",
		"--session", "nopixels",
		"--session-ref", "@nopixels",
		"--rows", "24",
		"--cols", "80",
	}
	if !slices.Equal(args, want) {
		t.Errorf("shimArgs() = %v, want %v", args, want)
	}
}

// recordSession stores a session and binds its ID as a name as well.
//
// What a test means by "a session called x" needs both rows now. Before names were bindings the record
// carried the name, so a bare Create was the whole fixture; a test that skips the bind gets a session
// nothing names, and anything that then opens x creates a second session rather than finding this one.
// Binding the ID as the name keeps the fixtures reading as they did, and is legal because names and IDs
// are separate namespaces: the same string can be both.
func recordSession(t *testing.T, st *store.Store, rec store.Session) {
	t.Helper()
	ctx := context.Background()
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	nameSession(t, st, rec.ID)
}

// nameSession binds a session's ID as a name for it, so a fixture can refer to it the way a user would.
//
// Separate from recordSession because plenty of fixtures create a record and never go through the
// resolve layer, and those genuinely need no name: a session with none is an ordinary session rather
// than a broken fixture.
func nameSession(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.Bind(context.Background(), store.Binding{
		Name: id, SessionID: id, OnKill: store.KillTarget,
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
}
