package seqlog

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/seq"
)

func openTestDir(t *testing.T, limits FileLimits) (*Dir[seq.Shim], string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.log.d")
	d, err := OpenDir[seq.Shim](path, limits)
	if err != nil {
		t.Fatalf("OpenDir() error = %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

// segmentsOn lists a log directory as name -> content, so a test can assert the whole layout.
func segmentsOn(t *testing.T, path string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	got := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(path, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile() error = %v", err)
		}
		got[e.Name()] = string(b)
	}
	return got
}

func appendAll(t *testing.T, l Persisted[seq.Shim], chunks ...string) {
	t.Helper()
	for _, c := range chunks {
		if err := l.Append([]byte(c)); err != nil {
			t.Fatalf("Append(%q) error = %v", c, err)
		}
	}
}

type readResult struct {
	Data   string
	Gap    bool
	Oldest seq.Shim
	Next   seq.Shim
}

func readAll(t *testing.T, l Persisted[seq.Shim], from seq.Shim) readResult {
	t.Helper()
	data, gap, err := l.ReadFrom(from)
	if err != nil {
		t.Fatalf("ReadFrom(%d) error = %v", from, err)
	}
	oldest, next := l.Bounds()
	return readResult{Data: string(data), Gap: gap, Oldest: oldest, Next: next}
}

// A segment is sealed at the first newline past the size, so the next one starts at a line.
func TestDirSealsAtNewlinePastSize(t *testing.T) {
	d, path := openTestDir(t, FileLimits{SegmentBytes: 4})
	appendAll(t, d, "ab", "cd\nef", "gh\n", "ij")

	want := map[string]string{
		segmentName(0):  "abcd\n",
		segmentName(5):  "efgh\n",
		segmentName(10): "ij",
	}
	if got := segmentsOn(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("segments = %q, want %q", got, want)
	}
	if got, want := readAll(t, d, 0), (readResult{Data: "abcd\nefgh\nij", Oldest: 0, Next: 12}); got != want {
		t.Errorf("read = %+v, want %+v", got, want)
	}
}

// Full-screen programs position the cursor rather than writing newlines, so a segment has to be cut
// without one or a single nvim session would grow it without bound.
func TestDirCutsWithoutNewlineAtTwiceSize(t *testing.T) {
	d, path := openTestDir(t, FileLimits{SegmentBytes: 4})
	appendAll(t, d, "0123456789ab")

	want := map[string]string{
		segmentName(0): "01234567",
		segmentName(8): "89ab",
	}
	if got := segmentsOn(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("segments = %q, want %q", got, want)
	}
}

// Retention drops whole segments when one is sealed, keeping the smallest suffix that still covers the
// limit. Sealing "cccc\n" left 10 bytes, so "aaaa\n" went and "bbbb\n" stayed: dropping it too would
// have left 5, under the limit of 6.
func TestDirRetainsWholeSegmentsByBytes(t *testing.T) {
	d, path := openTestDir(t, FileLimits{MaxBytes: 6, SegmentBytes: 4})
	appendAll(t, d, "aaaa\n", "bbbb\n", "cccc\n", "dd")

	want := map[string]string{
		segmentName(5):  "bbbb\n",
		segmentName(10): "cccc\n",
		segmentName(15): "dd",
	}
	if got := segmentsOn(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("segments = %q, want %q", got, want)
	}
	if got, want := readAll(t, d, 0), (readResult{Data: "bbbb\ncccc\ndd", Gap: true, Oldest: 5, Next: 17}); got != want {
		t.Errorf("read = %+v, want %+v", got, want)
	}
}

func TestDirRetainsWholeSegmentsByLines(t *testing.T) {
	d, path := openTestDir(t, FileLimits{MaxLines: 2, SegmentBytes: 4})
	appendAll(t, d, "aaaa\n", "bbbb\n", "cccc\n", "dddd\n")

	want := map[string]string{
		segmentName(10): "cccc\n",
		segmentName(15): "dddd\n",
		segmentName(20): "",
	}
	if got := segmentsOn(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("segments = %q, want %q", got, want)
	}
}

// The bug this type exists for: at the cap, File rewrote everything retained on each append. A
// segment that is not the active one must never be written again, whatever the limits.
func TestDirNeverRewritesASealedSegment(t *testing.T) {
	d, path := openTestDir(t, FileLimits{MaxBytes: 64, SegmentBytes: 8})
	appendAll(t, d, "0123456789\n")
	sealed := filepath.Join(path, segmentName(0))
	before, err := os.Stat(sealed)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		appendAll(t, d, "x")
	}
	after, err := os.Stat(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || before.ModTime() != after.ModTime() || before.Size() != after.Size() {
		t.Errorf("sealed segment changed: before %v %d, after %v %d",
			before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
}

func TestDirReopenContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.log.d")
	limits := FileLimits{SegmentBytes: 4}

	first, err := OpenDir[seq.Shim](path, limits)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, first, "before\n")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenDir[seq.Shim](path, limits)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	appendAll(t, second, "after")

	if got, want := readAll(t, second, 0), (readResult{Data: "before\nafter", Oldest: 0, Next: 12}); got != want {
		t.Errorf("read = %+v, want %+v", got, want)
	}
}

func TestDirReadFromMidSegment(t *testing.T) {
	d, _ := openTestDir(t, FileLimits{SegmentBytes: 4})
	appendAll(t, d, "abcd\n", "efgh\n")

	if got, want := readAll(t, d, 7), (readResult{Data: "gh\n", Oldest: 0, Next: 10}); got != want {
		t.Errorf("read = %+v, want %+v", got, want)
	}
	if got, want := readAll(t, d, 10), (readResult{Oldest: 0, Next: 10}); got != want {
		t.Errorf("read at end = %+v, want %+v", got, want)
	}
}

// A reader opens the log of a session whose shim may still be writing it, so opening must not
// create, trim or delete anything.
func TestDirOpenChangesNothing(t *testing.T) {
	d, path := openTestDir(t, FileLimits{MaxBytes: 4, SegmentBytes: 4})
	appendAll(t, d, "aaaa\n", "bb")
	want := segmentsOn(t, path)

	reader, err := OpenDir[seq.Shim](path, FileLimits{MaxBytes: 1, SegmentBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if got := segmentsOn(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("segments after a read-only open = %q, want %q", got, want)
	}

	empty := filepath.Join(t.TempDir(), "empty.log.d")
	e, err := OpenDir[seq.Shim](empty, FileLimits{})
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	if got := segmentsOn(t, empty); len(got) != 0 {
		t.Errorf("opening an empty log created %q", got)
	}
}

// Only the segments after a break in numbering can be given positions that agree with each other.
func TestDirKeepsWhatFollowsABreak(t *testing.T) {
	path := t.TempDir()
	for name, content := range map[string]string{
		segmentName(0):  "aaaa\n",
		segmentName(20): "cccc\n",
		segmentName(25): "dd",
		"notes.txt":     "not a segment",
	} {
		if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	d, err := OpenDir[seq.Shim](path, FileLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if got, want := readAll(t, d, 0), (readResult{Data: "cccc\ndd", Gap: true, Oldest: 20, Next: 27}); got != want {
		t.Errorf("read = %+v, want %+v", got, want)
	}
}

func TestDirIsOwnerOnly(t *testing.T) {
	d, path := openTestDir(t, FileLimits{})
	appendAll(t, d, "secret")

	for _, p := range []string{path, filepath.Join(path, segmentName(0))} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s mode = %v, want owner-only", p, perm)
		}
	}
}

// A legacy file becomes one segment numbered from where the file said it started, so the shim that
// continues it keeps the same positions.
func TestMigrateFileKeepsNumbering(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "s.log")
	f, err := OpenFile[seq.Shim](file, FileLimits{MaxLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, f, "dropped\n", "kept\n")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(root, "s.log.d")
	if err := MigrateFile(file, dir); err != nil {
		t.Fatalf("MigrateFile() error = %v", err)
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("legacy file still present: %v", err)
	}
	if got, want := segmentsOn(t, dir), map[string]string{segmentName(8): "kept\n"}; !reflect.DeepEqual(got, want) {
		t.Errorf("segments = %q, want %q", got, want)
	}

	d, err := OpenDir[seq.Shim](dir, FileLimits{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	appendAll(t, d, "new")
	if got, want := readAll(t, d, 0), (readResult{Data: "kept\nnew", Gap: true, Oldest: 8, Next: 16}); got != want {
		t.Errorf("read = %+v, want %+v", got, want)
	}
}

// The path comes from the database, so removal must not reach anything cm did not write.
func TestRemoveDirLeavesForeignFiles(t *testing.T) {
	d, path := openTestDir(t, FileLimits{SegmentBytes: 4})
	appendAll(t, d, "aaaa\n", "bb")
	d.Close()
	foreign := filepath.Join(path, "keep.txt")
	if err := os.WriteFile(foreign, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := RemoveDir(path); err == nil {
		t.Error("RemoveDir() succeeded on a directory holding a foreign file")
	}
	if got, want := segmentsOn(t, path), map[string]string{"keep.txt": "x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("left behind %q, want %q", got, want)
	}

	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	if err := RemoveDir(path); err != nil {
		t.Errorf("RemoveDir() error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("directory still present: %v", err)
	}
}

func TestOpenPersistedReadsEitherLayout(t *testing.T) {
	root := t.TempDir()

	file := filepath.Join(root, "file.log")
	f, err := OpenFile[seq.Shim](file, FileLimits{})
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, f, "from a file")
	f.Close()

	dir := filepath.Join(root, "dir.log.d")
	d, err := OpenDir[seq.Shim](dir, FileLimits{})
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, d, "from a dir")
	d.Close()

	for path, want := range map[string]readResult{
		file: {Data: "from a file", Next: 11},
		dir:  {Data: "from a dir", Next: 10},
	} {
		l, err := OpenPersisted[seq.Shim](path, FileLimits{})
		if err != nil {
			t.Fatalf("OpenPersisted(%s) error = %v", path, err)
		}
		if got := readAll(t, l, 0); got != want {
			t.Errorf("OpenPersisted(%s) read = %+v, want %+v", filepath.Base(path), got, want)
		}
		l.Close()
	}
}
