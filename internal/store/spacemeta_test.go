package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func mustSetMeta(t *testing.T, s *Store, space, key, value string) int64 {
	t.Helper()
	rev, err := s.SetSpaceMeta(context.Background(), space, key, []byte(value), nil)
	if err != nil {
		t.Fatalf("SetSpaceMeta(%s, %s) error = %v", space, key, err)
	}
	return rev
}

func TestSpaceMetaRoundTrips(t *testing.T) {
	s := openTestStore(t)
	rev := mustSetMeta(t, s, "cm", "kspaces/layout/main", "layout")

	got, err := s.GetSpaceMeta(context.Background(), "cm", "kspaces/layout/main")
	if err != nil {
		t.Fatalf("GetSpaceMeta() error = %v", err)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
	want := SpaceMeta{
		Space:     "cm",
		Key:       "kspaces/layout/main",
		Value:     []byte("layout"),
		Size:      6,
		Revision:  rev,
		UpdatedAt: got.UpdatedAt,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GetSpaceMeta() = %+v\nwant %+v", got, want)
	}
}

// Writing metadata to a space nothing is in yet creates the space, as moving a session into one does.
func TestSettingMetaCreatesTheSpace(t *testing.T) {
	s := openTestStore(t)
	mustSetMeta(t, s, "infra", "note", "x")
	if got, want := spaceNames(t, s), []string{"default", "infra"}; !reflect.DeepEqual(got, want) {
		t.Errorf("spaces = %q, want %q", got, want)
	}
}

func TestEmptyMetaValueIsAValue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.SetSpaceMeta(ctx, "cm", "empty", nil, nil); err != nil {
		t.Fatalf("SetSpaceMeta(nil) error = %v", err)
	}
	got, err := s.GetSpaceMeta(ctx, "cm", "empty")
	if err != nil {
		t.Fatalf("GetSpaceMeta() error = %v", err)
	}
	// Nil and empty are one value here, as they are on the wire.
	if len(got.Value) != 0 || got.Size != 0 {
		t.Errorf("GetSpaceMeta() = %+v, want an empty value", got)
	}
}

func TestMetaOfAMissingSpaceOrKey(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.GetSpaceMeta(ctx, "nope", "k"); !errors.Is(err, ErrSpaceNotFound) {
		t.Errorf("GetSpaceMeta(missing space) error = %v, want ErrSpaceNotFound", err)
	}
	if _, err := s.SpaceMetas(ctx, "nope", ""); !errors.Is(err, ErrSpaceNotFound) {
		t.Errorf("SpaceMetas(missing space) error = %v, want ErrSpaceNotFound", err)
	}
	if _, err := s.DeleteSpaceMeta(ctx, "nope", "k", nil); !errors.Is(err, ErrSpaceNotFound) {
		t.Errorf("DeleteSpaceMeta(missing space) error = %v, want ErrSpaceNotFound", err)
	}
	if _, err := s.GetSpaceMeta(ctx, "default", "k"); !errors.Is(err, ErrMetaNotFound) {
		t.Errorf("GetSpaceMeta(missing key) error = %v, want ErrMetaNotFound", err)
	}
}

func TestSetMetaIfRevision(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// 0 is "must not exist", so creating twice conditionally lets only the first through.
	first, err := s.SetSpaceMeta(ctx, "cm", "k", []byte("a"), new(int64(0)))
	if err != nil {
		t.Fatalf("SetSpaceMeta(if 0) error = %v", err)
	}
	_, err = s.SetSpaceMeta(ctx, "cm", "k", []byte("b"), new(int64(0)))
	if got, want := err, error(&RevisionMismatchError{Current: first}); !reflect.DeepEqual(got, want) {
		t.Errorf("SetSpaceMeta(if 0) over an existing key error = %v, want %v", got, want)
	}

	second, err := s.SetSpaceMeta(ctx, "cm", "k", []byte("c"), new(first))
	if err != nil {
		t.Fatalf("SetSpaceMeta(if %d) error = %v", first, err)
	}
	// The stale writer, still holding first, is refused and told where the key is.
	_, err = s.SetSpaceMeta(ctx, "cm", "k", []byte("d"), new(first))
	if got, want := err, error(&RevisionMismatchError{Current: second}); !reflect.DeepEqual(got, want) {
		t.Errorf("SetSpaceMeta(stale) error = %v, want %v", got, want)
	}
	got, err := s.GetSpaceMeta(ctx, "cm", "k")
	if err != nil {
		t.Fatalf("GetSpaceMeta() error = %v", err)
	}
	if string(got.Value) != "c" || got.Revision != second {
		t.Errorf("GetSpaceMeta() = %q at %d, want %q at %d", got.Value, got.Revision, "c", second)
	}
}

