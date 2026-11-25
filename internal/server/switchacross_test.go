package server

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// acrossTestService is closeTestService with its clients reporting caps, and the session named name.
func acrossTestService(t *testing.T, name string, caps capability.Set) (*Service, *Session) {
	t.Helper()
	svc, sess := closeTestService(t, name, 0)
	tok := sess.reserveClient()
	sess.noteClientIdentity(tok, "v1", 1000, caps)
	if _, err := sess.attach(nil, tok); err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	return svc, sess
}

func rebindAcross(svc *Service, name string) (*serverv1.SwitchResponse, error) {
	return svc.Switch(context.Background(), &serverv1.SwitchRequest{
		Session:        name,
		Target:         "white:work",
		RemoteTargetId: "abcd2345",
		Bind:           true,
		AllClients:     true,
	})
}

// A window's name points at the session on the other host, and the window is told to go there by ID.
func TestRebindAcrossPointsTheName(t *testing.T) {
	svc, _ := acrossTestService(t, "kitty.1", capability.Client())
	ctx := context.Background()

	resp, err := rebindAcross(svc, "kitty.1")
	if err != nil {
		t.Fatalf("Switch() error = %v", err)
	}
	want := &serverv1.SwitchResponse{
		Asked:      1,
		TargetId:   "abcd2345",
		SwitchedTo: "white:@abcd2345",
		BoundName:  "kitty.1",
	}
	if !reflect.DeepEqual(resp.String(), want.String()) {
		t.Errorf("Switch() = %v, want %v", resp, want)
	}
	rb, err := svc.mgr.store.RemoteBindingFor(ctx, "kitty.1")
	if err != nil {
		t.Fatalf("RemoteBindingFor() error = %v", err)
	}
	// What was typed, so a restore after the remote session ended recreates it by name.
	if rb.Qualified() != "white:work" {
		t.Errorf("pointer = %s, want white:work", rb.Qualified())
	}

	// And nothing here answers to the name any more, which an old client would otherwise recreate.
	var remoteName *RemoteNameError
	if _, err := svc.mgr.Resolve(ctx, "kitty.1"); !errors.As(err, &remoteName) {
		t.Errorf("Resolve() error = %v, want a RemoteNameError", err)
	}
	if _, _, err := svc.mgr.Open(ctx, OpenOptions{Ref: "kitty.1"}); !errors.As(err, &remoteName) {
		t.Errorf("Open() error = %v, want a RemoteNameError rather than a new session", err)
	}
}

// A window whose client cannot follow is not asked, and nothing moves: it would try white:@id here, fail,
// and close.
func TestRebindAcrossRefusesAnOldClient(t *testing.T) {
	svc, _ := acrossTestService(t, "kitty.2", capability.Set{})
	if _, err := rebindAcross(svc, "kitty.2"); err == nil || !strings.Contains(err.Error(), "cm clients upgrade") {
		t.Fatalf("Switch() error = %v, want a refusal naming the upgrade", err)
	}
	if _, err := svc.mgr.store.RemoteBindingFor(context.Background(), "kitty.2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a pointer was written for a refused rebind: %v", err)
	}
}

// Closing or killing by a pointer lets go of it and touches nothing else, as a borrowed name does.
func TestClosingAPointerUnbindsIt(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()
	for _, n := range []string{"closed", "killed"} {
		if err := st.BindRemote(ctx, store.RemoteBinding{Name: n, Host: "white", Ref: "work"}); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := svc.Close(ctx, &serverv1.CloseRequest{Session: "closed", ClientPid: 1000})
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if resp.Outcome != serverv1.CloseOutcome_CLOSE_OUTCOME_UNBOUND {
		t.Errorf("Close() = %v, want unbound", resp.Outcome)
	}
	kill, err := svc.Kill(ctx, &serverv1.KillRequest{Sessions: []string{"killed"}})
	if err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if want := map[string]string{"killed": "white:work"}; !reflect.DeepEqual(kill.Unbound, want) {
		t.Errorf("Kill() unbound = %v, want %v", kill.Unbound, want)
	}
	if left, err := st.RemoteBindings(ctx); err != nil || len(left) != 0 {
		t.Errorf("RemoteBindings() = %v, %v; want none left", left, err)
	}
}

// Taking a pointer back to a session here is a move, as taking any held name is.
func TestBindingOverAPointerNeedsMove(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	recordSession(t, st, store.Session{ID: "local1", State: store.StateRunning})
	if err := st.BindRemote(ctx, store.RemoteBinding{Name: "n", Host: "white", Ref: "work"}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Bind(ctx, "n", "local1", store.KillTarget, false); err == nil || !strings.Contains(err.Error(), "white:work") {
		t.Errorf("Bind() without move error = %v, want a refusal naming white:work", err)
	}
	if err := mgr.Bind(ctx, "n", "local1", store.KillTarget, true); err != nil {
		t.Fatalf("Bind() with move error = %v", err)
	}
	if _, err := st.RemoteBindingFor(ctx, "n"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the pointer survived a move: %v", err)
	}
}
