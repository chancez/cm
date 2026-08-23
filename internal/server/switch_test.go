package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// fixtureAttachments holds each fixture session's attachment, so leaveWindow can drop it.
//
// A map rather than another return value, because eleven call sites do not need it and would each have to
// ignore one.
var fixtureAttachments = map[*Session]attachment{}

// leaveWindow detaches the fixture's client, standing in for a window that has moved elsewhere.
func leaveWindow(t *testing.T, sess *Session) {
	t.Helper()
	att, ok := fixtureAttachments[sess]
	if !ok {
		t.Fatal("no fixture attachment for this session")
	}
	sess.detach(att)
	delete(fixtureAttachments, sess)
}

// switchFixture is a live session with one client that has typed, plus a second session to switch to.
//
// A real attachment rather than a hand-written clientSizes entry, because the client a switch acts on is
// the one that typed, and that is recorded through the attach path. A fixture that wrote the state
// directly could pass while the path a keystroke actually takes recorded nothing.
func switchFixture(t *testing.T) (*Service, *store.Store, *Session, *attachToken, string) {
	t.Helper()

	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("here", "sleep 30"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, live := mgr.Get("here")
	if !live {
		t.Fatal("session not adopted")
	}

	tok := sess.reserveClient()
	sess.noteClientIdentity(tok, "v1", 1000)
	att, err := sess.attach(nil, tok)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	// Kept so a test can make this window leave. A real client detaches and reattaches elsewhere on its
	// own; nothing here does, so a test that needs the session unwatched has to say so.
	fixtureAttachments[sess] = att
	// The client someone is using is the one that typed most recently, so a switch has nothing to act on
	// until something has been typed.
	sess.noteClientInput(tok)

	// The session being switched to needs no shim: Switch only checks that it exists.
	exitedSession(t, st, "there222")
	nameSession(t, st, "there222")

	return NewService(mgr), st, sess, tok, "there222"
}

func TestSwitchAsksTheActiveClientAndNamesTheTarget(t *testing.T) {
	svc, _, sess, tok, target := switchFixture(t)

	resp, err := svc.Switch(context.Background(), &serverv1.SwitchRequest{
		Session: "here",
		Target:  target,
	})
	if err != nil {
		t.Fatalf("Switch() error = %v", err)
	}
	want := &serverv1.SwitchResponse{
		Asked:      1,
		TargetId:   "there222",
		SwitchedTo: paths.FormatSessionID("there222"),
	}
	if resp.Asked != want.Asked || resp.TargetId != want.TargetId ||
		resp.SwitchedTo != want.SwitchedTo || resp.BoundName != want.BoundName {
		t.Errorf("Switch() = %+v, want %+v", resp, want)
	}

	// The client is told where to come back to, by ID: without --bind the name still points at the
	// session being left, so sending a name would send the window straight back where it started.
	if got := sess.switchTarget(tok); got != paths.FormatSessionID("there222") {
		t.Errorf("switchTarget() = %q, want %q", got, paths.FormatSessionID("there222"))
	}
	// And it is marked as coming back rather than exiting, or the window would close instead of switching.
	if !sess.isUpgrading(tok) {
		t.Error("isUpgrading() = false, want the eviction marked as one the client returns from")
	}
}

// `cm rebind` moves the window's own name to the target and tells the client to come back under that name.
// The name moving is what a restored window follows, since the emulator re-runs the same launch command.
func TestRebindMovesTheNameAndSendsIt(t *testing.T) {
	svc, st, sess, tok, target := switchFixture(t)
	ctx := context.Background()

	resp, err := svc.Switch(ctx, &serverv1.SwitchRequest{
		Session: "here",
		Target:  target,
		Bind:    true,
	})
	if err != nil {
		t.Fatalf("Switch(rebind) error = %v", err)
	}
	if resp.BoundName != "here" || resp.SwitchedTo != "here" || resp.TargetId != "there222" {
		t.Errorf("Switch(rebind) = %+v, want here bound to there222 and sent as the reference", resp)
	}
	if got := sess.switchTarget(tok); got != "here" {
		t.Errorf("switchTarget() = %q, want the window's own name %q", got, "here")
	}

	// The name now resolves to the target, which is what a restored window will find.
	binding, err := st.Binding(ctx, "here")
	if err != nil {
		t.Fatalf("Binding() error = %v", err)
	}
	if binding.SessionID != "there222" {
		t.Errorf("Binding() = %+v, want it pointing at there222", binding)
	}
	// And borrowed, so closing the window releases the name rather than killing a session that lives
	// elsewhere.
	if binding.OnKill != store.KillUnbind {
		t.Errorf("OnKill = %q, want %q for a window that borrowed a session",
			binding.OnKill, store.KillUnbind)
	}
}

// The session switched away from keeps running, which is the difference the ID made: when a name was an
// identity, freeing a window's name meant ending the session holding it.
func TestSwitchLeavesTheOldSessionRunning(t *testing.T) {
	svc, st, sess, _, target := switchFixture(t)
	ctx := context.Background()

	if _, err := svc.Switch(ctx, &serverv1.SwitchRequest{
		Session: "here", Target: target, Bind: true,
	}); err != nil {
		t.Fatalf("Switch() error = %v", err)
	}

	if _, err := st.Get(ctx, sess.id); err != nil {
		t.Errorf("Get() after switching away error = %v, want the session still recorded", err)
	}
	if ended, _ := sess.Ended(); ended {
		t.Error("the session switched away from ended, want it still running")
	}
	// Reachable by ID even though its only name now points elsewhere, which is what keeps it from being
	// stranded.
	if _, err := svc.mgr.Resolve(ctx, paths.FormatSessionID(sess.id)); err != nil {
		t.Errorf("Resolve() by ID error = %v, want the session still reachable", err)
	}
}

// A session with no name has nothing to move, so `cm rebind` is refused rather than silently doing what
// `cm switch` does. Finding out at the next terminal restart is the worst time.
func TestRebindRefusesAnUnnamedSession(t *testing.T) {
	svc, st, sess, _, target := switchFixture(t)
	ctx := context.Background()

	if _, err := st.Unbind(ctx, "here"); err != nil {
		t.Fatalf("Unbind() error = %v", err)
	}

	_, err := svc.Switch(ctx, &serverv1.SwitchRequest{
		Session: paths.FormatSessionID(sess.id),
		Target:  target,
		Bind:    true,
	})
	if err == nil {
		t.Fatal("Switch(rebind) on an unnamed session = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "no name to move") {
		t.Errorf("Switch(rebind) error = %v, want it to name the missing name", err)
	}
}

func TestSwitchToTheSameSessionIsRefused(t *testing.T) {
	svc, _, _, _, _ := switchFixture(t)
	_, err := svc.Switch(context.Background(), &serverv1.SwitchRequest{
		Session: "here",
		Target:  "here",
	})
	if err == nil {
		t.Error("Switch() onto the same session = nil error, want a refusal")
	}
}

func TestSwitchToAMissingSessionChangesNothing(t *testing.T) {
	svc, _, sess, tok, _ := switchFixture(t)

	_, err := svc.Switch(context.Background(), &serverv1.SwitchRequest{
		Session: "here",
		Target:  paths.FormatSessionID("gone2222"),
	})
	if err == nil {
		t.Fatal("Switch() to a missing session = nil error, want a refusal")
	}
	// Checked before anything is asked to move, so the window is still where it was rather than half-way.
	if got := sess.switchTarget(tok); got != "" {
		t.Errorf("switchTarget() = %q, want nothing asked", got)
	}
}

// Nothing typed means no client can be named, and that reports zero rather than switching every window.
// A caller asked for one window; moving all of them because cm cannot tell which would be the opposite.
func TestSwitchWithNoActiveClientAsksNobody(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("quiet", "sleep 30"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, live := mgr.Get("quiet")
	if !live {
		t.Fatal("session not adopted")
	}
	tok := sess.reserveClient()
	sess.noteClientIdentity(tok, "v1", 1000)
	if _, err := sess.attach(nil, tok); err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	// Deliberately no noteClientInput: this is a freshly attached window nobody has typed in.

	exitedSession(t, st, "there222")
	svc := NewService(mgr)
	resp, err := svc.Switch(ctx, &serverv1.SwitchRequest{
		Session: "quiet",
		Target:  paths.FormatSessionID("there222"),
	})
	if err != nil {
		t.Fatalf("Switch() error = %v", err)
	}
	if resp.Asked != 0 {
		t.Errorf("Switch() asked %d, want 0 when no client can be named", resp.Asked)
	}
	if got := sess.switchTarget(tok); got != "" {
		t.Errorf("switchTarget() = %q, want nothing asked", got)
	}
}

// --all-clients moves every window showing the session, not only the one in use.
//
// Covered because the flag inverts the rule the rest of the command rests on: without it a switch acts on
// the client that typed, and a flag that silently kept doing that would look like it worked. Two clients,
// only one of which has typed, so acting on the active one and acting on all of them are distinguishable.
func TestSwitchAllClientsMovesEveryWindow(t *testing.T) {
	svc, _, sess, tok, target := switchFixture(t)

	// A second window on the same session, which has typed nothing: it is not the active client, so the
	// default would leave it where it is.
	quiet := sess.reserveClient()
	sess.noteClientIdentity(quiet, "v1", 2000)
	if _, err := sess.attach(nil, quiet); err != nil {
		t.Fatalf("attach() error = %v", err)
	}

	resp, err := svc.Switch(context.Background(), &serverv1.SwitchRequest{
		Session:    "here",
		Target:     target,
		AllClients: true,
	})
	if err != nil {
		t.Fatalf("Switch(all) error = %v", err)
	}
	if resp.Asked != 2 {
		t.Errorf("Switch(all) asked %d, want 2", resp.Asked)
	}
	want := paths.FormatSessionID("there222")
	for name, got := range map[string]string{
		"the client that typed":   sess.switchTarget(tok),
		"the client that did not": sess.switchTarget(quiet),
	} {
		if got != want {
			t.Errorf("%s was told %q, want %q", name, got, want)
		}
	}
}

// And without the flag the quiet window stays put, which is what makes the flag mean something.
func TestSwitchLeavesOtherWindowsAlone(t *testing.T) {
	svc, _, sess, tok, target := switchFixture(t)

	quiet := sess.reserveClient()
	sess.noteClientIdentity(quiet, "v1", 2000)
	if _, err := sess.attach(nil, quiet); err != nil {
		t.Fatalf("attach() error = %v", err)
	}

	resp, err := svc.Switch(context.Background(), &serverv1.SwitchRequest{
		Session: "here",
		Target:  target,
	})
	if err != nil {
		t.Fatalf("Switch() error = %v", err)
	}
	if resp.Asked != 1 {
		t.Errorf("Switch() asked %d, want 1: only the client that typed", resp.Asked)
	}
	if got := sess.switchTarget(tok); got == "" {
		t.Error("the client that typed was not asked to switch")
	}
	if got := sess.switchTarget(quiet); got != "" {
		t.Errorf("a window that typed nothing was told to switch to %q, want left alone", got)
	}
}

// A rebind with replace ends the session the name moved off, once no window is watching it.
func TestRebindWithReplaceEndsThePreviousSession(t *testing.T) {
	svc, st, sess, _, target := switchFixture(t)
	ctx := context.Background()

	// The window has moved on, which is the state replace waits for: a real client reattaches elsewhere
	// within its own retry, and the kill happens once nothing is watching.
	leaveWindow(t, sess)

	resp, err := svc.Switch(ctx, &serverv1.SwitchRequest{
		Session: "here", Target: target, Bind: true, Replace: true,
		// From inside the session being replaced, which is the ordinary case and the one that skips the
		// busy check.
		CallerSession: "here",
	})
	if err != nil {
		t.Fatalf("Switch(replace) error = %v", err)
	}
	if resp.KilledSession != sess.id {
		t.Errorf("KilledSession = %q, want %q (kept: %q)", resp.KilledSession, sess.id, resp.KeptReason)
	}
	if _, err := st.Get(ctx, sess.id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() error = %v, want the record gone", err)
	}
}

// A session with another name is not this window's alone, so replace leaves it and says why. That name is
// how something else reaches it, and nothing here asked for that to stop working.
func TestRebindWithReplaceRefusesASessionWithOtherNames(t *testing.T) {
	svc, st, sess, _, target := switchFixture(t)
	ctx := context.Background()

	if err := st.Bind(ctx, store.Binding{
		Name: "alsohere", SessionID: sess.id, OnKill: store.KillTarget,
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	_, err := svc.Switch(ctx, &serverv1.SwitchRequest{
		Session: "here", Target: target, Bind: true, Replace: true, CallerSession: "here",
	})
	if err == nil {
		t.Fatal("Switch(replace) on a session with another name = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "other names") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
	// Refused before anything moved, so the window is still where it was.
	if _, err := st.Get(ctx, sess.id); err != nil {
		t.Errorf("Get() error = %v, want the session untouched", err)
	}
	binding, err := st.Binding(ctx, "here")
	if err != nil || binding.SessionID != sess.id {
		t.Errorf("Binding(here) = %+v, %v, want it still on the old session", binding, err)
	}
}

// Replace without moving the name is refused: a switch leaves the name pointing at the session being left,
// so ending it would leave that name resolving to nothing.
func TestReplaceWithoutBindIsRefused(t *testing.T) {
	svc, _, _, _, target := switchFixture(t)

	_, err := svc.Switch(context.Background(), &serverv1.SwitchRequest{
		Session: "here", Target: target, Replace: true,
	})
	if err == nil {
		t.Fatal("Switch(replace without bind) = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "rebind") {
		t.Errorf("error = %v, want it to point at cm rebind", err)
	}
}

// A window still attached after the switch keeps its session alive, and the reason is reported.
//
// Only the client that typed is moved unless --all-clients is given, so a session can still be on someone
// else's screen. Ending it would take that window's session away, which nothing asked for.
func TestRebindWithReplaceKeepsASessionAWindowIsStillWatching(t *testing.T) {
	svc, st, sess, _, target := switchFixture(t)
	ctx := context.Background()

	// Deliberately not leaving: the fixture's client stays attached, which is what a second window on the
	// same session looks like from here.
	resp, err := svc.Switch(ctx, &serverv1.SwitchRequest{
		Session: "here", Target: target, Bind: true, Replace: true, CallerSession: "here",
	})
	if err != nil {
		t.Fatalf("Switch(replace) error = %v", err)
	}
	if resp.KilledSession != "" {
		t.Errorf("KilledSession = %q, want nothing killed while a window is attached", resp.KilledSession)
	}
	if !strings.Contains(resp.KeptReason, "still attached") {
		t.Errorf("KeptReason = %q, want it to say a window is still attached", resp.KeptReason)
	}
	// The name still moved: that half was asked for and is not conditional on the old session going.
	if _, err := st.Get(ctx, sess.id); err != nil {
		t.Errorf("Get() error = %v, want the session still there", err)
	}
	binding, err := st.Binding(ctx, "here")
	if err != nil || binding.SessionID != "there222" {
		t.Errorf("Binding(here) = %+v, %v, want it moved to the target", binding, err)
	}
}
