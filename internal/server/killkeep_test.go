package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
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

// `cm kill --keep --expire-after` records the retention with the session, and leaves the rest of the record
// as a plain --keep would.
//
// Asserted against the record as it stood before the kill, with only what the shell's end changes copied
// across, so a retention written by replacing the row rather than one column shows as the difference.
func TestKillWithKeepRecordsItsRetention(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "suspend", "sleep 30")
	rec.LogPath = dirs.SessionLog("suspend")
	rec.PersistRequested = true
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, live := mgr.Get("suspend"); !live {
		t.Fatal("session was not adopted, so the live path is not under test")
	}
	before, err := st.Get(ctx, "suspend")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	if _, err := mgr.Kill(ctx, "suspend", KillOptions{Keep: true, ExpireAfter: store.ExpireNever}); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}

	got := waitForEndedRecord(t, st, "suspend")
	want := before
	want.State = store.StateExited
	want.ExpireAfter = store.ExpireNever
	want.ExitCode, want.LastSeq, want.ClientSeq, want.UpdatedAt = got.ExitCode, got.LastSeq, got.ClientSeq, got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("record after the kill = %+v\nwant %+v", got, want)
	}
}

// A retention reaches a session whose shell had already ended, which is the one kill --keep otherwise does
// nothing to.
//
// A suspend does not know whether the program it is suspending already exited, and dropping the retention
// there would leave that session on the week's schedule it asked to be taken off.
func TestKillWithKeepRecordsARetentionOnAnEndedSession(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := store.Session{
		ID:               "ended",
		ShimSocket:       dirs.ShimSocket("ended"),
		State:            store.StateExited,
		PersistRequested: true,
		Space:            "cm",
	}
	recordSession(t, st, rec)

	if _, err := mgr.Kill(ctx, "ended", KillOptions{Keep: true, ExpireAfter: store.Expiry(720 * time.Hour)}); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	got, err := st.Get(ctx, "ended")
	if err != nil {
		t.Fatalf("Get() error = %v, want the record kept", err)
	}
	want := rec
	want.ExpireAfter = store.Expiry(720 * time.Hour)
	want.CreatedAt, want.UpdatedAt = got.CreatedAt, got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("record after the kill = %+v\nwant %+v", got, want)
	}
}

// The service refuses a retention without keep, or one it cannot read, for the whole request and before
// anything is killed.
//
// The client checks both too, but the server is what a remote or a script talking to the socket reaches, and
// killing the sessions while dropping the retention would forget exactly what was asked to be kept.
func TestServiceKillValidatesExpireAfter(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()
	recordSession(t, st, store.Session{ID: "ended", ShimSocket: dirs.ShimSocket("ended"), State: store.StateExited})

	for _, tc := range []struct {
		req  *serverv1.KillRequest
		want string
	}{
		{&serverv1.KillRequest{Sessions: []string{"ended"}, ExpireAfter: "never"},
			"expire_after needs keep: a killed session is forgotten at once"},
		{&serverv1.KillRequest{Sessions: []string{"ended"}, Keep: true, ExpireAfter: "0s"},
			`expiry must be positive or never, got "0s"`},
	} {
		resp, err := svc.Kill(ctx, tc.req)
		if resp != nil || err == nil || err.Error() != tc.want {
			t.Errorf("Kill(%v) = %v, %v; want nil, %q", tc.req, resp, err, tc.want)
		}
	}
	if _, err := st.Get(ctx, "ended"); err != nil {
		t.Errorf("Get() error = %v, want the session left alone by a refused request", err)
	}
}

// A program's declared resume command survives the kill that suspends it, even though the hangup closes its
// frame on the way out.
//
// The regression: zsh answers SIGHUP by printing "zsh: hangup" and drawing a prompt before it dies, so the
// frame the declaration was bound to closed 36ms after `cm kill --keep` arrived and the declaration went
// with it, and the suspended session revived into a bare shell. The shell here does the same thing on
// purpose: it traps the hangup and writes the frame close before exiting, which is the ordering a unit test
// of the frame stack alone cannot show, since it is the real shim's kill that has to set it off.
//
// Both the session's own copy and the record are checked: the record is what a revive reads, and it is
// written from the session's copy as it changes.
func TestKillWithKeepKeepsTheResumeCommandTheHangupWouldDrop(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	dir := shortTempDir(t)
	open, closeFrame := filepath.Join(dir, "open"), filepath.Join(dir, "close")
	if err := os.WriteFile(open, osc.FrameSequence("sh-1", "claude", false), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(closeFrame, osc.FrameSequence("sh-1", "", true), 0o600); err != nil {
		t.Fatal(err)
	}
	// The trap goes in before the frame opens, since the frame opening is what the test waits for before
	// killing, and the shell then waits in a builtin rather than on a child. `sleep 30 & wait` let the
	// hangup kill the sleep and wait return normally, so the shell exited 0 without running the trap 11
	// runs in 150 and the frame never closed; this shape closed it in 200 of 200.
	script := fmt.Sprintf("c=$(cat %s); trap 'printf %%s \"$c\"; exit 77' HUP; cat %s; while :; do read -r _; done",
		closeFrame, open)
	want := store.ResumeCommand{Argv: []string{"claude", "--resume", "abc"}, Frame: "sh-1"}

	rec := startShimInRuntimeDir(t, dirs, "agent", script)
	rec.LogPath = dirs.SessionLog("agent")
	rec.PersistRequested = true
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, live := mgr.Get("agent")
	if !live {
		t.Fatal("session was not adopted, so the live path is not under test")
	}
	pollUntil(t, "the program's frame to open", func() bool { return len(sess.Location()) == 1 })
	sess.SetResume([]string{"claude", "--resume", "abc"})
	pollUntil(t, "the declaration to be recorded", func() bool {
		got, err := st.Get(ctx, "agent")
		return err == nil && reflect.DeepEqual(got.Resume, want)
	})

	if _, err := mgr.Kill(ctx, "agent", KillOptions{Keep: true, ExpireAfter: store.ExpireNever}); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	<-sess.Done()
	got := waitForEndedRecord(t, st, "agent")
	// The case under test happened: the trap ran, and its close was read.
	if loc := sess.Location(); len(loc) != 0 || got.ExitCode != 77 {
		t.Fatalf("Location() = %+v, exit code %d; want the hangup's trap to have closed the frame and exited 77",
			loc, got.ExitCode)
	}
	if r := sess.Resume(); !reflect.DeepEqual(r, want) {
		t.Errorf("Resume() after the kill = %+v, want %+v", r, want)
	}
	if !reflect.DeepEqual(got.Resume, want) {
		t.Errorf("recorded Resume after the kill = %+v, want %+v", got.Resume, want)
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

// pollUntil waits up to 10s for a condition the session's goroutines bring about.
func pollUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 10s waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
