package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/store"
)

// A directory other users can read is reported, and a correctly restricted one is not.
//
// The reason this needs a check at all is that the obvious defense does not work. cm creates its directories
// 0700, but os.MkdirAll applies a mode only when it creates the directory: pointed at one that already
// exists it succeeds and leaves the mode alone. So CM_RUNTIME_DIR aimed at a pre-existing shared path gives a
// working installation whose session logs, which contain everything typed at a prompt, are world-readable.
func TestCheckDirPerms(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)

	// Ensure() just created both at 0700, which is the shape of a normal installation.
	if got := mgr.checkDirPerms(); len(got) != 0 {
		t.Fatalf("checkDirPerms() = %+v, want nothing for 0700 directories", got)
	}

	if err := os.Chmod(dirs.Runtime, 0o755); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	got := mgr.checkDirPerms()
	if len(got) != 1 {
		t.Fatalf("checkDirPerms() = %+v, want one finding for a 0755 runtime directory", got)
	}
	if got[0].Kind != FindingLooseDirPerms {
		t.Errorf("kind = %q, want %q", got[0].Kind, FindingLooseDirPerms)
	}
	// The path is carried in Socket, which is what Repair chmods. A finding that named the directory only in
	// its prose would be unfixable.
	if got[0].Socket != dirs.Runtime {
		t.Errorf("Socket = %q, want the runtime directory %q", got[0].Socket, dirs.Runtime)
	}
	if !got[0].Fixable {
		t.Error("Fixable = false, want true: tightening a mode is safe to do automatically")
	}
	// The actual mode is in the message, since "too loose" without a number does not say what to look at.
	if !strings.Contains(got[0].Detail, "0755") {
		t.Errorf("detail does not name the mode: %q", got[0].Detail)
	}

	// Group-only access counts too. A shared group is the realistic way this happens, and it is no safer
	// than world-readable when the group has other members.
	if err := os.Chmod(dirs.Runtime, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	if err := os.Chmod(dirs.State, 0o740); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	got = mgr.checkDirPerms()
	if len(got) != 1 || got[0].Socket != dirs.State {
		t.Fatalf("checkDirPerms() = %+v, want one finding for the 0740 state directory", got)
	}
}

// Repair tightens a loose directory, and the check then passes.
//
// Asserted end to end rather than by calling Chmod in the test, because the two halves are what can drift:
// checkDirPerms puts the path in Socket and Repair reads it from there, so a finding whose path moved to
// another field would leave --clean silently fixing nothing.
func TestRepairTightensDirPerms(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	ctx := context.Background()

	if err := os.Chmod(dirs.Runtime, 0o777); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}

	findings := mgr.checkDirPerms()
	done := mgr.Repair(ctx, findings)
	if len(done) != 1 {
		t.Fatalf("Repair() = %+v, want one repair", done)
	}

	fi, err := os.Stat(dirs.Runtime)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("mode after Repair = %04o, want 0700", perm)
	}
	if got := mgr.checkDirPerms(); len(got) != 0 {
		t.Errorf("checkDirPerms() after Repair = %+v, want nothing", got)
	}
}

