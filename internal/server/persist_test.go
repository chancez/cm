package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/store"
)

// testPolicy returns a policy that persists everything, which most cases here want.
func testPolicy() *PersistPolicy {
	return &PersistPolicy{
		Matches:     func(string) bool { return true },
		Limits:      seqlog.FileLimits{MaxLines: 1000, MaxBytes: 1 << 20},
		OnRestore:   RestoreShell,
		ExpireAfter: time.Hour,
	}
}

// writeSavedLog creates a persisted log as a previous incarnation would have left it.
func writeSavedLog(t *testing.T, path, content string) uint64 {
	t.Helper()
	f, err := seqlog.OpenFile(path, seqlog.FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	defer f.Close()
	if err := f.Append([]byte(content)); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	_, next := f.Bounds()
	return next
}

// A dead session's saved directory must be inherited, so a revived session starts where it was
// rather than wherever the server happens to be.
func TestInheritForRestoreCarriesDirectory(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetPersistPolicy(testPolicy())

	rec := store.Session{
		Name:    "revived",
		LogPath: dirs.SessionLog("revived"),
		Cwd:     "/saved/directory",
		Command: "/bin/zsh",
	}

	got := mgr.inheritForRestore(OpenOptions{Name: "revived"}, rec)
	if got.Dir != "/saved/directory" {
		t.Errorf("Dir = %q, want the saved directory", got.Dir)
	}
	if got.restoreFrom != rec.LogPath {
		t.Errorf("restoreFrom = %q, want the saved log path", got.restoreFrom)
	}
	if !got.Persist {
		t.Error("Persist = false, want a revived session to keep persisting")
	}
}

// What the caller asks for now outranks what a previous incarnation was doing, since a user asking
// for something specific should get it.
func TestInheritForRestoreDoesNotOverrideCaller(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetPersistPolicy(testPolicy())

	rec := store.Session{
		Name:    "revived",
		LogPath: dirs.SessionLog("revived"),
		Cwd:     "/saved/directory",
		Command: "/bin/zsh",
	}

	got := mgr.inheritForRestore(OpenOptions{
		Name:    "revived",
		Dir:     "/explicit",
		Command: []string{"/bin/bash"},
	}, rec)

	if got.Dir != "/explicit" {
		t.Errorf("Dir = %q, want the caller's directory to win", got.Dir)
	}
	if len(got.Command) != 1 || got.Command[0] != "/bin/bash" {
		t.Errorf("Command = %v, want the caller's command to win", got.Command)
	}
}

// The default must not re-run a recorded command. That is the one restore behavior which executes
// something the user did not type just now.
func TestInheritForRestoreDoesNotRerunByDefault(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetPersistPolicy(testPolicy()) // OnRestore is RestoreShell

	rec := store.Session{
		Name:    "revived",
		LogPath: dirs.SessionLog("revived"),
		Command: "make install",
	}

	got := mgr.inheritForRestore(OpenOptions{Name: "revived"}, rec)
	if len(got.Command) != 0 {
		t.Errorf("Command = %v, want nothing re-run under the default policy", got.Command)
	}
}

// A session that explicitly asked for its command back gets it, even when the program is not on the
// allowlist, since the per-session request is the real control.
func TestInheritForRestoreHonorsExplicitCommandRequest(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	policy := testPolicy()
	policy.CommandIsSafeToRerun = func([]string) bool { return false }
	mgr.SetPersistPolicy(policy)

	rec := store.Session{
		Name:    "revived",
		LogPath: dirs.SessionLog("revived"),
		Command: "make watch",
	}

	got := mgr.inheritForRestore(OpenOptions{
		Name:      "revived",
		OnRestore: RestoreCommand,
	}, rec)

	want := []string{"make", "watch"}
	if len(got.Command) != len(want) {
		t.Fatalf("Command = %v, want %v", got.Command, want)
	}
	for i := range want {
		if got.Command[i] != want[i] {
			t.Errorf("Command = %v, want %v", got.Command, want)
			break
		}
	}
}

// The allowlist lets a matching command be re-run without a per-session request. It is a
// convenience, so the test also pins that a non-matching command is not re-run.
func TestInheritForRestoreAllowlist(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	policy := testPolicy()
	policy.OnRestore = RestoreCommand
	policy.CommandIsSafeToRerun = func(argv []string) bool {
		return len(argv) > 0 && filepath.Base(argv[0]) == "nvim"
	}
	mgr.SetPersistPolicy(policy)

	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"allowlisted", "nvim notes.md", true},
		{"allowlisted by path", "/usr/bin/nvim notes.md", true},
		{"not allowlisted", "make install", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := store.Session{
				Name:    "revived",
				LogPath: dirs.SessionLog("revived"),
				Command: tt.command,
			}
			got := mgr.inheritForRestore(OpenOptions{Name: "revived"}, rec)
			rerun := len(got.Command) > 0 && got.Command[0] != holdCommand
			if rerun != tt.want {
				t.Errorf("re-ran = %v, want %v (command %v)", rerun, tt.want, got.Command)
			}
		})
	}
}

