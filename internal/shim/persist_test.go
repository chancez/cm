package shim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/seqlog"
)

// A persisted session's output must reach disk, since that file is the only thing that survives a
// reboot.
func TestSessionPersistsOutput(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "persist.log")

	sess, err := Start(Config{
		Session:     "persisted",
		Command:     []string{"/bin/sh", "-c", "echo PERSISTED_LINE; sleep 5"},
		Rows:        24,
		Cols:        80,
		PersistPath: path,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { sess.Signal(9, true) })

	r := sess.Log().Subscribe(0)
	defer r.Close()
	readUntil(t, r, "PERSISTED_LINE")

	// Read the file back the way a later process would.
	f, err := seqlog.OpenFile(path, seqlog.FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	defer f.Close()

	got, _, err := f.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if !strings.Contains(string(got), "PERSISTED_LINE") {
		t.Errorf("persisted log = %q, want it to contain the session's output", got)
	}
}

// Without a persist path nothing is written, since most sessions are not worth the disk.
func TestSessionWithoutPersistPathWritesNothing(t *testing.T) {
	dir := shortTempDir(t)

	sess, err := Start(Config{
		Session: "ephemeral",
		Command: []string{"/bin/sh", "-c", "echo NOT_PERSISTED; sleep 5"},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { sess.Signal(9, true) })

	r := sess.Log().Subscribe(0)
	defer r.Close()
	readUntil(t, r, "NOT_PERSISTED")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("directory has %d entries, want none for a non-persisted session", len(entries))
	}
}

// The in-memory log must continue the persisted file's numbering, or a sequence number recorded
// before this process started would refer to a different byte.
func TestSessionContinuesPersistedSequenceNumbering(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "continue.log")

	// Pre-existing content, as an earlier run of the same session would have left.
	pre, err := seqlog.OpenFile(path, seqlog.FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	if err := pre.Append([]byte("earlier output\n")); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	_, wantStart := pre.Bounds()
	if err := pre.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	sess, err := Start(Config{
		Session:     "continue",
		Command:     []string{"/bin/sh", "-c", "echo NEW_OUTPUT; sleep 5"},
		Rows:        24,
		Cols:        80,
		PersistPath: path,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { sess.Signal(9, true) })

	if oldest, _ := sess.Log().Bounds(); oldest != wantStart {
		t.Errorf("in-memory log starts at %d, want %d to continue the file's numbering",
			oldest, wantStart)
	}

	r := sess.Log().Subscribe(wantStart)
	defer r.Close()
	if got := readUntil(t, r, "NEW_OUTPUT"); strings.Contains(got, "earlier output") {
		t.Error("the in-memory log replayed content from the file, want only new output")
	}
}

// A session whose persist path cannot be opened must fail loudly. Silently not persisting would be
// discovered only after a reboot, when it is too late to do anything about it.
func TestSessionFailsWhenPersistPathIsUnusable(t *testing.T) {
	// A path whose parent is a file, so the directory cannot be created.
	dir := shortTempDir(t)
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Start(Config{
		Session:     "unusable",
		Command:     []string{"/bin/sh", "-c", "sleep 1"},
		Rows:        24,
		Cols:        80,
		PersistPath: filepath.Join(blocker, "nested", "persist.log"),
	})
	if err == nil {
		t.Error("Start() = nil error with an unusable persist path, want a failure")
	}
}

// The tail is what a restore needs most, so it must be flushed when the shell exits.
func TestSessionFlushesPersistedLogOnExit(t *testing.T) {
	dir := shortTempDir(t)
	path := filepath.Join(dir, "flush.log")

	sess, err := Start(Config{
		Session:     "flush",
		Command:     []string{"/bin/sh", "-c", "echo FINAL_LINE"},
		Rows:        24,
		Cols:        80,
		PersistPath: path,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitExit(t, sess)

	f, err := seqlog.OpenFile(path, seqlog.FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	defer f.Close()

	got, _, err := f.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if !strings.Contains(string(got), "FINAL_LINE") {
		t.Errorf("persisted log = %q, want the shell's final output", got)
	}
}
