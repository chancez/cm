package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A snapshot is a standalone database holding what was committed when it was taken.
//
// The content is the point rather than the file existing. The database runs in WAL mode, so rows can still
// be in the -wal file when the snapshot is taken, and a copy of cm.db alone would produce a file that looks
// fine and is missing the newest sessions -- which is the failure this would hide, since a snapshot is only
// ever read after something has gone wrong.
//
// Calls snapshot directly, at a version number no migration uses, so it does not depend on which migration
// happens to be last.
func TestSnapshotIsAReadableCopy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	ctx := context.Background()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()
	if err := s.Create(ctx, sampleSession("kept2222")); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := s.snapshot(ctx, path, 99); err != nil {
		t.Fatalf("snapshot() error = %v", err)
	}

	// Opened without migrating, since a snapshot is read by the older build rather than upgraded.
	snap, err := openRaw(BackupPath(path, 99))
	if err != nil {
		t.Fatalf("opening the snapshot: %v", err)
	}
	defer snap.Close()
	// A row count rather than a column, so this asserts what the snapshot is for without depending on the
	// schema it happens to hold: what would break is rows missing, not rows shaped differently.
	var count int
	if err := snap.QueryRow(`SELECT count(*) FROM sessions`).Scan(&count); err != nil {
		t.Fatalf("reading the snapshot: %v", err)
	}
	if count != 1 {
		t.Errorf("snapshot holds %d sessions, want the one committed before it was taken", count)
	}
}

// Taking one twice for the same version replaces it, rather than failing on an existing file.
//
// VACUUM INTO refuses a target that exists, and a retried migration is an ordinary thing: the same version
// means the same starting point, so replacing it loses nothing while failing would block the upgrade.
func TestSnapshotReplacesAnEarlierOneForTheSameVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	ctx := context.Background()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	if err := s.snapshot(ctx, path, 99); err != nil {
		t.Fatalf("first snapshot() error = %v", err)
	}
	if err := s.snapshot(ctx, path, 99); err != nil {
		t.Errorf("second snapshot() error = %v, want it to replace the first", err)
	}
}

// Migrating an existing database takes a snapshot named for the version it came from.
//
// Asserts the file rather than its contents, which the test above covers: what matters here is that the
// migration path takes one at all, and doing it without reading the schema keeps this from breaking every
// time a migration is appended.
func TestMigrateSnapshotsBeforeChangingTheSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	from := len(migrations) - 1
	seedAtVersion(t, path, from)

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	if _, err := os.Stat(BackupPath(path, from)); err != nil {
		t.Errorf("no snapshot at %s: %v", BackupPath(path, from), err)
	}
}

// A fresh database has nothing worth keeping, so nothing is written for it. Otherwise every first run would
// leave an empty file behind.
func TestMigrateDoesNotSnapshotAFreshDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")

	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	found, err := filepath.Glob(path + ".v*.bak")
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(found) != 0 {
		t.Errorf("snapshots = %v, want none for a database created from scratch", found)
	}
}

func TestExpireBackupsRemovesOnlyTheOldOnes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	stale := BackupPath(path, 5)
	fresh := BackupPath(path, 6)
	write(t, stale, now.Add(-8*24*time.Hour))
	write(t, fresh, now.Add(-2*24*time.Hour))

	removed, err := ExpireBackups(path, 7*24*time.Hour, now)
	if err != nil {
		t.Fatalf("ExpireBackups() error = %v", err)
	}
	if !reflect.DeepEqual(removed, []string{stale}) {
		t.Errorf("ExpireBackups() = %v, want %v", removed, []string{stale})
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("the snapshot inside the retention period is gone: %v", err)
	}
}

// The sweep must never match the database itself or its sidecars. Getting the glob wrong here deletes the
// live database, which is why this is asserted rather than left to the pattern looking right.
func TestExpireBackupsNeverTouchesTheDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	ancient := now.Add(-365 * 24 * time.Hour)

	// Every neighbour a real state directory has, all old enough to be swept if they matched.
	for _, p := range []string{
		path,
		path + "-wal",
		path + "-shm",
		filepath.Join(dir, "cm.db.bak"),
		filepath.Join(dir, "other.db.v1.bak"),
	} {
		write(t, p, ancient)
	}

	removed, err := ExpireBackups(path, 7*24*time.Hour, now)
	if err != nil {
		t.Fatalf("ExpireBackups() error = %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("ExpireBackups() removed %v, want nothing: none of those is a snapshot of this database",
			removed)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm", filepath.Join(dir, "cm.db.bak")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s is gone: %v", p, err)
		}
	}
}

