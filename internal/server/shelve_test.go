package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/shim"
	"github.com/chancez/cm/internal/store"
)

// startDeclaredProgram starts a persisted session whose shell is inside a command frame with a declared
// resume command, the shape `cm restore-with` leaves, and returns its live session and its record.
//
// hupTrap is the shell's answer to the hangup: a trap body, or "" to ignore the signal. The shell waits in
// a builtin rather than on a child, since `sleep & wait` let the hangup end the wait without running the
// trap. See TestKillWithKeepKeepsTheResumeCommandTheHangupWouldDrop.
func startDeclaredProgram(
	t *testing.T, mgr *Manager, st *store.Store, dirs paths.Dirs, id, hupTrap string,
) (*Session, store.Session) {
	t.Helper()
	dir := shortTempDir(t)
	open, closeFrame := filepath.Join(dir, "open"), filepath.Join(dir, "close")
	if err := os.WriteFile(open, osc.FrameSequence(id+"-1", "claude", false), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(closeFrame, osc.FrameSequence(id+"-1", "", true), 0o600); err != nil {
		t.Fatal(err)
	}
	trap := "trap '' HUP"
	if hupTrap != "" {
		trap = fmt.Sprintf("c=$(cat %s); trap '%s' HUP", closeFrame, hupTrap)
	}
	script := fmt.Sprintf("%s; cat %s; while :; do read -r _; done", trap, open)

	rec := startShimInRuntimeDir(t, dirs, id, script)
	rec.LogPath = dirs.SessionLog(id)
	rec.PersistRequested = true
	rec.Cwd = "/tmp"
	rec.Tags = map[string]string{"kind": "agent"}
	recordSession(t, st, rec)
	if err := os.WriteFile(rec.LogPath, []byte("output worth restoring to\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, live := mgr.Get(id)
	if !live {
		t.Fatal("session was not adopted, so the live path is not under test")
	}
	pollUntil(t, "the program's frame to open", func() bool { return len(sess.Location()) == 1 })
	sess.SetResume([]string{"claude", "--resume", "abc"})
	want := store.ResumeCommand{Argv: []string{"claude", "--resume", "abc"}, Frame: id + "-1"}
	pollUntil(t, "the declaration to be recorded", func() bool {
		got, err := st.Get(ctx, id)
		return err == nil && reflect.DeepEqual(got.Resume, want)
	})
	before, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	return sess, before
}

// shellPIDOf asks a live session's shim for its shell's pid.
func shellPIDOf(t *testing.T, sess *Session) int {
	t.Helper()
	st, err := sess.State(context.Background())
	if err != nil || st.ShellPid <= 0 {
		t.Fatalf("State() = %v, %v; want a shell pid", st, err)
	}
	return int(st.ShellPid)
}

// shelvedFrom is the record shelving should leave from the one before it: everything kept, the state
// shelved, no exit code, the given retention, and the positions the shell's end moved.
func shelvedFrom(before, got store.Session, expiry store.Expiry) store.Session {
	want := before
	want.State = store.StateShelved
	want.ExitCode = 0
	want.ExpireAfter = expiry
	want.LastSeq, want.ClientSeq, want.UpdatedAt = got.LastSeq, got.ClientSeq, got.UpdatedAt
	return want
}

// Shelving a running session ends its shell and keeps everything a restore needs.
//
// The whole record is compared with the one before, so the resume command, the tags, the directory and the
// log path are each asserted as kept. The shell is asserted gone, not just the shim: a restore that started a
// second copy of a program still holding its files is the failure the wait for its exit prevents. The shell
// here behaves like zsh, writing its frame close on the hangup, which is what withdrew the declaration
// before `cm kill --keep` froze it.
func TestShelveEndsTheShellAndKeepsWhatARestoreNeeds(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	mgr.SetPersistPolicy(testPolicy())
	ctx := context.Background()

	sess, before := startDeclaredProgram(t, mgr, st, dirs, "agent", `printf %s "$c"; exit 0`)
	shell := shellPIDOf(t, sess)

	got, err := mgr.Shelve(ctx, "agent", 0)
	if err != nil || got != Shelved {
		t.Fatalf("Shelve() = %v, %v; want Shelved", got, err)
	}
	if shim.ProcessLives(shell) {
		t.Errorf("shell %d is still running after Shelve returned", shell)
	}
	rec := waitForEndedRecord(t, st, "agent")
	if want := shelvedFrom(before, rec, store.ExpireNever); !reflect.DeepEqual(rec, want) {
		t.Errorf("record after Shelve = %+v\nwant %+v", rec, want)
	}
	if _, err := os.Stat(before.LogPath); err != nil {
		t.Errorf("Stat(%s) error = %v, want the saved output kept", before.LogPath, err)
	}
	if id, err := mgr.Resolve(ctx, "agent"); err != nil || id != "agent" {
		t.Errorf("Resolve(agent) = %q, %v; want the name kept", id, err)
	}
}

// A shell that ignores the hangup is killed once the wait runs out, and the session is still shelved.
//
// Without the escalation shelving would either report success over a live shell or wait forever.
func TestShelveKillsAShellThatIgnoresTheHangup(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out shelveExitWait")
	}
	mgr, st, dirs := newTestManager(t, nil)
	mgr.SetPersistPolicy(testPolicy())
	ctx := context.Background()

	sess, before := startDeclaredProgram(t, mgr, st, dirs, "stubborn", "")
	shell := shellPIDOf(t, sess)

	start := time.Now()
	if got, err := mgr.Shelve(ctx, "stubborn", 0); err != nil || got != Shelved {
		t.Fatalf("Shelve() = %v, %v; want Shelved", got, err)
	}
	if shim.ProcessLives(shell) {
		t.Errorf("shell %d is still running after Shelve returned", shell)
	}
	if took := time.Since(start); took < shelveExitWait {
		t.Errorf("Shelve took %v, want at least the %v wait before escalating", took, shelveExitWait)
	}
	rec := waitForEndedRecord(t, st, "stubborn")
	if want := shelvedFrom(before, rec, store.ExpireNever); !reflect.DeepEqual(rec, want) {
		t.Errorf("record after Shelve = %+v\nwant %+v", rec, want)
	}
}

// A session whose shell has already ended is marked shelved as it stands, keeping its declaration, and
// shelving it again is reported as nothing to do.
func TestShelveMarksAnEndedSession(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	mgr.SetPersistPolicy(testPolicy())
	ctx := context.Background()

	for _, state := range []store.State{store.StateExited, store.StateDead} {
		id := string(state)
		rec := store.Session{
			ID:         id,
			ShimSocket: dirs.ShimSocket(id),
			LogPath:    dirs.SessionLog(id),
			State:      state,
			ExitCode:   3,
			Cwd:        "/tmp",
			Resume:     store.ResumeCommand{Argv: []string{"nvim", "-S", "s.vim"}, Frame: "f-1"},
		}
		recordSession(t, st, rec)
		before, _ := st.Get(ctx, id)

		if got, err := mgr.Shelve(ctx, id, store.Expiry(720*time.Hour)); err != nil || got != Shelved {
			t.Fatalf("Shelve(%s) = %v, %v; want Shelved", id, got, err)
		}
		after, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if want := shelvedFrom(before, after, store.Expiry(720*time.Hour)); !reflect.DeepEqual(after, want) {
			t.Errorf("record after shelving a %s session = %+v\nwant %+v", state, after, want)
		}

		if got, err := mgr.Shelve(ctx, id, 0); err != nil || got != AlreadyShelved {
			t.Errorf("second Shelve(%s) = %v, %v; want AlreadyShelved", id, got, err)
		}
		again, _ := st.Get(ctx, id)
		if !reflect.DeepEqual(again, after) {
			t.Errorf("record after a second Shelve = %+v\nwant it unchanged, %+v", again, after)
		}
	}
}

// A session that saved no output is refused and left alone: a restore would come back to an empty screen and
// a fresh shell, which is a new session wearing the old name.
func TestShelveRefusesASessionWithNothingToRestoreTo(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	mgr.SetPersistPolicy(testPolicy())
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "plain", "while :; do read -r _; done")
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	before, _ := st.Get(ctx, "plain")

	if _, err := mgr.Shelve(ctx, "plain", 0); !errors.Is(err, ErrNothingSaved) {
		t.Fatalf("Shelve() error = %v, want ErrNothingSaved", err)
	}
	after, _ := st.Get(ctx, "plain")
	if !reflect.DeepEqual(after, before) {
		t.Errorf("record after a refused Shelve = %+v\nwant it unchanged, %+v", after, before)
	}
	if _, live := mgr.Get("plain"); !live {
		t.Error("the session was stopped by a refused Shelve")
	}
}

// The sweep keeps a shelved session past every interval unless it was given a retention, and honors one
// it was given.
//
// Both records end up the same age and the same state, so the retention is the only difference between
// them, and the configured expire_after would remove both.
func TestExpireKeepsAShelvedSessionUnlessItWasGivenARetention(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	policy := testPolicy()
	policy.ExpireAfter = time.Hour
	mgr.SetPersistPolicy(policy)
	ctx := context.Background()
	now := time.Now()

	for id, expiry := range map[string]store.Expiry{"forever": 0, "brief": store.Expiry(30 * time.Minute)} {
		recordSession(t, st, store.Session{
			ID: id, ShimSocket: dirs.ShimSocket(id), LogPath: dirs.SessionLog(id),
			State: store.StateExited, PersistRequested: true,
		})
		if _, err := mgr.Shelve(ctx, id, expiry); err != nil {
			t.Fatalf("Shelve(%s) error = %v", id, err)
		}
		ageRecord(t, st, id, now.Add(-10000*time.Hour))
	}

	if removed, err := mgr.ExpireDeadSessions(ctx, now); err != nil || removed != 1 {
		t.Fatalf("ExpireDeadSessions() = %d, %v; want 1, nil", removed, err)
	}
	records, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var kept []string
	for _, rec := range records {
		kept = append(kept, fmt.Sprintf("%s=%s/%s", rec.ID, rec.State, rec.ExpireAfter))
	}
	slices.Sort(kept)
	if want := []string{"forever=shelved/never"}; !slices.Equal(kept, want) {
		t.Errorf("kept %q, want %q", kept, want)
	}
}

// Every ended session that saved output comes back the same way, whatever ended it: an attach's command and
// the client's implicit directory are ignored, an explicit directory is kept, and the declared resume command,
// else the recorded command, is what on_restore brings back. A run, and a session that saved nothing, keep
// the caller's command and directory.
//
// The whole options each time, since what a revive starts is all of them: the command, the directory, the
// typed resume command, the log to restore from. The exited and dead rows are the reboot and crash paths,
// which used to run the attach's command in the client's directory and so buried the declared one.
func TestRestoreOptionsBringEveryEndedSessionBackAsItWas(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	policy := testPolicy()
	policy.OnRestore = RestoreCommand
	policy.CommandIsSafeToRerun = func([]string) bool { return true }
	mgr.SetPersistPolicy(policy)

	claude := []string{"claude", "--resume", "abc"}
	rec := store.Session{
		ID:      "agent",
		LogPath: dirs.SessionLogDir("agent"),
		Cwd:     "/srv/project",
		Command: []string{"htop", "-d", "5"},
		Resume:  store.ResumeCommand{Argv: claude, Frame: "old-1"},
		Tags:    map[string]string{"kind": "agent"},
	}
	asked := OpenOptions{
		Ref: "agent", Rows: 40, Cols: 120, Command: []string{"/bin/sh"}, InShell: true, Dir: "/home/me",
		name: "agent",
	}
	restored := OpenOptions{
		Ref: "agent", Rows: 40, Cols: 120, Persist: true, Tags: rec.Tags, restoreFrom: rec.LogPath,
		name: "agent", Dir: rec.Cwd, typeOnStart: &typedCommand{argv: claude, run: true, declared: true},
	}

	type row struct {
		name string
		rec  func(store.Session) store.Session
		opts OpenOptions
		want OpenOptions
	}
	var tests []row
	for _, state := range []store.State{store.StateShelved, store.StateExited, store.StateDead} {
		tests = append(tests, row{
			string(state) + " ignores the command and the client's directory",
			func(r store.Session) store.Session { r.State = state; return r }, asked, restored,
		})
	}
	tests = append(tests, []row{
		{"an explicit directory is kept",
			func(r store.Session) store.Session { r.State = store.StateDead; return r },
			func() OpenOptions { o := asked; o.Dir, o.DirExplicit = "/elsewhere", true; return o }(),
			func() OpenOptions { w := restored; w.Dir, w.DirExplicit = "/elsewhere", true; return w }()},
		{"with nothing declared the recorded command comes back",
			func(r store.Session) store.Session {
				r.State, r.Resume = store.StateExited, store.ResumeCommand{}
				return r
			}, asked,
			func() OpenOptions {
				w := restored
				w.Command, w.typeOnStart = []string{"htop", "-d", "5"}, nil
				return w
			}()},
		{"a run keeps its own command and directory",
			func(r store.Session) store.Session { r.State = store.StateExited; return r },
			func() OpenOptions { o := asked; o.CaptureOutput = true; return o }(),
			func() OpenOptions {
				w := restored
				w.Command, w.InShell, w.Dir, w.CaptureOutput, w.typeOnStart = asked.Command, true, asked.Dir, true, nil
				return w
			}()},
		{"a session that saved nothing starts afresh as asked",
			func(r store.Session) store.Session { r.State, r.LogPath = store.StateExited, ""; return r }, asked,
			func() OpenOptions { w := asked; w.Tags = rec.Tags; return w }()},
	}...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mgr.restoreOptions(tt.opts, tt.rec(rec)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("restoreOptions() = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
