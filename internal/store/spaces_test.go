package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/spaces"
)

// spaceNames returns the names of every space, which is the part of Spaces a test can predict.
func spaceNames(t *testing.T, s *Store) []string {
	t.Helper()
	all, err := s.Spaces(context.Background())
	if err != nil {
		t.Fatalf("Spaces() error = %v", err)
	}
	var out []string
	for _, sp := range all {
		out = append(out, sp.Name)
	}
	return out
}

func TestDefaultSpaceExistsFromTheStart(t *testing.T) {
	s := openTestStore(t)
	if got, want := spaceNames(t, s), []string{spaces.Default}; !reflect.DeepEqual(got, want) {
		t.Errorf("spaces = %q, want %q", got, want)
	}
}

func TestCreateWithoutASpacePutsTheSessionInDefault(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	sess := sampleSession(idWork)
	sess.Space = ""
	if err := s.Create(ctx, sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got, err := s.Get(ctx, idWork)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := sess
	want.Space = spaces.Default
	want.UpdatedAt = got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get() = %+v\nwant %+v", got, want)
	}
}

// A session names its space, and the space has to exist for it to be found in: the store creates the
// record rather than leaving a session in a space Spaces never lists.
func TestCreatingASessionCreatesItsSpace(t *testing.T) {
	s := openTestStore(t)
	if err := s.Create(context.Background(), sampleSession(idWork)); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if got, want := spaceNames(t, s), []string{"cm", spaces.Default}; !reflect.DeepEqual(got, want) {
		t.Errorf("spaces = %q, want %q", got, want)
	}
}

func TestMovingASessionCreatesItsNewSpace(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, sampleSession(idWork)); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := s.Apply(ctx, idWork, Update{Space: new("infra")}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	got, err := s.Get(ctx, idWork)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Space != "infra" {
		t.Errorf("Space = %q, want infra", got.Space)
	}
	// The space it left stays: a space outlives its sessions.
	if got, want := spaceNames(t, s), []string{"cm", spaces.Default, "infra"}; !reflect.DeepEqual(got, want) {
		t.Errorf("spaces = %q, want %q", got, want)
	}
}

// A move of a session that does not exist must not leave the space it named behind, which a space
// created outside the update's transaction would.
func TestMovingAMissingSessionCreatesNoSpace(t *testing.T) {
	s := openTestStore(t)
	err := s.Apply(context.Background(), idWork, Update{Space: new("infra")})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Apply() error = %v, want ErrNotFound", err)
	}
	if got, want := spaceNames(t, s), []string{spaces.Default}; !reflect.DeepEqual(got, want) {
		t.Errorf("spaces = %q, want %q", got, want)
	}
}

func TestCreateSpaceIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.CreateSpace(ctx, "infra"); err != nil {
		t.Fatalf("CreateSpace() error = %v", err)
	}
	before, err := s.Spaces(ctx)
	if err != nil {
		t.Fatalf("Spaces() error = %v", err)
	}
	if err := s.CreateSpace(ctx, "infra"); err != nil {
		t.Fatalf("CreateSpace() again error = %v", err)
	}
	after, err := s.Spaces(ctx)
	if err != nil {
		t.Fatalf("Spaces() error = %v", err)
	}
	// Including its creation time, which a second create must not reset.
	if !reflect.DeepEqual(after, before) {
		t.Errorf("after a second CreateSpace = %+v, want %+v", after, before)
	}
}

func TestDeleteSpace(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.CreateSpace(ctx, "infra"); err != nil {
		t.Fatalf("CreateSpace() error = %v", err)
	}
	if err := s.DeleteSpace(ctx, "infra"); err != nil {
		t.Fatalf("DeleteSpace() error = %v", err)
	}
	if got, want := spaceNames(t, s), []string{spaces.Default}; !reflect.DeepEqual(got, want) {
		t.Errorf("spaces = %q, want %q", got, want)
	}
	if err := s.DeleteSpace(ctx, "infra"); !errors.Is(err, ErrSpaceNotFound) {
		t.Errorf("DeleteSpace() of a missing space error = %v, want ErrSpaceNotFound", err)
	}
}

func TestDeleteSpaceRefusesDefault(t *testing.T) {
	s := openTestStore(t)
	if err := s.DeleteSpace(context.Background(), spaces.Default); !errors.Is(err, ErrDefaultSpace) {
		t.Errorf("DeleteSpace(default) error = %v, want ErrDefaultSpace", err)
	}
}

// Ended sessions count. A shelved session is what a space is kept for, so removing its space would lose
// the place to find it.
func TestDeleteSpaceRefusesOneWithSessions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	sess := sampleSession(idWork)
	sess.State = StateShelved
	if err := s.Create(ctx, sess); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := s.DeleteSpace(ctx, "cm"); !errors.Is(err, ErrSpaceNotEmpty) {
		t.Errorf("DeleteSpace() error = %v, want ErrSpaceNotEmpty", err)
	}
	if got, want := spaceNames(t, s), []string{"cm", spaces.Default}; !reflect.DeepEqual(got, want) {
		t.Errorf("spaces = %q, want %q", got, want)
	}
}
