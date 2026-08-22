package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
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
//
// Takes an ID rather than a name: a record has no name, and the paths derive from the ID, which is what
// a session created after the flip gets.
func sampleSession(id string) Session {
	return Session{
		ID:         id,
		ShimSocket: "/run/cm/shim-" + id + ".sock",
		LogPath:    "/state/cm/logs/" + id + ".log",
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
		Env:        map[string]string{"KITTY_LISTEN_ON": "unix:/tmp/kitty-1", "TERM": "xterm-kitty"},
		Tags:       map[string]string{"project": "cm", "role": "reviewer", "review": ""},
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
	if !reflect.DeepEqual(got, want) {
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

// A duplicate ID must be refused, or two shims could believe they own one session. Unreachable in
// practice, since NewID checks, and worth pinning anyway: the primary key is what makes that check a
// convenience rather than the guarantee.
func TestCreateRejectsDuplicateID(t *testing.T) {
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
	if !reflect.DeepEqual(got, want) {
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

// Applying tags replaces the whole set, so a tag can be removed rather than only added.
func TestApplyReplacesTheWholeTagSet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	orig := sampleSession("tagged")
	if err := s.Create(ctx, orig); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// The new set drops "role" and "review" and changes "project", which a merge could not express.
	replacement := map[string]string{"project": "zmx"}
	if err := s.Apply(ctx, "tagged", Update{Tags: replacement}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	got, err := s.Get(ctx, "tagged")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := orig
	want.Tags = replacement
	want.UpdatedAt = got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after Apply, Get() = %+v\nwant %+v", got, want)
	}
}

// A nil map leaves tags alone while an empty one clears them.
//
// The distinction is what makes removing the last tag possible. Treating both as "no change" would
// leave a session permanently tagged once it had one tag, and treating both as "clear" would wipe
// the tags on every unrelated update, since the manager applies updates constantly for cwd and
// title.
func TestApplyDistinguishesNilTagsFromEmpty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	orig := sampleSession("keep")
	if err := s.Create(ctx, orig); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// A nil map, alongside another field, is the shape every ordinary update has.
	title := "something else"
	if err := s.Apply(ctx, "keep", Update{Title: &title, Tags: nil}); err != nil {
		t.Fatalf("Apply(nil tags) error = %v", err)
	}
	got, err := s.Get(ctx, "keep")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := orig
	want.Title = title
	want.UpdatedAt = got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after Apply with nil tags, Get() = %+v\nwant %+v", got, want)
	}

	// An empty but non-nil map clears them.
	if err := s.Apply(ctx, "keep", Update{Tags: map[string]string{}}); err != nil {
		t.Fatalf("Apply(empty tags) error = %v", err)
	}
	got, err = s.Get(ctx, "keep")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	// Read back as nil rather than an empty map: an empty set is stored as the empty string, which
	// is what a row written before this column existed also holds, so the two must decode alike.
	want.Tags = nil
	want.UpdatedAt = got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after Apply with empty tags, Get() = %+v\nwant %+v", got, want)
	}
}

// A session created with no tags reads back as nil, matching a row that predates the column.
func TestCreateWithoutTags(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	orig := sampleSession("untagged")
	orig.Tags = nil
	if err := s.Create(ctx, orig); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	got, err := s.Get(ctx, "untagged")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := orig
	want.UpdatedAt = got.UpdatedAt
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get() = %+v\nwant %+v", got, want)
	}
}

func TestListOrdersByCreation(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i, id := range []string{"third", "first", "second"} {
		sess := sampleSession(id)
		// Distinct creation times, deliberately not in ID order.
		sess.CreatedAt = time.UnixMilli(int64(1_700_000_000_000 + (2-i)*1000))
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%s) error = %v", id, err)
		}
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}

	var ids []string
	for _, sess := range got {
		ids = append(ids, sess.ID)
	}
	want := []string{"second", "first", "third"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("List() = %v, want %v", ids, want)
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

func duplicates(counts map[string]int) []string {
	var out []string
	for name, c := range counts {
		if c > 1 {
			out = append(out, name)
		}
	}
	return out
}

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
	// Bind a name too, so what survives a restart covers both tables. A session whose names were
	// lost on restart would come back unreachable by every name a terminal emulator has recorded.
	binding := Binding{
		Name:      "kitty.164",
		SessionID: want.ID,
		OnKill:    KillTarget,
		CreatedAt: time.UnixMilli(1_700_000_000_000),
	}
	if err := s1.Bind(ctx, binding); err != nil {
		t.Fatalf("Bind() error = %v", err)
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
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after reopen Get() = %+v\nwant %+v", got, want)
	}

	gotBinding, err := s2.Binding(ctx, "kitty.164")
	if err != nil {
		t.Fatalf("Binding() after reopen error = %v", err)
	}
	if !reflect.DeepEqual(gotBinding, binding) {
		t.Errorf("after reopen Binding() = %+v\nwant %+v", gotBinding, binding)
	}
}

