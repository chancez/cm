package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// Session IDs as the generator produces them. Spelled out rather than passing a word like "work",
// which would read as a name and invite exactly the confusion this model exists to remove.
const (
	idWork   = "a7k2m9x4"
	idReview = "qr3hjv8t"
)

// sampleBinding returns a fully populated binding, so round-trip tests compare whole values.
func sampleBinding(name, sessionID string) Binding {
	return Binding{
		Name:      name,
		SessionID: sessionID,
		OnKill:    KillUnbind,
		CreatedAt: time.UnixMilli(1_700_000_000_000),
	}
}

func TestBindAndGetRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	want := sampleBinding("kitty.164", idWork)
	if err := s.Bind(ctx, want); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	got, err := s.Binding(ctx, "kitty.164")
	if err != nil {
		t.Fatalf("Binding() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Binding() = %+v\nwant %+v", got, want)
	}
}

// An unset action must read back as unbind, since that is the safe half: killing a window that
// borrowed a session has to leave the session running.
func TestBindDefaultsOnKillToUnbind(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Bind(ctx, Binding{Name: "kitty.164", SessionID: idWork}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	got, err := s.Binding(ctx, "kitty.164")
	if err != nil {
		t.Fatalf("Binding() error = %v", err)
	}
	want := Binding{
		Name:      "kitty.164",
		SessionID: idWork,
		OnKill:    KillUnbind,
		CreatedAt: got.CreatedAt,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Binding() = %+v\nwant %+v", got, want)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero, want it set by Bind")
	}
}

// Switching the same window twice is ordinary, so the second bind has to land. A refused rebind would
// leave the window showing one session while the binding named another.
func TestBindReplacesAnExistingBinding(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Bind(ctx, sampleBinding("kitty.164", idWork)); err != nil {
		t.Fatalf("first Bind() error = %v", err)
	}
	want := Binding{
		Name:      "kitty.164",
		SessionID: idReview,
		OnKill:    KillTarget,
		CreatedAt: time.UnixMilli(1_700_000_001_000),
	}
	if err := s.Bind(ctx, want); err != nil {
		t.Fatalf("second Bind() error = %v", err)
	}

	got, err := s.Binding(ctx, "kitty.164")
	if err != nil {
		t.Fatalf("Binding() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Binding() = %+v\nwant %+v", got, want)
	}

	all, err := s.Bindings(ctx)
	if err != nil {
		t.Fatalf("Bindings() error = %v", err)
	}
	if !reflect.DeepEqual(all, []Binding{want}) {
		t.Errorf("Bindings() = %+v\nwant %+v", all, []Binding{want})
	}
}

func TestBindingMissingReturnsNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Binding(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Binding() error = %v, want ErrNotFound", err)
	}
}

func TestBindingsOrderedOldestFirst(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	second := sampleBinding("kitty.164", idWork)
	second.CreatedAt = time.UnixMilli(1_700_000_002_000)
	first := sampleBinding("kitty.9", idReview)
	first.CreatedAt = time.UnixMilli(1_700_000_001_000)

	// Written newest first, so the order below comes from the query rather than from insertion.
	for _, b := range []Binding{second, first} {
		if err := s.Bind(ctx, b); err != nil {
			t.Fatalf("Bind(%s) error = %v", b.Name, err)
		}
	}

	got, err := s.Bindings(ctx)
	if err != nil {
		t.Fatalf("Bindings() error = %v", err)
	}
	want := []Binding{first, second}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Bindings() = %+v\nwant %+v", got, want)
	}
}

func TestUnbindReportsWhetherABindingWasThere(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Bind(ctx, sampleBinding("kitty.164", idWork)); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	removed, err := s.Unbind(ctx, "kitty.164")
	if err != nil || !removed {
		t.Errorf("Unbind() = %v, %v, want true, nil", removed, err)
	}
	removed, err = s.Unbind(ctx, "kitty.164")
	if err != nil || removed {
		t.Errorf("second Unbind() = %v, %v, want false, nil", removed, err)
	}

	got, err := s.Bindings(ctx)
	if err != nil {
		t.Fatalf("Bindings() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Bindings() = %+v, want none", got)
	}
}

// Removing a session for good takes its names with it, or the next attach by one of them would resolve
// to an ID nothing holds.
func TestUnbindSessionReleasesEveryBindingToIt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// kitty.9 first, so insertion order and sorted order differ. Written the other way round the
	// assertion below passes whether or not UnbindSession sorts at all.
	for _, b := range []Binding{
		sampleBinding("kitty.9", idWork),
		sampleBinding("kitty.164", idWork),
		sampleBinding("kitty.12", idReview),
	} {
		if err := s.Bind(ctx, b); err != nil {
			t.Fatalf("Bind(%s) error = %v", b.Name, err)
		}
	}

	released, err := s.UnbindSession(ctx, idWork)
	if err != nil {
		t.Fatalf("UnbindSession() error = %v", err)
	}
	// Sorted by name, since the caller only reports these and the order sqlite returns deletions in
	// is not something to depend on.
	want := []string{"kitty.164", "kitty.9"}
	if !reflect.DeepEqual(released, want) {
		t.Errorf("UnbindSession() = %v, want %v", released, want)
	}

	got, err := s.Bindings(ctx)
	if err != nil {
		t.Fatalf("Bindings() error = %v", err)
	}
	remaining := []Binding{sampleBinding("kitty.12", idReview)}
	if !reflect.DeepEqual(got, remaining) {
		t.Errorf("Bindings() = %+v\nwant %+v", got, remaining)
	}
}

func TestUnbindSessionWithNoBindingsReportsNone(t *testing.T) {
	s := openTestStore(t)
	released, err := s.UnbindSession(context.Background(), idWork)
	if err != nil || released != nil {
		t.Errorf("UnbindSession() = %v, %v, want nil, nil", released, err)
	}
}

// The names of one session, which is what a listing needs in order to show a session by the name a
// person recognizes rather than by its ID.
func TestBindingsForOneSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mine := sampleBinding("kitty.164", idWork)
	also := sampleBinding("work", idWork)
	also.CreatedAt = time.UnixMilli(1_700_000_003_000)
	other := sampleBinding("kitty.12", idReview)
	for _, b := range []Binding{mine, also, other} {
		if err := s.Bind(ctx, b); err != nil {
			t.Fatalf("Bind(%s) error = %v", b.Name, err)
		}
	}

	got, err := s.BindingsFor(ctx, idWork)
	if err != nil {
		t.Fatalf("BindingsFor() error = %v", err)
	}
	want := []Binding{mine, also}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BindingsFor() = %+v\nwant %+v", got, want)
	}
}

func TestBindingsForASessionWithNoNames(t *testing.T) {
	s := openTestStore(t)
	got, err := s.BindingsFor(context.Background(), idWork)
	if err != nil || got != nil {
		t.Errorf("BindingsFor() = %+v, %v, want nil, nil", got, err)
	}
}
