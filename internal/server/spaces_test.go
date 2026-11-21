package server

import (
	"context"
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/spaces"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// spaceTestSession records a named session in a space, without spawning a shim. Spaces live in the store,
// so nothing here needs a live session.
func spaceTestSession(t *testing.T, st *store.Store, name, space string, state store.State) {
	t.Helper()
	recordSession(t, st, store.Session{ID: name, State: state, Space: space})
}

func TestOpenOptionsSpace(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts OpenOptions
		want string
	}{
		{name: "nothing given", opts: OpenOptions{}, want: spaces.Default},
		{name: "inherited from the parent", opts: OpenOptions{InheritSpace: "cm"}, want: "cm"},
		{name: "asked for", opts: OpenOptions{Space: "infra"}, want: "infra"},
		// Asking is the caller saying where it goes, so the parent's does not override it.
		{name: "asked for beats the parent", opts: OpenOptions{Space: "infra", InheritSpace: "cm"}, want: "infra"},
	} {
		if got := tc.opts.space(); got != tc.want {
			t.Errorf("%s: space() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A revived session comes back in the space it was left in, rather than in the one the client reviving
// it happens to be inside: a window restored after a reboot is inside nothing.
func TestInheritForRestoreKeepsTheRecordsSpace(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	rec := store.Session{ID: "revived", Space: "cm"}

	got := mgr.inheritForRestore(OpenOptions{Ref: "revived", InheritSpace: "infra"}, rec)
	if s := got.space(); s != "cm" {
		t.Errorf("space() = %q, want the record's, cm", s)
	}
}

func TestInheritForRestoreLetsTheCallerMoveIt(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	rec := store.Session{ID: "revived", Space: "cm"}

	got := mgr.inheritForRestore(OpenOptions{Ref: "revived", Space: "infra"}, rec)
	if s := got.space(); s != "infra" {
		t.Errorf("space() = %q, want the caller's, infra", s)
	}
}

// The parent is resolved through the same layer as any reference, so a client inside a session it knows
// by name or by @id inherits either way, and one inside a session this server does not hold inherits
// nothing rather than failing its attach.
func TestParentSpace(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()
	spaceTestSession(t, st, "parent", "cm", store.StateRunning)

	for _, tc := range []struct {
		ref  string
		want string
	}{
		{ref: "parent", want: "cm"},
		{ref: "@parent", want: "cm"},
		{ref: "", want: ""},
		{ref: "elsewhere", want: ""},
	} {
		if got := svc.parentSpace(ctx, tc.ref); got != tc.want {
			t.Errorf("parentSpace(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}

func TestSpacesRPCCountsSessions(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()
	spaceTestSession(t, st, "a", "cm", store.StateRunning)
	spaceTestSession(t, st, "b", "cm", store.StateShelved)
	spaceTestSession(t, st, "c", "", store.StateRunning)
	if _, err := svc.CreateSpace(ctx, &serverv1.CreateSpaceRequest{Name: "empty"}); err != nil {
		t.Fatalf("CreateSpace() error = %v", err)
	}

	resp, err := svc.Spaces(ctx, &serverv1.SpacesRequest{})
	if err != nil {
		t.Fatalf("Spaces() error = %v", err)
	}
	// Creation times are the store's clock, so they are checked for presence and then left out.
	var got []*serverv1.Space
	for _, sp := range resp.Spaces {
		if sp.CreatedAtUnix == 0 {
			t.Errorf("space %s has no creation time", sp.Name)
		}
		got = append(got, &serverv1.Space{Name: sp.Name, Sessions: sp.Sessions, Running: sp.Running})
	}
	want := []*serverv1.Space{
		{Name: "cm", Sessions: 2, Running: 1},
		{Name: "default", Sessions: 1, Running: 1},
		{Name: "empty"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Spaces() = %v\nwant %v", got, want)
	}
}

func TestSpaceRPCsRejectHostileNames(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()
	spaceTestSession(t, st, "work", "cm", store.StateRunning)

	for _, name := range []string{"", "\x1b]2;pwned\x07", "a\nb", "host:cm"} {
		if _, err := svc.CreateSpace(ctx, &serverv1.CreateSpaceRequest{Name: name}); err == nil {
			t.Errorf("CreateSpace(%q) = nil error, want it refused", name)
		}
		if _, err := svc.Move(ctx, &serverv1.MoveRequest{Sessions: []string{"work"}, Space: name}); err == nil {
			t.Errorf("Move(%q) = nil error, want it refused", name)
		}
	}
	rec, err := st.Get(ctx, "work")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if rec.Space != "cm" {
		t.Errorf("Space = %q after every move was refused, want cm", rec.Space)
	}
}

// Moving reports where each session came from, and a session that cannot be found is reported without
// stopping the rest.
func TestMoveRPC(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()
	spaceTestSession(t, st, "a", "cm", store.StateRunning)
	// Ended sessions move too: a shelved one in the wrong space is the one hardest to find.
	spaceTestSession(t, st, "b", "", store.StateShelved)

	resp, err := svc.Move(ctx, &serverv1.MoveRequest{Sessions: []string{"a", "b", "missing"}, Space: "infra"})
	if err != nil {
		t.Fatalf("Move() error = %v", err)
	}
	if want := map[string]string{"a": "cm", "b": spaces.Default}; !reflect.DeepEqual(resp.Moved, want) {
		t.Errorf("Moved = %v, want %v", resp.Moved, want)
	}
	if _, ok := resp.Errors["missing"]; !ok || len(resp.Errors) != 1 {
		t.Errorf("Errors = %v, want one, for missing", resp.Errors)
	}
	for _, id := range []string{"a", "b"} {
		rec, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if rec.Space != "infra" {
			t.Errorf("%s: Space = %q, want infra", id, rec.Space)
		}
	}
}

func TestMoveRPCRequiresASession(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	if _, err := svc.Move(context.Background(), &serverv1.MoveRequest{Space: "infra"}); err == nil {
		t.Error("Move() with no session = nil error, want one")
	}
}

func TestRemoveSpaceRPC(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()
	spaceTestSession(t, st, "a", "cm", store.StateShelved)
	if _, err := svc.CreateSpace(ctx, &serverv1.CreateSpaceRequest{Name: "empty"}); err != nil {
		t.Fatalf("CreateSpace() error = %v", err)
	}

	for _, name := range []string{"cm", spaces.Default, "missing"} {
		if _, err := svc.RemoveSpace(ctx, &serverv1.RemoveSpaceRequest{Name: name}); err == nil {
			t.Errorf("RemoveSpace(%q) = nil error, want it refused", name)
		}
	}
	if _, err := svc.RemoveSpace(ctx, &serverv1.RemoveSpaceRequest{Name: "empty"}); err != nil {
		t.Errorf("RemoveSpace(empty) error = %v", err)
	}
	all, err := st.Spaces(ctx)
	if err != nil {
		t.Fatalf("Spaces() error = %v", err)
	}
	var names []string
	for _, sp := range all {
		names = append(names, sp.Name)
	}
	if want := []string{"cm", spaces.Default}; !reflect.DeepEqual(names, want) {
		t.Errorf("spaces = %q, want %q", names, want)
	}
}

func TestListFiltersBySpaceAndReportsIt(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()
	spaceTestSession(t, st, "a", "cm", store.StateRunning)
	spaceTestSession(t, st, "b", "", store.StateRunning)

	all, err := svc.List(ctx, &serverv1.ListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	got := map[string]string{}
	for _, sess := range all.Sessions {
		got[sess.Name] = sess.Space
	}
	if want := map[string]string{"a": "cm", "b": spaces.Default}; !reflect.DeepEqual(got, want) {
		t.Errorf("List() spaces = %v, want %v", got, want)
	}

	only, err := svc.List(ctx, &serverv1.ListRequest{Space: "cm"})
	if err != nil {
		t.Fatalf("List(space=cm) error = %v", err)
	}
	var names []string
	for _, sess := range only.Sessions {
		names = append(names, sess.Name)
	}
	if want := []string{"a"}; !reflect.DeepEqual(names, want) {
		t.Errorf("List(space=cm) = %q, want %q", names, want)
	}
}
