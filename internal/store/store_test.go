package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "cm.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// sampleSession returns a fully populated record, so round-trip tests compare whole values
// rather than a handful of fields.
func sampleSession(name string) Session {
	return Session{
		Name:       name,
		ShimSocket: "/run/cm/shim-" + name + ".sock",
		LogPath:    "/state/cm/logs/" + name + ".log",
		ShimPID:    4242,
		ShellPID:   4243,
		LastSeq:    9000,
		State:      StateRunning,
		ExitCode:   0,
		Command:    "/bin/zsh",
		Cwd:        "/home/user/projects",
		Title:      "zsh",
		Rows:       40,
		Cols:       120,
		Owned:      true,
		CreatedAt:  time.UnixMilli(1_700_000_000_000),
	}
}

func TestCreateAndGetRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	want := sampleSession("work")
	if err := s.Create(ctx, want); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := s.Get(ctx, "work")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	// UpdatedAt is assigned by Create, so it cannot be predicted; copy it across and
	// compare everything else as a whole value.
	want.UpdatedAt = got.UpdatedAt
	if got != want {
		t.Errorf("Get() = %+v\nwant %+v", got, want)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero, want it set by Create")
	}
}

func TestGetMissingReturnsNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.Get(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

// A duplicate name must be refused, or two shims could believe they own one session.
func TestCreateRejectsDuplicateName(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, sampleSession("dup")); err != nil {
		t.Fatalf("first Create() error = %v", err)
	}
	if err := s.Create(ctx, sampleSession("dup")); err == nil {
		t.Error("second Create() = nil error, want rejection")
	}
}

func TestApplyUpdatesOnlyGivenFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	orig := sampleSession("upd")
	if err := s.Create(ctx, orig); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	newSeq := uint64(12345)
	newState := StateExited
	code := 3
	if err := s.Apply(ctx, "upd", Update{
		LastSeq:  &newSeq,
		State:    &newState,
		ExitCode: &code,
	}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	got, err := s.Get(ctx, "upd")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}

	want := orig
	want.LastSeq = newSeq
	want.State = newState
	want.ExitCode = code
	want.UpdatedAt = got.UpdatedAt
	if got != want {
		t.Errorf("after Apply, Get() = %+v\nwant %+v", got, want)
	}
}

func TestApplyMissingReturnsNotFound(t *testing.T) {
	s := openTestStore(t)
	seq := uint64(1)
	err := s.Apply(context.Background(), "ghost", Update{LastSeq: &seq})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Apply() error = %v, want ErrNotFound", err)
	}
}

func TestApplyEmptyUpdateIsNoop(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.Create(ctx, sampleSession("noop")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// An empty update must not error even though there is nothing to write, so callers can
	// build updates conditionally without checking whether any field was set.
	if err := s.Apply(ctx, "noop", Update{}); err != nil {
		t.Errorf("Apply(empty) error = %v, want nil", err)
	}
}

func TestListOrdersByCreation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i, name := range []string{"third", "first", "second"} {
		sess := sampleSession(name)
		// Distinct creation times, deliberately not in name order.
		sess.CreatedAt = time.UnixMilli(int64(1_700_000_000_000 + (2-i)*1000))
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%s) error = %v", name, err)
		}
	}

	got, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	var names []string
	for _, sess := range got {
		names = append(names, sess.Name)
	}
	want := []string{"second", "first", "third"}
	if len(names) != len(want) {
		t.Fatalf("List() returned %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("List() = %v, want %v", names, want)
			break
		}
	}
}

func TestListFiltersByPrefix(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"kitty.1", "kitty.2", "work"} {
		if err := s.Create(ctx, sampleSession(name)); err != nil {
			t.Fatalf("Create(%s) error = %v", name, err)
		}
	}

	got, err := s.List(ctx, "kitty.")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List(kitty.) returned %d sessions, want 2", len(got))
	}
}

// A prefix containing LIKE wildcards must match literally, or "100%" would match anything.
func TestListEscapesLikeWildcards(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"a_b", "axb", "a-b"} {
		if err := s.Create(ctx, sampleSession(name)); err != nil {
			t.Fatalf("Create(%s) error = %v", name, err)
		}
	}

	got, err := s.List(ctx, "a_")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 || got[0].Name != "a_b" {
		var names []string
		for _, sess := range got {
			names = append(names, sess.Name)
		}
		t.Errorf("List(\"a_\") = %v, want exactly [a_b]: '_' must not act as a wildcard", names)
	}
}

func TestDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.Create(ctx, sampleSession("gone")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := s.Delete(ctx, "gone"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := s.Get(ctx, "gone"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() after Delete error = %v, want ErrNotFound", err)
	}
	// Deleting twice is not an error: the caller wanted it gone, and it is.
	if err := s.Delete(ctx, "gone"); err != nil {
		t.Errorf("second Delete() error = %v, want nil", err)
	}
}

// Names must never be reused, including after deletion, or a client holding an old name
// could reattach to an unrelated session.
func TestNextNameNeverReuses(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first, err := s.NextName(ctx, "kitty.")
	if err != nil {
		t.Fatalf("NextName() error = %v", err)
	}
	if first != "kitty.1" {
		t.Errorf("first NextName() = %q, want %q", first, "kitty.1")
	}

	if err := s.Create(ctx, sampleSession(first)); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := s.Delete(ctx, first); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	second, err := s.NextName(ctx, "kitty.")
	if err != nil {
		t.Fatalf("second NextName() error = %v", err)
	}
	if second == first {
		t.Errorf("NextName() reused %q after deletion", second)
	}
	if second != "kitty.2" {
		t.Errorf("second NextName() = %q, want %q", second, "kitty.2")
	}
}

func TestNextNameIsUniqueUnderConcurrency(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	const n = 50
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		names = make(map[string]int)
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, err := s.NextName(ctx, "s")
			if err != nil {
				t.Errorf("NextName() error = %v", err)
				return
			}
			mu.Lock()
			names[name]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(names) != n {
		t.Errorf("got %d distinct names from %d calls, want %d; duplicates: %v",
			len(names), n, n, duplicates(names))
	}
}

func duplicates(counts map[string]int) []string {
	var out []string
	for name, c := range counts {
		if c > 1 {
			out = append(out, name)
		}
	}
	return out
}

// Separate prefixes must not share a counter, so implicit and named sessions number
// independently.
func TestNextNameCountersAreIndependent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	a, err := s.NextName(ctx, "kitty.")
	if err != nil {
		t.Fatalf("NextName() error = %v", err)
	}
	b, err := s.NextName(ctx, "tmp.")
	if err != nil {
		t.Fatalf("NextName() error = %v", err)
	}
	if a != "kitty.1" || b != "tmp.1" {
		t.Errorf("got (%q, %q), want (kitty.1, tmp.1)", a, b)
	}
}

// Reopening must preserve records and not re-run migrations destructively, which is the
// whole point of persisting to disk.
func TestReopenPreservesSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	ctx := context.Background()

	s1, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	want := sampleSession("persist")
	if err := s1.Create(ctx, want); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	// Allocate a name so the counter's persistence is covered too.
	if _, err := s1.NextName(ctx, "kitty."); err != nil {
		t.Fatalf("NextName() error = %v", err)
	}
	s1.Close()

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer s2.Close()

	got, err := s2.Get(ctx, "persist")
	if err != nil {
		t.Fatalf("Get() after reopen error = %v", err)
	}
	want.UpdatedAt = got.UpdatedAt
	if got != want {
		t.Errorf("after reopen Get() = %+v\nwant %+v", got, want)
	}

	next, err := s2.NextName(ctx, "kitty.")
	if err != nil {
		t.Fatalf("NextName() after reopen error = %v", err)
	}
	if next != "kitty.2" {
		t.Errorf("NextName() after reopen = %q, want %q: the counter must survive restart",
			next, "kitty.2")
	}
}

// Migrating an already-current database must be a no-op, so a server restart is cheap and
// cannot corrupt state.
func TestMigrateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	ctx := context.Background()

	for i := range 3 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("Open() attempt %d error = %v", i+1, err)
		}
		var version int
		if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			t.Fatalf("reading user_version: %v", err)
		}
		if version != len(migrations) {
			t.Errorf("user_version = %d, want %d", version, len(migrations))
		}
		s.Close()
	}
}