// A session record whose output log is gone is reported.
//
// Worth reporting because every symptom points elsewhere. `cm history` returns nothing, which looks like the
// emulator failing; reattaching restores a blank screen, which looks like a bug in restore; a revived session
// has no scrollback. None of them mentions a missing file.
func TestCheckMissingLogs(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	// A record whose log exists, which must not be reported.
	present := dirs.SessionLog("present")
	if err := os.MkdirAll(filepath.Dir(present), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(present, []byte("some output"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	mustCreateRecord(t, st, "present", present)

	// A record with no log path at all, which is a session that never had one rather than one that lost it.
	mustCreateRecord(t, st, "nopath", "")

	if got := mgr.checkMissingLogs(ctx); len(got) != 0 {
		t.Fatalf("checkMissingLogs() = %+v, want nothing when no log is missing", got)
	}

	mustCreateRecord(t, st, "gone", dirs.SessionLog("gone"))

	got := mgr.checkMissingLogs(ctx)
	if len(got) != 1 {
		t.Fatalf("checkMissingLogs() = %+v, want one finding", got)
	}
	if got[0].Kind != FindingMissingLog {
		t.Errorf("kind = %q, want %q", got[0].Kind, FindingMissingLog)
	}
	// The session is named, since "a log is missing" without saying whose is not actionable.
	if !strings.Contains(got[0].Detail, "gone") {
		t.Errorf("detail does not name the session: %q", got[0].Detail)
	}
	// And the sessions whose logs are fine are not mentioned, which is what proves the check discriminates
	// rather than reporting every record.
	if strings.Contains(got[0].Detail, "present") || strings.Contains(got[0].Detail, "nopath") {
		t.Errorf("detail names sessions whose logs are fine: %q", got[0].Detail)
	}
	if got[0].Fixable {
		t.Error("Fixable = true, want false: the bytes are gone and deleting the record would lose the rest")
	}
}

// A session the server tracks whose shim does not answer is reported.
//
// Distinct from missing-shim, which compares the store against sockets on disk. This compares the server's
// own in-memory registry against reality, which is the shape an adoption bug takes: `cm list` keeps showing
// the session as running while every command against it fails.
func TestCheckTrackedShims(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	// Two real shims at the paths a real server would use, adopted through Reconcile: the same code path
	// that runs at startup, so the registry is populated the way it is in production rather than by reaching
	// into the map.
	for _, name := range []string{"live", "ghost"} {
		rec := startShimInRuntimeDir(t, dirs, name, "sleep 60")
		rec.LogPath = filepath.Join(dirs.State, name+".log")
		if err := st.Create(ctx, rec); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	for _, name := range []string{"live", "ghost"} {
		if _, live := mgr.Get(name); !live {
			t.Fatalf("session %q was not adopted, so this test would assert nothing", name)
		}
	}

	if got := mgr.checkTrackedShims(ctx); len(got) != 0 {
		t.Fatalf("checkTrackedShims() = %+v, want nothing while both shims answer", got)
	}

	// Take one shim's socket away, so a dial fails the way it would if the process were gone while the
	// server went on tracking the session.
	if err := os.Remove(dirs.ShimSocket("ghost")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}

	got := mgr.checkTrackedShims(ctx)
	if len(got) != 1 {
		t.Fatalf("checkTrackedShims() = %+v, want one finding", got)
	}
	if got[0].Kind != FindingUnreachableShim {
		t.Errorf("kind = %q, want %q", got[0].Kind, FindingUnreachableShim)
	}
	if got[0].Session != "ghost" {
		t.Errorf("Session = %q, want ghost", got[0].Session)
	}
}

// Pty accounting either works or says it does not, and never reports a healthy system as in trouble.
//
// Not a test of the finding, which would need hundreds of ptys to trigger. It tests the reading the finding
// depends on, since a check whose input is wrong is worse than one that is absent: a bogus limit would either
// cry wolf on every run or stay silent through the incident it exists for.
func TestPtyUsageIsPlausible(t *testing.T) {
	used, limit, ok := ptyUsage()
	if !ok {
		t.Skip("pty accounting is unavailable on this platform")
	}
	if limit <= 0 {
		t.Errorf("ptyUsage() limit = %d, want a positive cap", limit)
	}
	// At least one, since this test is running in a process that has a terminal somewhere above it, and fewer
	// than the cap, since the machine is working.
	if used < 1 || used > limit {
		t.Errorf("ptyUsage() used = %d, want between 1 and the limit %d", used, limit)
	}
}

// A machine that is not out of ptys produces no finding.
//
// The complement of the check being useful: it runs on every `cm doctor`, so a threshold set wrongly would
// make the command always report a problem, and a command that always complains is one nobody reads.
func TestCheckPtyPressureIsQuietOnAHealthySystem(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)

	used, limit, ok := ptyUsage()
	if !ok {
		t.Skip("pty accounting is unavailable on this platform")
	}
	if float64(used) >= float64(limit)*ptyPressureFraction {
		t.Skipf("this machine really is low on ptys (%d of %d), which the check should report", used, limit)
	}
	if got := mgr.checkPtyPressure(); len(got) != 0 {
		t.Errorf("checkPtyPressure() = %+v, want nothing with %d of %d ptys used", got, used, limit)
	}
}

// mustCreateRecord stores a session record naming an output log.
//
// A helper because the fields that matter here are just the name and the log path, and spelling out a full
// store.Session at each call site would bury that.
func mustCreateRecord(t *testing.T, st *store.Store, name, logPath string) {
	t.Helper()
	if err := st.Create(context.Background(), store.Session{
		Name: name, LogPath: logPath, State: store.StateExited,
	}); err != nil {
		t.Fatalf("Create(%q) error = %v", name, err)
	}
}
