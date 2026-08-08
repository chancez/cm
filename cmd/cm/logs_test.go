package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// captureStdout runs fn with stdout redirected and returns what it wrote.
//
// printLog writes to os.Stdout directly rather than to an injected writer. Capturing it here rather than
// changing the signature, since the command's job is to write to stdout and threading a writer through would be
// a change made only for the test.
func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	saved := os.Stdout
	os.Stdout = w

	// Drained on a goroutine, or a write larger than the pipe buffer deadlocks.
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		done <- sb.String()
	}()

	fnErr := fn()

	os.Stdout = saved
	w.Close()
	out := <-done
	r.Close()
	return out, fnErr
}

// writeLines writes a file of numbered lines and returns its path.
func writeLines(t *testing.T, count int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "log")
	var sb strings.Builder
	for i := range count {
		sb.WriteString(strconv.Itoa(i))
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// printLog prints the last n lines, in order.
//
// The tail is kept in a fixed ring indexed by a counter, which replaced a version that resliced from the front.
// That version was correct and reallocated on every line past the first n. The rewrite moved the interesting
// part into index arithmetic, which is why the boundaries below are covered explicitly; see
// BenchmarkPrintLogTail for what it bought.
func TestPrintLogTail(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines int
		n     int
		want  []string
	}{
		// The ordinary case: more lines than asked for, so the ring has wrapped.
		{name: "wrapped", lines: 100, n: 3, want: []string{"97", "98", "99"}},
		// Exactly n lines, so the ring is full and has not wrapped. The boundary where count%n returns to
		// zero, and where an off-by-one would rotate the output.
		{name: "exactly n", lines: 3, n: 3, want: []string{"0", "1", "2"}},
		// Fewer lines than asked for, where the naive start index would be negative and wrap to the end of the
		// ring, printing empty strings that were never in the file.
		{name: "fewer than n", lines: 2, n: 5, want: []string{"0", "1"}},
		// One past full, the first wrap, where the oldest line is at index 1 rather than 0.
		{name: "one past n", lines: 4, n: 3, want: []string{"1", "2", "3"}},
		{name: "single line", lines: 1, n: 1, want: []string{"0"}},
		// An empty file must print nothing rather than n blank lines.
		{name: "empty file", lines: 0, n: 5, want: nil},
		// A multiple of n, so count%n is zero at the end: the other place an off-by-one shows up.
		{name: "exact multiple", lines: 9, n: 3, want: []string{"6", "7", "8"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeLines(t, tc.lines)

			out, err := captureStdout(t, func() error { return printLog(path, tc.n) })
			if err != nil {
				t.Fatalf("printLog() error = %v", err)
			}

			var got []string
			if out != "" {
				got = strings.Split(strings.TrimSuffix(out, "\n"), "\n")
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("printLog(%d lines, n=%d) = %v, want %v", tc.lines, tc.n, got, tc.want)
			}
		})
	}
}

// A non-positive n prints the whole file.
//
// That is how `cm logs` with no --tail behaves, and it takes a different path: a straight copy rather than the
// ring, so it is not covered by the cases above.
func TestPrintLogWholeFile(t *testing.T) {
	path := writeLines(t, 5)

	out, err := captureStdout(t, func() error { return printLog(path, 0) })
	if err != nil {
		t.Fatalf("printLog() error = %v", err)
	}
	if out != "0\n1\n2\n3\n4\n" {
		t.Errorf("printLog(n=0) = %q, want the whole file", out)
	}
}

// A file whose last line has no newline is still printed.
//
// A log being appended to by a live process can be read mid-line. Dropping that line would silently hide the
// most recent entry, which is the one a reader is looking for.
func TestPrintLogHandlesAPartialFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("first\nsecond\nthird without newline"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	out, err := captureStdout(t, func() error { return printLog(path, 2) })
	if err != nil {
		t.Fatalf("printLog() error = %v", err)
	}
	if out != "second\nthird without newline\n" {
		t.Errorf("printLog() = %q, want the partial final line included", out)
	}
}

// A missing log is an error, not silence.
//
// `cm logs` on a session that never wrote one should say so. Printing nothing successfully would look like an
// empty log, which is a different thing and sends the reader looking in the wrong place.
func TestPrintLogMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.log")
	if _, err := captureStdout(t, func() error { return printLog(path, 10) }); err == nil {
		t.Error("printLog() error = nil for a missing file, want an error")
	}
}

// looksRotated reports a replaced or truncated file, and not an unchanged one.
//
// This drives the reopen in followLog. A false negative makes `cm logs -f` go silent after a rotation, which
// reads as nothing being logged; a false positive reopens constantly and reprints from the start.
func TestLooksRotated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log")
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer f.Close()

	// Unchanged: not rotated.
	rotated, err := looksRotated(f, path)
	if err != nil {
		t.Fatalf("looksRotated() error = %v", err)
	}
	if rotated {
		t.Error("looksRotated() = true for an unchanged file, want false")
	}

	// Grown, which is the common case while following: still the same file, so not rotated.
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	rotated, err = looksRotated(f, path)
	if err != nil {
		t.Fatalf("looksRotated() error = %v", err)
	}
	if rotated {
		t.Error("looksRotated() = true for a file that only grew, want false")
	}

	// Replaced, which is what rotation does: a different inode at the same path.
	replaced := filepath.Join(dir, "new")
	if err := os.WriteFile(replaced, []byte("fresh\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Rename(replaced, path); err != nil {
		t.Fatalf("Rename() error = %v", err)
	}
	rotated, err = looksRotated(f, path)
	if err != nil {
		t.Fatalf("looksRotated() error = %v", err)
	}
	if !rotated {
		t.Error("looksRotated() = false after the file was replaced, want true")
	}
}

// A truncated file is treated as rotated, so the reader starts over rather than waiting past the end.
//
// Separate from replacement because it is the same inode: only the size says anything happened. A reader sitting
// at offset 500 in a file now 10 bytes long would never see another line.
func TestLooksRotatedOnTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 500)), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer f.Close()
	// Read to the end, as a follower does.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("Seek() error = %v", err)
	}

	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	rotated, err := looksRotated(f, path)
	if err != nil {
		t.Fatalf("looksRotated() error = %v", err)
	}
	if !rotated {
		t.Error("looksRotated() = false after truncation, want true: the reader is past the end")
	}
}

// BenchmarkPrintLogTail measures the tail path over a large log.
//
// Here because the reason for the current implementation is a measurement rather than an argument. The obvious
// version resliced from the front, which is correct and reallocates: the slice window walks forward until it
// reaches the end of the backing array, then append grows a new one and copies. Measured over 200k lines with a
// tail of 10: 5.44ms and 7.79 MB for that version against 3.51ms and 1.39 MB for this one.
//
// The allocations that remain are one string per line from Scanner.Text, which is the scan rather than the ring
// and would need a different reading strategy to avoid.
//
// A benchmark rather than a comment alone, so a future rewrite that reintroduces the pattern shows up as a
// number instead of going unnoticed.
func BenchmarkPrintLogTail(b *testing.B) {
	path := filepath.Join(b.TempDir(), "log")
	var sb strings.Builder
	for i := range 200000 {
		sb.WriteString(strconv.Itoa(i))
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		b.Fatalf("WriteFile() error = %v", err)
	}

	// Discarded rather than captured through a pipe: the drain goroutine's scheduling would dominate the
	// measurement, and what is being measured is the scan and the ring.
	saved := os.Stdout
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		b.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	os.Stdout = devNull
	defer func() { os.Stdout = saved }()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := printLog(path, 10); err != nil {
			b.Fatalf("printLog() error = %v", err)
		}
	}
}