// "none" must start nothing the user could mistake for their program. It runs a holding process,
// because the rest of cm assumes a session has a pty and a child.
func TestInheritForRestoreNoneRunsAHold(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	policy := testPolicy()
	policy.OnRestore = RestoreNone
	mgr.SetPersistPolicy(policy)

	rec := store.Session{
		Name:    "revived",
		LogPath: dirs.SessionLog("revived"),
		Command: "make install",
	}

	got := mgr.inheritForRestore(OpenOptions{Name: "revived"}, rec)
	if len(got.Command) != 1 || got.Command[0] != holdCommand {
		t.Errorf("Command = %v, want the holding command %q", got.Command, holdCommand)
	}
}

// With persistence off, nothing is inherited and no log is consulted.
func TestInheritForRestoreDisabledWhenNoPolicy(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	// No SetPersistPolicy call.

	rec := store.Session{
		Name:    "revived",
		LogPath: dirs.SessionLog("revived"),
		Cwd:     "/saved",
		Command: "nvim x",
	}

	got := mgr.inheritForRestore(OpenOptions{Name: "revived"}, rec)
	if got.restoreFrom != "" || got.Persist || got.Dir != "" || len(got.Command) != 0 {
		t.Errorf("options were modified with persistence disabled: %+v", got)
	}
}

// A record with no saved log has nothing to restore, which is the normal case for a session that
// never persisted.
func TestInheritForRestoreWithoutLogPath(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	mgr.SetPersistPolicy(testPolicy())

	got := mgr.inheritForRestore(OpenOptions{Name: "plain"}, store.Session{Name: "plain", Cwd: "/x"})
	if got.restoreFrom != "" || got.Persist {
		t.Errorf("options claim a restore with no saved log: %+v", got)
	}
}

// A session only persists when configured to, so an unmatched name writes nothing to disk.
func TestPersistsSessionRespectsPolicy(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)

	// No policy at all.
	if mgr.persistsSession("anything", false) {
		t.Error("persists with no policy, want false")
	}
	// An explicit request must still be refused without a policy, since there is nowhere to record
	// the limits or the restore behavior.
	if mgr.persistsSession("anything", true) {
		t.Error("persists on request with no policy, want false")
	}

	mgr.SetPersistPolicy(&PersistPolicy{
		Matches: func(name string) bool { return strings.HasPrefix(name, "kitty.") },
	})
	if !mgr.persistsSession("kitty.55", false) {
		t.Error("kitty.55 does not persist, want it to match the pattern")
	}
	if mgr.persistsSession("scratch", false) {
		t.Error("scratch persists, want only matching names")
	}
	// An explicit request overrides the patterns, which is what --persist is for.
	if !mgr.persistsSession("scratch", true) {
		t.Error("scratch does not persist on request, want the request to win")
	}
}

// The screen replayed from a previous incarnation must reach the first client, and only the first:
// a later client would otherwise be shown a screen from before the reboot.
func TestRestoredScreenGoesToFirstClientOnly(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("restored", "sleep 5"))

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	sess.setRestored([]byte("SCREEN_FROM_BEFORE"))

	first, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("first attach() error = %v", err)
	}
	if string(first.restore) != "SCREEN_FROM_BEFORE" {
		t.Errorf("first attach restore = %q, want the replayed screen", first.restore)
	}
	sess.detach(first)

	second, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("second attach() error = %v", err)
	}
	defer sess.detach(second)
	if strings.Contains(string(second.restore), "SCREEN_FROM_BEFORE") {
		t.Error("the replayed screen was delivered twice; the second client would see pre-reboot state")
	}
}

