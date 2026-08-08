package seqlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTestFile(t *testing.T, limits FileLimits) (*File, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.log")
	f, err := OpenFile(path, limits)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f, path
}

func TestFileAppendAndRead(t *testing.T) {
	f, _ := openTestFile(t, FileLimits{})

	if err := f.Append([]byte("hello ")); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := f.Append([]byte("world")); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, gap, err := f.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if string(got) != "hello world" || gap {
		t.Errorf("ReadFrom(0) = (%q, %v), want (%q, false)", got, gap, "hello world")
	}

	if oldest, next := f.Bounds(); oldest != 0 || next != 11 {
		t.Errorf("Bounds() = (%d, %d), want (0, 11)", oldest, next)
	}
}

func TestFileReadFromOffset(t *testing.T) {
	f, _ := openTestFile(t, FileLimits{})
	if err := f.Append([]byte("0123456789")); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, gap, err := f.ReadFrom(6)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if string(got) != "6789" || gap {
		t.Errorf("ReadFrom(6) = (%q, %v), want (%q, false)", got, gap, "6789")
	}

	// Past the end is not an error: a caller fully caught up asks for exactly this.
	got, _, err = f.ReadFrom(10)
	if err != nil {
		t.Fatalf("ReadFrom(end) error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ReadFrom(end) = %q, want nothing", got)
	}
}

// Surviving a restart is the whole point: the file must be adopted and appended to rather than
// truncated, and its sequence numbering must continue.
func TestFileReopenContinues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.log")

	first, err := OpenFile(path, FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	if err := first.Append([]byte("before\n")); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	_, wantNext := first.Bounds()
	if err := first.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	second, err := OpenFile(path, FileLimits{})
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer second.Close()

	if _, next := second.Bounds(); next != wantNext {
		t.Errorf("after reopen next = %d, want %d", next, wantNext)
	}
	if err := second.Append([]byte("after\n")); err != nil {
		t.Fatalf("Append() after reopen error = %v", err)
	}

	got, _, err := second.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if string(got) != "before\nafter\n" {
		t.Errorf("content = %q, want %q", got, "before\nafter\n")
	}
}

// Line trimming drops whole lines from the front, so the retained content starts at a line boundary
// rather than mid-escape.
func TestFileTrimsByLines(t *testing.T) {
	f, _ := openTestFile(t, FileLimits{MaxLines: 3})

	for _, line := range []string{"one\n", "two\n", "three\n", "four\n", "five\n"} {
		if err := f.Append([]byte(line)); err != nil {
			t.Fatalf("Append(%q) error = %v", line, err)
		}
	}

	got, _, err := f.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if want := "three\nfour\nfive\n"; string(got) != want {
		t.Errorf("content = %q, want %q", got, want)
	}

	// The sequence numbering must account for what was dropped, or a replay would number the
	// output from zero and disagree with the rest of the system.
	oldest, next := f.Bounds()
	if oldest != uint64(len("one\ntwo\n")) {
		t.Errorf("oldest = %d, want %d", oldest, len("one\ntwo\n"))
	}
	if next-oldest != uint64(len("three\nfour\nfive\n")) {
		t.Errorf("retained span = %d, want %d", next-oldest, len("three\nfour\nfive\n"))
	}
}

// A caller resuming from a position that has been trimmed must be told its view is discontinuous.
func TestFileReportsGapAfterTrim(t *testing.T) {
	f, _ := openTestFile(t, FileLimits{MaxLines: 2})
	for _, line := range []string{"a\n", "b\n", "c\n", "d\n"} {
		if err := f.Append([]byte(line)); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	got, gap, err := f.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if !gap {
		t.Error("ReadFrom(0) gap = false, want true after trimming")
	}
	if string(got) != "c\nd\n" {
		t.Errorf("content = %q, want %q", got, "c\nd\n")
	}
}

// The byte ceiling is a backstop, not an alternative to the line limit: one very long line must not
// be able to fill the disk regardless of how few lines it is.
func TestFileByteCeilingBoundsOneEnormousLine(t *testing.T) {
	f, _ := openTestFile(t, FileLimits{MaxLines: 1000, MaxBytes: 100})

	// A single line far larger than the byte ceiling, well inside the line limit.
	if err := f.Append([]byte(strings.Repeat("x", 500))); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, gap, err := f.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if len(got) > 100 {
		t.Errorf("retained %d bytes, want at most 100: the line limit alone would not bound this",
			len(got))
	}
	if !gap {
		t.Error("gap = false, want true since the front was dropped")
	}
}

func TestFileHonorsBothLimits(t *testing.T) {
	f, _ := openTestFile(t, FileLimits{MaxLines: 5, MaxBytes: 20})

	for i := range 20 {
		if err := f.Append([]byte(strings.Repeat("y", 9) + "\n")); err != nil {
			t.Fatalf("Append() %d error = %v", i, err)
		}
	}

	got, _, err := f.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	// Whichever limit binds tighter wins; here it is bytes.
	if len(got) > 20 {
		t.Errorf("retained %d bytes, want at most 20", len(got))
	}
}

// An unreadable file must not stop a session from starting. Refusing to run because of a corrupt
// cache trades something recoverable for something that is not.
func TestFileResetsOnUnrecognizedContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.log")
	if err := os.WriteFile(path, []byte("this is not a cm log at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := OpenFile(path, FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() on a corrupt file error = %v, want it to recover", err)
	}
	defer f.Close()

	if oldest, next := f.Bounds(); oldest != 0 || next != 0 {
		t.Errorf("Bounds() = (%d, %d), want (0, 0) after resetting a corrupt file", oldest, next)
	}
	if err := f.Append([]byte("fresh")); err != nil {
		t.Fatalf("Append() after reset error = %v", err)
	}
}

func TestFileTruncatedHeaderRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.log")
	// Shorter than a header, which is what a crash mid-create leaves behind.
	if err := os.WriteFile(path, []byte("cm"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := OpenFile(path, FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	defer f.Close()
	if err := f.Append([]byte("ok")); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
}

// The log holds whatever the user typed or read, so it must not be world-readable.
func TestFileIsOwnerOnly(t *testing.T) {
	_, path := openTestFile(t, FileLimits{})

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

func TestFileEmptyAppendIsNoop(t *testing.T) {
	f, _ := openTestFile(t, FileLimits{})
	if err := f.Append(nil); err != nil {
		t.Fatalf("Append(nil) error = %v", err)
	}
	if err := f.Append([]byte{}); err != nil {
		t.Fatalf("Append(empty) error = %v", err)
	}
	if oldest, next := f.Bounds(); oldest != 0 || next != 0 {
		t.Errorf("Bounds() = (%d, %d), want (0, 0)", oldest, next)
	}
}

func TestFileCloseIsIdempotent(t *testing.T) {
	f, _ := openTestFile(t, FileLimits{})
	if err := f.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("second Close() error = %v, want nil", err)
	}
}
