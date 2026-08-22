package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestGenerateIDShapeAndAlphabet(t *testing.T) {
	for range 200 {
		id, err := generateID()
		if err != nil {
			t.Fatalf("generateID() error = %v", err)
		}
		if len(id) != idLen {
			t.Fatalf("generateID() = %q, want %d characters", id, idLen)
		}
		for _, r := range id {
			if !strings.ContainsRune(idAlphabet, r) {
				t.Fatalf("generateID() = %q, which contains %q, outside the alphabet", id, r)
			}
		}
		// The glyphs an ID is read off a screen and typed back must never include the pairs that get
		// misread, and 'i' in particular is what keeps a generated ID from colliding with the "mig"
		// prefix the migration backfilled.
		if strings.ContainsAny(id, "01ilou") {
			t.Fatalf("generateID() = %q, which contains an ambiguous character", id)
		}
	}
}

// Not a uniqueness proof, which no test can give: this catches a generator that has stopped drawing
// randomness at all, which is what a mistake here looks like.
func TestGenerateIDDoesNotRepeatItself(t *testing.T) {
	seen := map[string]struct{}{}
	for range 500 {
		id, err := generateID()
		if err != nil {
			t.Fatalf("generateID() error = %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("generateID() returned %q twice in 500 draws", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewIDReturnsSomethingFree(t *testing.T) {
	s := openTestStore(t)
	id, err := s.NewID(context.Background())
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	if len(id) != idLen {
		t.Errorf("NewID() = %q, want %d characters", id, idLen)
	}
}

// A collision must be drawn again rather than returned, since the caller would then fail at Create with
// a bare UNIQUE constraint error naming nothing useful.
func TestNewIDSkipsAnIDAlreadyInUse(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, sampleSession("taken222")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	draws := []string{"taken222", "taken222", "free2222"}
	s.IDSource = func() (string, error) {
		next := draws[0]
		if len(draws) > 1 {
			draws = draws[1:]
		}
		return next, nil
	}

	got, err := s.NewID(ctx)
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	if got != "free2222" {
		t.Errorf("NewID() = %q, want %q", got, "free2222")
	}
}

// A generator that only ever returns a taken ID has to give up and say so. Looping until it works would
// present a broken IDSource as cm hanging on session creation, with nothing naming the cause.
func TestNewIDGivesUpRatherThanSpinning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, sampleSession("taken222")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	s.IDSource = func() (string, error) { return "taken222", nil }

	if _, err := s.NewID(ctx); err == nil {
		t.Error("NewID() = nil error, want it to give up")
	}
}

func TestNewIDReportsAGeneratorFailure(t *testing.T) {
	s := openTestStore(t)
	want := errors.New("no entropy")
	s.IDSource = func() (string, error) { return "", want }

	if _, err := s.NewID(context.Background()); !errors.Is(err, want) {
		t.Errorf("NewID() error = %v, want %v", err, want)
	}
}