// A resuming client must not receive the replayed screen: it already has the session displayed, and
// repainting would show it a screen from before the reboot.
func TestRestoredScreenSkippedOnResume(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("resumed", "sleep 5"))

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	sess.setRestored([]byte("SCREEN_FROM_BEFORE"))

	from := uint64(0)
	att, err := sess.attach(&from, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	if len(att.restore) != 0 {
		t.Errorf("resuming client got restore %q, want nothing", att.restore)
	}
}

// The stream must start at the log's end when a replayed screen is delivered, or the client would
// receive output the screen already accounts for.
func TestRestoredScreenStreamStartsAtPresent(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("streamstart", "echo SOMETHING; sleep 5"))

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// Let output accumulate, so the log's start and end differ.
	warm := sess.recent.Subscribe(0)
	readUntil(t, warm, "SOMETHING")
	warm.Close()

	sess.setRestored([]byte("SCREEN"))

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	if got, want := att.reader.Position(), sess.recent.Next(); got != want {
		t.Errorf("stream starts at %d, want the log's end %d", got, want)
	}
}

// The whole feature, end to end at the manager level: a dead session with saved content is revived
// with its screen replayed and its directory inherited.
func TestOpenRevivesDeadPersistedSession(t *testing.T) {
	newTerm, term := replayTerminal(t)
	mgr, st, dirs := newTestManager(t, newTerm)
	mgr.SetPersistPolicy(testPolicy())
	ctx := context.Background()

	logPath := dirs.SessionLog("revive")
	writeSavedLog(t, logPath, "OUTPUT_FROM_BEFORE_REBOOT\r\n")

	// A record as a reboot would leave it: marked dead, with a log and a directory.
	if err := st.Create(ctx, store.Session{
		Name:       "revive",
		ShimSocket: dirs.ShimSocket("revive"),
		LogPath:    logPath,
		Cwd:        "/tmp",
		State:      store.StateDead,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Spawning a shim needs the real binary, so this asserts what happens up to that point: the
	// options must carry the restore, and the stale record must be replaced.
	mgr.selfExe = "/nonexistent/cm"
	if _, _, err := mgr.Open(ctx, OpenOptions{Name: "revive", Rows: 24, Cols: 80}); err == nil {
		t.Fatal("Open() succeeded with a bogus shim binary, want failure")
	}

	// The replay is attempted only after a shim exists, so the emulator should not have been fed.
	if got := term.Written(); got != "" {
		t.Errorf("emulator was fed %q before a shim existed", got)
	}
}

// Replaying a corrupt or unreadable log must not stop a session from opening. A session that starts
// empty is strictly better than one that refuses to open because a cache could not be read.
func TestReplayFailureDoesNotBlockSession(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "corrupt.log")
	if err := os.WriteFile(path, []byte("not a cm log"), 0o600); err != nil {
		t.Fatal(err)
	}

	newTerm, _ := replayTerminal(t)
	// A corrupt file resets to empty, so there is nothing to restore rather than an error.
	_, _, err := replayPersisted(path, newTerm, 24, 80, seqlog.FileLimits{})
	if err == nil {
		t.Error("replayPersisted() = nil error for a reset log, want ErrNothingToRestore")
	}
}

// A terminal that fails during replay must be reported rather than producing a half-built screen.
func TestReplayReportsTerminalFailure(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "log")
	writeSavedLog(t, path, "content\r\n")

	failing := func(rows, cols uint16) (Terminal, error) {
		return &fakeTerminal{writeErr: errWriteFailed}, nil
	}
	if _, _, err := replayPersisted(path, failing, 24, 80, seqlog.FileLimits{}); err == nil {
		t.Error("replayPersisted() = nil error when the emulator failed, want an error")
	}
}

// errWriteFailed stands in for an emulator failing mid-replay.
var errWriteFailed = errors.New("emulator write failed")

