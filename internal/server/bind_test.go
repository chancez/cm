package server

import (
	"context"
	"errors"
	"testing"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// exitedSession records a session with no live shim, which is enough for every test here.
//
// Deliberately not a real shim: what is under test is which rows change, and a kill of an exited record
// removes it without anything to shut down. A pty would only add a way for these to be flaky.
func exitedSession(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.Create(context.Background(), store.Session{
		ID:    id,
		State: store.StateExited,
	}); err != nil {
		t.Fatalf("Create(%s) error = %v", id, err)
	}
}

func TestBindPointsANameAtASession(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()
	exitedSession(t, st, "aaaa2222")

	resp, err := svc.Bind(ctx, &serverv1.BindRequest{
		Name:    "work",
		Session: paths.FormatSessionID("aaaa2222"),
	})
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	want := &serverv1.BindResponse{SessionId: "aaaa2222"}
	if resp.SessionId != want.SessionId || resp.PreviousSessionId != want.PreviousSessionId {
		t.Errorf("Bind() = %+v, want %+v", resp, want)
	}

	// The name has to resolve afterwards, which is the whole point of binding it.
	got, err := mgr.Resolve(ctx, "work")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "aaaa2222" {
		t.Errorf("Resolve(work) = %q, want %q", got, "aaaa2222")
	}
}

// A name already in use is refused, and the message names the flag that would allow it. Silently moving
// it would send whatever window is watching that name to a different session.
func TestBindRefusesAnInUseNameWithoutMove(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()
	exitedSession(t, st, "aaaa2222")
	exitedSession(t, st, "bbbb3333")
	nameSession(t, st, "aaaa2222")

	_, err := svc.Bind(ctx, &serverv1.BindRequest{
		Name:    "aaaa2222",
		Session: paths.FormatSessionID("bbbb3333"),
	})
	if err == nil {
		t.Fatal("Bind() over an in-use name = nil error, want a refusal")
	}

	// And it must not have moved anyway.
	got, err := mgr.Resolve(ctx, "aaaa2222")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "aaaa2222" {
		t.Errorf("Resolve() = %q, want the name still on %q", got, "aaaa2222")
	}
}

func TestBindMovesANameWithMove(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()
	exitedSession(t, st, "aaaa2222")
	exitedSession(t, st, "bbbb3333")
	if err := st.Bind(ctx, store.Binding{
		Name: "work", SessionID: "aaaa2222", OnKill: store.KillTarget,
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	resp, err := svc.Bind(ctx, &serverv1.BindRequest{
		Name:    "work",
		Session: paths.FormatSessionID("bbbb3333"),
		Move:    true,
	})
	if err != nil {
		t.Fatalf("Bind(move) error = %v", err)
	}
	// Both halves reported: where it landed, and where it came from, since a caller that moved a name
	// has to be able to say what it moved off.
	if resp.SessionId != "bbbb3333" || resp.PreviousSessionId != "aaaa2222" {
		t.Errorf("Bind(move) = %+v, want session bbbb3333 moved from aaaa2222", resp)
	}
	got, err := mgr.Resolve(ctx, "work")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != "bbbb3333" {
		t.Errorf("Resolve(work) = %q, want %q", got, "bbbb3333")
	}
}

// Binding to a session that does not exist is refused, or a name would resolve to nothing and the next
// attach by it would quietly create a new session instead of finding the one meant.
func TestBindRefusesAMissingSession(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	_, err := svc.Bind(context.Background(), &serverv1.BindRequest{
		Name:    "work",
		Session: paths.FormatSessionID("nothere2"),
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Bind() to a missing session error = %v, want ErrNotFound", err)
	}
}

func TestUnbindLeavesTheSessionRunning(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()
	exitedSession(t, st, "aaaa2222")
	if err := st.Bind(ctx, store.Binding{
		Name: "work", SessionID: "aaaa2222", OnKill: store.KillTarget,
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	resp, err := svc.Unbind(ctx, &serverv1.UnbindRequest{Name: "work"})
	if err != nil {
		t.Fatalf("Unbind() error = %v", err)
	}
	if !resp.Removed || resp.SessionId != "aaaa2222" {
		t.Errorf("Unbind() = %+v, want removed with session aaaa2222", resp)
	}

	// The record survives: unbinding is about the name.
	if _, err := st.Get(ctx, "aaaa2222"); err != nil {
		t.Errorf("Get() after Unbind error = %v, want the session still recorded", err)
	}
	if _, err := mgr.Resolve(ctx, "work"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Resolve() after Unbind error = %v, want ErrNotFound", err)
	}
}

// Unbinding a name nothing uses is satisfied already, so it reports rather than fails: a caller tearing
// down should not have to check first.
func TestUnbindOfAnUnusedNameIsNotAnError(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	resp, err := svc.Unbind(context.Background(), &serverv1.UnbindRequest{Name: "nothing"})
	if err != nil {
		t.Fatalf("Unbind() error = %v", err)
	}
	if resp.Removed || resp.SessionId != "" {
		t.Errorf("Unbind() = %+v, want removed=false with no session", resp)
	}
}

// The case the whole policy exists for: a window that borrowed a session closes, its watcher runs
// `cm kill <name>`, and the session someone else is using has to survive it.
func TestKillByABorrowedNameReleasesItAndKeepsTheSession(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()
	exitedSession(t, st, "aaaa2222")
	if err := st.Bind(ctx, store.Binding{
		Name: "kitty.164", SessionID: "aaaa2222", OnKill: store.KillUnbind,
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	resp, err := svc.Kill(ctx, &serverv1.KillRequest{Sessions: []string{"kitty.164"}})
	if err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if len(resp.Killed) != 0 {
		t.Errorf("Kill() killed %v, want nothing killed", resp.Killed)
	}
	if resp.Unbound["kitty.164"] != "aaaa2222" {
		t.Errorf("Kill() unbound = %v, want kitty.164 released from aaaa2222", resp.Unbound)
	}
	if _, err := st.Get(ctx, "aaaa2222"); err != nil {
		t.Errorf("Get() after killing a borrowed name error = %v, want the session still there", err)
	}
	if _, err := st.Binding(ctx, "kitty.164"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Binding() error = %v, want the name gone", err)
	}
}

// A name created with its session owns it, so killing by that name kills, which is what `cm kill work`
// has always meant.
func TestKillByAnOwningNameKillsTheSession(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()
	exitedSession(t, st, "aaaa2222")
	if err := st.Bind(ctx, store.Binding{
		Name: "work", SessionID: "aaaa2222", OnKill: store.KillTarget,
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	resp, err := svc.Kill(ctx, &serverv1.KillRequest{Sessions: []string{"work"}})
	if err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if len(resp.Killed) != 1 || resp.Killed[0] != "work" {
		t.Errorf("Kill() killed %v, want [work]", resp.Killed)
	}
	if len(resp.Unbound) != 0 {
		t.Errorf("Kill() unbound = %v, want nothing released", resp.Unbound)
	}
	if _, err := st.Get(ctx, "aaaa2222"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() after the kill error = %v, want ErrNotFound", err)
	}
	// The names go with the session, since it is gone for good.
	if _, err := st.Binding(ctx, "work"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Binding() error = %v, want the name released with the session", err)
	}
}

// An ID reference kills whatever it names, which is what makes a session whose every name is a borrower
// reachable at all. Without this such a session could only be ended by unbinding every name first.
func TestKillByIDKillsEvenABorrowedSession(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()
	exitedSession(t, st, "aaaa2222")
	if err := st.Bind(ctx, store.Binding{
		Name: "kitty.164", SessionID: "aaaa2222", OnKill: store.KillUnbind,
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	ref := paths.FormatSessionID("aaaa2222")
	resp, err := svc.Kill(ctx, &serverv1.KillRequest{Sessions: []string{ref}})
	if err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if len(resp.Killed) != 1 || resp.Killed[0] != ref {
		t.Errorf("Kill() killed %v, want [%s]", resp.Killed, ref)
	}
	if _, err := st.Get(ctx, "aaaa2222"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() after the kill error = %v, want ErrNotFound", err)
	}
	if _, err := st.Binding(ctx, "kitty.164"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Binding() error = %v, want the borrowed name released with the session", err)
	}
}