// A key removed and written again must not come back at a revision a stale writer could hold, or that
// writer's conditional save passes and overwrites a layout it never saw.
func TestMetaRevisionIsNeverReused(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	stale := mustSetMeta(t, s, "cm", "k", "a")
	if _, err := s.DeleteSpaceMeta(ctx, "cm", "k", nil); err != nil {
		t.Fatalf("DeleteSpaceMeta() error = %v", err)
	}
	again := mustSetMeta(t, s, "cm", "k", "b")
	if again == stale {
		t.Fatalf("revision %d reused after a delete", again)
	}
	_, err := s.SetSpaceMeta(ctx, "cm", "k", []byte("c"), new(stale))
	if got, want := err, error(&RevisionMismatchError{Current: again}); !reflect.DeepEqual(got, want) {
		t.Errorf("SetSpaceMeta(stale) error = %v, want %v", got, want)
	}
}

func TestDeleteSpaceMeta(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	rev := mustSetMeta(t, s, "cm", "k", "a")

	_, err := s.DeleteSpaceMeta(ctx, "cm", "k", new(rev+1))
	if got, want := err, error(&RevisionMismatchError{Current: rev}); !reflect.DeepEqual(got, want) {
		t.Errorf("DeleteSpaceMeta(wrong revision) error = %v, want %v", got, want)
	}
	if removed, err := s.DeleteSpaceMeta(ctx, "cm", "k", new(rev)); err != nil || !removed {
		t.Errorf("DeleteSpaceMeta() = %v, %v; want true, nil", removed, err)
	}
	// Gone already: satisfied, unless it was expected at a revision.
	if removed, err := s.DeleteSpaceMeta(ctx, "cm", "k", nil); err != nil || removed {
		t.Errorf("DeleteSpaceMeta(gone) = %v, %v; want false, nil", removed, err)
	}
	_, err = s.DeleteSpaceMeta(ctx, "cm", "k", new(rev))
	if got, want := err, error(&RevisionMismatchError{Current: 0}); !reflect.DeepEqual(got, want) {
		t.Errorf("DeleteSpaceMeta(gone, if %d) error = %v, want %v", rev, got, want)
	}
}

func TestSpaceMetasByPrefix(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a := mustSetMeta(t, s, "cm", "kspaces/layout/a", "aa")
	b := mustSetMeta(t, s, "cm", "kspaces/layout/b", "bbb")
	mustSetMeta(t, s, "cm", "kspaces_layout", "x")
	mustSetMeta(t, s, "cm", "other", "x")
	mustSetMeta(t, s, "infra", "kspaces/layout/a", "x")

	got, err := s.SpaceMetas(ctx, "cm", "kspaces/layout/")
	if err != nil {
		t.Fatalf("SpaceMetas() error = %v", err)
	}
	for i := range got {
		got[i].UpdatedAt = got[i].UpdatedAt.Truncate(0)
	}
	want := []SpaceMeta{
		{Space: "cm", Key: "kspaces/layout/a", Size: 2, Revision: a},
		{Space: "cm", Key: "kspaces/layout/b", Size: 3, Revision: b},
	}
	for i := range want {
		if i < len(got) {
			want[i].UpdatedAt = got[i].UpdatedAt
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SpaceMetas() = %+v\nwant %+v", got, want)
	}

	// '_' is literal, where LIKE would match it against '/'.
	got, err = s.SpaceMetas(ctx, "cm", "kspaces_")
	if err != nil {
		t.Fatalf("SpaceMetas() error = %v", err)
	}
	if len(got) != 1 || got[0].Key != "kspaces_layout" {
		t.Errorf("SpaceMetas(kspaces_) = %+v, want only kspaces_layout", got)
	}
}

// Removing a space takes its metadata with it, so a space created again later does not inherit a layout
// of windows that belonged to the old one.
func TestRemovingASpaceRemovesItsMeta(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustSetMeta(t, s, "infra", "k", "x")
	if err := s.DeleteSpace(ctx, "infra"); err != nil {
		t.Fatalf("DeleteSpace() error = %v", err)
	}
	if err := s.CreateSpace(ctx, "infra"); err != nil {
		t.Fatalf("CreateSpace() error = %v", err)
	}
	got, err := s.SpaceMetas(ctx, "infra", "")
	if err != nil {
		t.Fatalf("SpaceMetas() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("SpaceMetas() = %+v, want none", got)
	}
}