// A database written by an older cm must survive the upgrade with its sessions readable.
//
// This is the case no other test reaches: every one of them starts from a fresh database, which
// applies the whole migration list at once and so exercises the end state rather than the path to
// it. A migration that fails on a real installation, or drops a column something still selects,
// presents as every existing session vanishing on upgrade while a fresh install looks perfect.
//
// Built by applying every migration but the last, which is what the previous release's binary
// would have left behind, then opening the store normally so the final one runs against it.
func TestUpgradeFromThePreviousSchemaKeepsSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	ctx := context.Background()

	// The previous schema, applied the way migrate does, including the version it would have
	// recorded so the real Open below picks up exactly where that binary left off.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	// Everything up to the migration that drops `owned`, since the row inserted below sets that
	// column and applying the drop first would insert into a column that no longer exists.
	//
	// Located by content rather than by counting back from the end, which is what this did and what
	// broke: appending any later migration moved the stop point past the drop, and the failure
	// surfaced as "table sessions has no column named owned" from the insert, which reads as the new
	// migration being broken rather than as this constant being stale.
	prior := migrations[:indexOfMigration(t, "DROP COLUMN owned")]
	for i, m := range prior {
		if _, err := old.ExecContext(ctx, m); err != nil {
			t.Fatalf("applying migration %d: %v", i+1, err)
		}
	}
	if _, err := old.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", len(prior))); err != nil {
		t.Fatalf("recording the prior schema version: %v", err)
	}
	// A row with the column this migration drops, since a database that never had one set would
	// let a broken migration pass.
	if _, err := old.ExecContext(ctx, `
		INSERT INTO sessions (name, shim_socket, log_path, state, owned, created_at, updated_at)
		VALUES ('old', '/run/cm/shim-old.sock', '', 'running', 1, 1700000000000, 1700000000000)`,
	); err != nil {
		t.Fatalf("inserting a row under the prior schema: %v", err)
	}
	old.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() on a database from the previous schema error = %v", err)
	}
	defer s.Close()

	// Readable by the ID the migration backfilled, which is "mig" plus the old rowid.
	got, err := s.Get(ctx, "mig00001")
	if err != nil {
		t.Fatalf("Get() after the upgrade error = %v", err)
	}
	want := Session{
		ID:         "mig00001",
		ShimSocket: "/run/cm/shim-old.sock",
		State:      StateRunning,
		CreatedAt:  time.UnixMilli(1_700_000_000_000),
		UpdatedAt:  time.UnixMilli(1_700_000_000_000),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after upgrading from the previous schema Get() = %+v\nwant %+v", got, want)
	}

	// And its name still resolves to it, owning it as it did before names became bindings. Without
	// this the sessions survive the upgrade while every name a terminal emulator recorded stops
	// finding them, which is the same outcome as losing them.
	gotBinding, err := s.Binding(ctx, "old")
	if err != nil {
		t.Fatalf("Binding() after the upgrade error = %v", err)
	}
	wantBinding := Binding{
		Name:      "old",
		SessionID: "mig00001",
		OnKill:    KillTarget,
		CreatedAt: time.UnixMilli(1_700_000_000_000),
	}
	if !reflect.DeepEqual(gotBinding, wantBinding) {
		t.Errorf("after the upgrade Binding() = %+v\nwant %+v", gotBinding, wantBinding)
	}

	// The shim socket is untouched, and that is the property the rebuild was designed around: paths
	// are recorded rather than derived, so a live session carried across the upgrade is still
	// reachable at the socket its shim is actually listening on. Deriving it from the new ID would
	// have pointed the server at shim-mig00001.sock and stranded the shell.
	if got.ShimSocket != "/run/cm/shim-old.sock" {
		t.Errorf("ShimSocket = %q, want it unchanged by the upgrade", got.ShimSocket)
	}

	// And the upgraded database still works for writes, not just reads: a migration that left the
	// table in a state INSERT rejects would otherwise pass the assertion above.
	if err := s.Create(ctx, sampleSession("fresh")); err != nil {
		t.Errorf("Create() on an upgraded database error = %v", err)
	}
}

// indexOfMigration returns the position of the one migration containing needle.
//
// Fails rather than returning a sentinel when it is missing or ambiguous: a test that silently pinned
// itself to the wrong schema version would keep passing while covering nothing.
func indexOfMigration(t *testing.T, needle string) int {
	t.Helper()
	found := -1
	for i, m := range migrations {
		if !strings.Contains(m, needle) {
			continue
		}
		if found >= 0 {
			t.Fatalf("%q appears in migrations %d and %d", needle, found+1, i+1)
		}
		found = i
	}
	if found < 0 {
		t.Fatalf("no migration contains %q", needle)
	}
	return found
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

// A database written by a newer cm is refused, with the version skew named.
//
// This is the rollback case, and it used to open successfully: migrate had nothing to apply, so the first
// query failed with "no such column: name". That error names a column rather than the reason it is missing,
// and says nothing about the binary being older than the file, which is the only fact that explains it.
func TestOpenRefusesADatabaseFromANewerBuild(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	ctx := context.Background()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	// One past what this build knows, which is what a newer build would have left.
	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+1)); err != nil {
		t.Fatalf("setting a future schema version: %v", err)
	}
	s.Close()

	_, err = Open(ctx, path)
	if err == nil {
		t.Fatal("Open() on a database from a newer build = nil error, want a refusal")
	}
	// Both numbers, since "which is newer" is the question a reader has.
	for _, want := range []string{
		fmt.Sprint(len(migrations) + 1),
		fmt.Sprint(len(migrations)),
		"newer cm",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Open() error = %v, want it to mention %q", err, want)
		}
	}
}