// The sweep removes an old snapshot while sparing the database in the same call.
//
// The strongest form of the safety check, and the one worth having over the two halves separately: a glob
// that matched too much would delete the database *and* report a plausible-looking removal, so the test
// that proves it removes the right file has to be the same test that proves it kept the wrong ones.
func TestExpireBackupsRemovesSnapshotsAndSparesTheDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	ancient := now.Add(-365 * 24 * time.Hour)

	// Everything old, so age cannot be what spares any of it.
	snapshot := BackupPath(path, 6)
	spared := []string{
		path,
		path + "-wal",
		path + "-shm",
		// Near misses: a hand-made backup, another database's snapshot, and a name that starts the same.
		filepath.Join(dir, "cm.db.bak"),
		filepath.Join(dir, "cm.db.backup"),
		filepath.Join(dir, "other.db.v6.bak"),
		filepath.Join(dir, "cm.db2.v6.bak"),
	}
	write(t, snapshot, ancient)
	for _, p := range spared {
		write(t, p, ancient)
	}

	removed, err := ExpireBackups(path, 7*24*time.Hour, now)
	if err != nil {
		t.Fatalf("ExpireBackups() error = %v", err)
	}
	if !reflect.DeepEqual(removed, []string{snapshot}) {
		t.Errorf("ExpireBackups() = %v, want exactly %v", removed, []string{snapshot})
	}
	if _, err := os.Stat(snapshot); !os.IsNotExist(err) {
		t.Errorf("the snapshot is still there: %v", err)
	}
	for _, p := range spared {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed and must not be: %v", p, err)
		}
	}
}

// Zero keeps them forever, which is the escape hatch for someone who wants the rollback window open.
func TestExpireBackupsWithZeroKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	old := BackupPath(path, 5)
	write(t, old, now.Add(-100*24*time.Hour))

	removed, err := ExpireBackups(path, 0, now)
	if err != nil || removed != nil {
		t.Errorf("ExpireBackups(0) = %v, %v, want nil, nil", removed, err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the snapshot was removed with retention disabled: %v", err)
	}
}

func write(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatalf("Chtimes(%s) error = %v", path, err)
	}
}

// seedAtVersion leaves a database at an earlier schema version, the way an older build would have.
//
// Applies the migration strings and records the version, which is what migrate does, and deliberately does
// not go through Open: Open would migrate it to current, which is the thing under test.
func seedAtVersion(t *testing.T, path string, version int) {
	t.Helper()
	ctx := context.Background()

	raw, err := openRaw(path)
	if err != nil {
		t.Fatalf("openRaw() error = %v", err)
	}
	defer raw.Close()

	for i, m := range migrations[:version] {
		if _, err := raw.ExecContext(ctx, m); err != nil {
			t.Fatalf("applying migration %d: %v", i+1, err)
		}
	}
	if _, err := raw.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("recording schema version %d: %v", version, err)
	}
}

// openRaw opens a database without migrating it, for reading a snapshot or building an old schema.
func openRaw(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// The refusal an older build gives names the snapshot when there is one, since that is a database it can
// read. Without it the message explains the problem and offers nothing.
func TestNewerDatabaseErrorNamesTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cm.db")
	ctx := context.Background()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	// A snapshot from the version this build knows, which is what a newer build would have left behind on
	// its way past it.
	backup := BackupPath(path, len(migrations))
	if err := s.snapshot(ctx, path, len(migrations)); err != nil {
		t.Fatalf("snapshot() error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+1)); err != nil {
		t.Fatalf("setting a future schema version: %v", err)
	}
	s.Close()

	_, err = Open(ctx, path)
	if err == nil {
		t.Fatal("Open() on a newer database = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), backup) {
		t.Errorf("Open() error = %v, want it to name the snapshot at %s", err, backup)
	}
	// And it says what restoring costs, since a snapshot missing a session strands that session's shell.
	if !strings.Contains(err.Error(), "stop every session") {
		t.Errorf("Open() error = %v, want it to say sessions must be stopped first", err)
	}
}
