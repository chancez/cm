package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRemoteBindingRoundTrips(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.BindRemote(ctx, RemoteBinding{Name: "kitty.1234", Host: "white", Ref: "work"}); err != nil {
		t.Fatalf("BindRemote() error = %v", err)
	}
	got, err := s.RemoteBindingFor(ctx, "kitty.1234")
	if err != nil {
		t.Fatalf("RemoteBindingFor() error = %v", err)
	}
	want := RemoteBinding{Name: "kitty.1234", Host: "white", Ref: "work", CreatedAt: got.CreatedAt}
	if got.CreatedAt.IsZero() || !reflect.DeepEqual(got, want) {
		t.Errorf("RemoteBindingFor() = %+v, want %+v", got, want)
	}
	if all, err := s.RemoteBindings(ctx); err != nil || !reflect.DeepEqual(all, []RemoteBinding{want}) {
		t.Errorf("RemoteBindings() = %+v, %v; want [%+v]", all, err, want)
	}
	if got.Qualified() != "white:work" {
		t.Errorf("Qualified() = %q, want white:work", got.Qualified())
	}
}

// A name means one thing: pointing it at another host takes it off the session here, and binding it here
// takes it off the other host. Either left in place would let the two disagree about where a window goes.
func TestANameIsLocalOrRemoteNotBoth(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, sampleSession(idWork)); err != nil {
		t.Fatal(err)
	}
	if err := s.Bind(ctx, Binding{Name: "kitty.1234", SessionID: idWork, OnKill: KillTarget}); err != nil {
		t.Fatal(err)
	}

	if err := s.BindRemote(ctx, RemoteBinding{Name: "kitty.1234", Host: "white", Ref: "@abcd"}); err != nil {
		t.Fatalf("BindRemote() error = %v", err)
	}
	if _, err := s.Binding(ctx, "kitty.1234"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Binding() after BindRemote error = %v, want ErrNotFound", err)
	}

	if err := s.Bind(ctx, Binding{Name: "kitty.1234", SessionID: idWork}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoteBindingFor(ctx, "kitty.1234"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoteBindingFor() after Bind error = %v, want ErrNotFound", err)
	}
}

func TestUnbindRemote(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.BindRemote(ctx, RemoteBinding{Name: "n", Host: "white", Ref: "work"}); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.UnbindRemote(ctx, "n"); err != nil || !removed {
		t.Errorf("UnbindRemote() = %v, %v; want true", removed, err)
	}
	if removed, err := s.UnbindRemote(ctx, "n"); err != nil || removed {
		t.Errorf("UnbindRemote() again = %v, %v; want false", removed, err)
	}
}