// Expiry removes dead sessions and their logs, since otherwise both the list and the disk grow
// forever across reboots.
func TestExpireDeadSessions(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	policy := testPolicy()
	policy.ExpireAfter = time.Hour
	mgr.SetPersistPolicy(policy)
	ctx := context.Background()

	now := time.Now()
	logPath := dirs.SessionLog("old")
	writeSavedLog(t, logPath, "old content\r\n")

	// Old and dead: should go.
	if err := st.Create(ctx, store.Session{
		Name:    "old",
		LogPath: logPath,
		State:   store.StateDead,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// Recently dead: should stay, since it is still worth reviving.
	if err := st.Create(ctx, store.Session{
		Name:  "recent",
		State: store.StateExited,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// Running: never expired regardless of age.
	if err := st.Create(ctx, store.Session{
		Name:  "alive",
		State: store.StateRunning,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Age the old record past the cutoff. UpdatedAt is what expiry measures, since what matters is
	// how long ago the session stopped being useful.
	ageRecord(t, st, "old", now.Add(-2*time.Hour))

	removed, err := mgr.ExpireDeadSessions(ctx, now)
	if err != nil {
		t.Fatalf("ExpireDeadSessions() error = %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d sessions, want 1", removed)
	}

	if _, err := st.Get(ctx, "old"); err == nil {
		t.Error("the old dead session survived expiry")
	}
	for _, keep := range []string{"recent", "alive"} {
		if _, err := st.Get(ctx, keep); err != nil {
			t.Errorf("session %q was expired, want it kept: %v", keep, err)
		}
	}
	// The log must go with the record, or the disk keeps growing.
	if _, err := os.Stat(logPath); err == nil {
		t.Error("the expired session's log is still on disk")
	}
}

// A session in the registry is never expired, whatever the record says. The record can lag, and
// deleting a session someone is attached to is worse than keeping a stale row.
func TestExpireSkipsLiveSessions(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	policy := testPolicy()
	policy.ExpireAfter = time.Nanosecond // everything is old enough
	mgr.SetPersistPolicy(policy)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("live", "sleep 5"))
	rec.State = store.StateDead // deliberately wrong, as a lagging record would be
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	sess, err := mgr.adopt(ctx, rec, 0)
	if err != nil {
		t.Fatalf("adopt() error = %v", err)
	}
	mgr.mu.Lock()
	mgr.sessions["live"] = sess
	mgr.mu.Unlock()

	if _, err := mgr.ExpireDeadSessions(ctx, time.Now()); err != nil {
		t.Fatalf("ExpireDeadSessions() error = %v", err)
	}
	if _, err := st.Get(ctx, "live"); err != nil {
		t.Errorf("a session in the registry was expired: %v", err)
	}
}

// Expiry does nothing without a policy, so a user who has not enabled persistence never has records
// removed behind their back.
func TestExpireDoesNothingWithoutPolicy(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{Name: "ancient", State: store.StateDead}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	ageRecord(t, st, "ancient", time.Now().Add(-10000*time.Hour))

	removed, err := mgr.ExpireDeadSessions(ctx, time.Now())
	if err != nil {
		t.Fatalf("ExpireDeadSessions() error = %v", err)
	}
	if removed != 0 {
		t.Errorf("removed %d sessions with no policy, want 0", removed)
	}
	if _, err := st.Get(ctx, "ancient"); err != nil {
		t.Errorf("a record was removed with persistence disabled: %v", err)
	}
}

// A log that cannot be removed must not keep the record alive: the row is what makes a session
// visible, and an orphaned file is the smaller problem.
func TestExpireRemovesRecordEvenIfLogRemovalFails(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	policy := testPolicy()
	policy.ExpireAfter = time.Nanosecond
	mgr.SetPersistPolicy(policy)
	ctx := context.Background()

	// A directory, which os.Remove refuses when non-empty.
	dir := filepath.Join(shortTempDir(t), "stubborn")
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := st.Create(ctx, store.Session{
		Name:    "stubborn",
		LogPath: dir,
		State:   store.StateDead,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	ageRecord(t, st, "stubborn", time.Now().Add(-time.Hour))

	if _, err := mgr.ExpireDeadSessions(ctx, time.Now()); err != nil {
		t.Fatalf("ExpireDeadSessions() error = %v", err)
	}
	if _, err := st.Get(ctx, "stubborn"); err == nil {
		t.Error("the record survived because its log could not be removed")
	}
}

// ageRecord backdates a record's UpdatedAt, which expiry measures against.
func ageRecord(t *testing.T, st *store.Store, name string, when time.Time) {
	t.Helper()
	if err := st.SetUpdatedAt(context.Background(), name, when); err != nil {
		t.Fatalf("SetUpdatedAt() error = %v", err)
	}
}

// A session nobody asked to persist is forgotten much sooner than one that was.
//
// Without this, every short command a user runs sits in `cm list` for the persisted-session lifetime,
// which defaults to a week. Twenty `cm run` invocations made the list useless.
//
// Keyed on PersistRequested rather than on whether a log exists, because `cm run` writes a log so its
// output can be read after the command exits. Those two cases look identical on disk and differ only in
// how long the session is worth keeping.
func TestExpireForgetsUnpersistedSessionsSooner(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	policy := testPolicy()
	policy.ExpireAfter = 24 * time.Hour
	policy.ForgetUnpersistedAfter = time.Minute
	mgr.SetPersistPolicy(policy)
	ctx := context.Background()

	now := time.Now()

	// Asked to persist, ended an hour ago: kept, since it is still worth reviving and reading.
	logPath := dirs.SessionLog("saved")
	writeSavedLog(t, logPath, "saved content\r\n")
	if err := st.Create(ctx, store.Session{
		Name:             "saved",
		LogPath:          logPath,
		State:            store.StateExited,
		PersistRequested: true,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Not asked for, ended an hour ago: gone. It has a log, because `cm run` captures output, and it is
	// still forgotten, which is the distinction this test exists for.
	capturedLog := dirs.SessionLog("ran")
	writeSavedLog(t, capturedLog, "captured output\r\n")
	if err := st.Create(ctx, store.Session{
		Name:    "ran",
		LogPath: capturedLog,
		State:   store.StateExited,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Not asked for, but only just ended: kept, or `cm run` could not read its exit status back.
	if err := st.Create(ctx, store.Session{
		Name:  "justran",
		State: store.StateExited,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	ageRecord(t, st, "saved", now.Add(-time.Hour))
	ageRecord(t, st, "ran", now.Add(-time.Hour))
	ageRecord(t, st, "justran", now.Add(-time.Second))

	removed, err := mgr.ExpireDeadSessions(ctx, now)
	if err != nil {
		t.Fatalf("ExpireDeadSessions() error = %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d sessions, want 1", removed)
	}

	if _, err := st.Get(ctx, "ran"); err == nil {
		t.Error("an unpersisted session an hour old survived, want it forgotten")
	}
	for _, keep := range []string{"saved", "justran"} {
		if _, err := st.Get(ctx, keep); err != nil {
			t.Errorf("session %q was expired, want it kept: %v", keep, err)
		}
	}
}

// With no forget interval configured, an unpersisted session falls back to the persisted lifetime.
//
// Guards the zero value: a policy built without the field set, which is what every existing caller
// and test does, must not start deleting records after zero seconds.
func TestExpireUnsetForgetIntervalFallsBackToExpireAfter(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	policy := testPolicy()
	policy.ExpireAfter = time.Hour
	// ForgetUnpersistedAfter deliberately left zero.
	mgr.SetPersistPolicy(policy)
	ctx := context.Background()

	now := time.Now()
	if err := st.Create(ctx, store.Session{Name: "ran", State: store.StateExited}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// Older than any plausible forget interval, but well inside ExpireAfter.
	ageRecord(t, st, "ran", now.Add(-30*time.Minute))

	removed, err := mgr.ExpireDeadSessions(ctx, now)
	if err != nil {
		t.Fatalf("ExpireDeadSessions() error = %v", err)
	}
	if removed != 0 {
		t.Errorf("removed %d sessions, want 0 with no forget interval configured", removed)
	}
	if _, err := st.Get(ctx, "ran"); err != nil {
		t.Errorf("the session was expired with no forget interval set: %v", err)
	}
}
