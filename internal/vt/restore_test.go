package vt

import (
	"strings"
	"testing"
)

// roundTrip serializes a terminal's state, replays it into a fresh terminal of the same size,
// and returns the second terminal's plain-text contents.
//
// This is the test that matters for restore. Upstream has no VT round-trip test of its own, and
// the failure mode without one is a screen that looks plausible but has the cursor or styling
// subtly wrong.
func roundTrip(t *testing.T, src *Terminal) (string, string) {
	t.Helper()

	restore, err := src.Restore()
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	rows, cols, err := src.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	dst, err := New(rows, cols, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer dst.Close()

	if err := dst.Write(restore); err != nil {
		t.Fatalf("replaying restore bytes: %v", err)
	}

	before, err := src.Plain()
	if err != nil {
		t.Fatalf("source Plain() error = %v", err)
	}
	after, err := dst.Plain()
	if err != nil {
		t.Fatalf("restored Plain() error = %v", err)
	}
	return normalize(string(before)), normalize(string(after))
}

// normalize trims trailing whitespace per line and drops trailing blank lines, so comparisons
// are about content rather than how far the formatter pads.
func normalize(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

func newTerminal(t *testing.T, rows, cols uint16) *Terminal {
	t.Helper()
	term, err := New(rows, cols, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { term.Close() })
	return term
}

func TestRestoreRoundTripPlainText(t *testing.T) {
	term := newTerminal(t, 24, 80)
	if err := term.Write([]byte("hello world\r\nsecond line\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	before, after := roundTrip(t, term)
	if before != after {
		t.Errorf("round trip mismatch:\n before = %q\n after  = %q", before, after)
	}
	if !strings.Contains(after, "hello world") {
		t.Errorf("restored screen = %q, want it to contain %q", after, "hello world")
	}
}

func TestRestoreRoundTripWithStyles(t *testing.T) {
	term := newTerminal(t, 24, 80)
	// Bold red, then reset, so styling is present but not left open.
	if err := term.Write([]byte("\x1b[1;31mred bold\x1b[0m normal\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	before, after := roundTrip(t, term)
	if before != after {
		t.Errorf("round trip mismatch:\n before = %q\n after  = %q", before, after)
	}
}

// Scrollback is the case a single-pass restore gets wrong, so it is worth asserting that
// content survives and the visible screen is not duplicated.
func TestRestoreRoundTripWithScrollback(t *testing.T) {
	term := newTerminal(t, 5, 40)

	var sb strings.Builder
	for i := range 20 {
		sb.WriteString("line ")
		sb.WriteByte(byte('0' + i%10))
		sb.WriteString("\r\n")
	}
	if err := term.Write([]byte(sb.String())); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	before, after := roundTrip(t, term)
	if before != after {
		t.Errorf("round trip mismatch:\n before = %q\n after  = %q", before, after)
	}
}

// Cursor position must survive, since the shell draws its prompt relative to it. Getting this
// wrong is the specific failure a one-phase restore produces.
func TestRestorePreservesCursorPosition(t *testing.T) {
	term := newTerminal(t, 24, 80)
	// Put the cursor at row 5, column 10 and leave a marker there.
	if err := term.Write([]byte("\x1b[5;10HMARKER")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	restore, err := term.Restore()
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	dst := newTerminal(t, 24, 80)
	if err := dst.Write(restore); err != nil {
		t.Fatalf("replaying: %v", err)
	}

	// Writing more must continue where the cursor was, so the marker text stays contiguous.
	if err := dst.Write([]byte("_APPENDED")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := dst.Plain()
	if err != nil {
		t.Fatalf("Plain() error = %v", err)
	}
	if !strings.Contains(string(got), "MARKER_APPENDED") {
		t.Errorf("restored screen = %q, want MARKER_APPENDED contiguous: the cursor landed in the wrong place",
			normalize(string(got)))
	}
}

// Synchronized output must not be replayed: a client that receives it withholds rendering until
// its own timeout fires, so the session appears frozen on attach.
func TestRestoreOmitsSynchronizedOutput(t *testing.T) {
	term := newTerminal(t, 24, 80)
	// Enter synchronized output and leave it on, as a program mid-render would.
	if err := term.Write([]byte("\x1b[?2026h" + "content")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	restore, err := term.Restore()
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if strings.Contains(string(restore), "2026h") {
		t.Errorf("restore bytes contain a synchronized-output set: %q", restore)
	}

	// And the source terminal's own mode must be put back, since suppression is only for the
	// duration of serialization.
	on, err := term.mode(cmSyncMode())
	if err != nil {
		t.Fatalf("mode() error = %v", err)
	}
	if !on {
		t.Error("synchronized output left off on the source terminal, want it restored")
	}
}

// The title is not part of the formatter's output, so it has to be replayed separately or a
// reattaching client shows its own process name.
func TestRestoreIncludesTitle(t *testing.T) {
	term := newTerminal(t, 24, 80)
	if err := term.Write([]byte("\x1b]2;my session\x07hello")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	title, err := term.Title()
	if err != nil {
		t.Fatalf("Title() error = %v", err)
	}
	if title != "my session" {
		t.Fatalf("Title() = %q, want %q", title, "my session")
	}

	restore, err := term.Restore()
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if !strings.Contains(string(restore), "my session") {
		t.Errorf("restore bytes = %q, want them to carry the title", restore)
	}
}

// Alternate-screen content must not leak into the restored main screen: a client that attaches
// while a full-screen program is running should see that program, not its own shell history
// mixed in.
func TestRestoreDoesNotLeakAlternateScreen(t *testing.T) {
	term := newTerminal(t, 10, 40)
	if err := term.Write([]byte("MAIN_SCREEN_TEXT\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	// Switch to the alternate screen and write something else.
	if err := term.Write([]byte("\x1b[?1049h" + "ALT_SCREEN_TEXT")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	before, after := roundTrip(t, term)
	if strings.Contains(after, "MAIN_SCREEN_TEXT") {
		t.Errorf("restored alternate screen leaked main-screen content:\n before = %q\n after = %q",
			before, after)
	}
	if !strings.Contains(after, "ALT_SCREEN_TEXT") {
		t.Errorf("restored screen = %q, want the alternate screen's content", after)
	}
}

func TestRestoreEmptyTerminalProducesNothingHarmful(t *testing.T) {
	term := newTerminal(t, 24, 80)

	// An untouched terminal has nothing worth restoring; whatever comes back must at least
	// replay without corrupting a fresh terminal.
	before, after := roundTrip(t, term)
	if before != after {
		t.Errorf("round trip mismatch on an empty terminal:\n before = %q\n after = %q",
			before, after)
	}
}

func TestPlainAndVTAndHTML(t *testing.T) {
	term := newTerminal(t, 24, 80)
	if err := term.Write([]byte("\x1b[32mgreen\x1b[0m text\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	plain, err := term.Plain()
	if err != nil {
		t.Fatalf("Plain() error = %v", err)
	}
	if !strings.Contains(string(plain), "green text") {
		t.Errorf("Plain() = %q, want the text without escapes", normalize(string(plain)))
	}
	// Plain text must carry no escape sequences, since it is meant to be piped.
	if strings.Contains(string(plain), "\x1b") {
		t.Errorf("Plain() contains escape sequences: %q", plain)
	}

	vt, err := term.VT()
	if err != nil {
		t.Fatalf("VT() error = %v", err)
	}
	if !strings.Contains(string(vt), "green") {
		t.Errorf("VT() = %q, want the content", vt)
	}

	html, err := term.HTML()
	if err != nil {
		t.Fatalf("HTML() error = %v", err)
	}
	if !strings.Contains(string(html), "green") {
		t.Errorf("HTML() = %q, want the content", html)
	}
}

func TestResizeReflowsContent(t *testing.T) {
	term := newTerminal(t, 24, 80)
	if err := term.Write([]byte("some text that will reflow\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if err := term.Resize(30, 40); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	rows, cols, err := term.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if rows != 30 || cols != 40 {
		t.Errorf("Size() = (%d, %d), want (30, 40)", rows, cols)
	}

	got, err := term.Plain()
	if err != nil {
		t.Fatalf("Plain() error = %v", err)
	}
	if !strings.Contains(strings.ReplaceAll(normalize(string(got)), "\n", ""), "some text") {
		t.Errorf("content lost across resize: %q", normalize(string(got)))
	}
}

func TestNewRejectsZeroSize(t *testing.T) {
	if _, err := New(0, 80, Callbacks{}); err == nil {
		t.Error("New(0, 80) = nil error, want rejection")
	}
	if _, err := New(24, 0, Callbacks{}); err == nil {
		t.Error("New(24, 0) = nil error, want rejection")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := term.Close(); err != nil {
		t.Fatalf("first Close() error = %v", err)
	}
	if err := term.Close(); err != nil {
		t.Errorf("second Close() error = %v, want nil", err)
	}
	// Operations after close must fail rather than touch freed memory.
	if err := term.Write([]byte("x")); err == nil {
		t.Error("Write() after Close = nil error, want failure")
	}
	if _, err := term.Restore(); err == nil {
		t.Error("Restore() after Close = nil error, want failure")
	}
}

// Regression test for a bug the generic round-trip check nearly hid.
//
// Restoring scrollback into a terminal much shorter than the scrollback exposed two faults:
// the last scrollback line was overwritten because the formatter emits no trailing newline,
// and using ED to clear the viewport erased rows that were still showing scrollback rather
// than scrolling them away. Both only appear when the terminal is short relative to the
// content, so a tall terminal masks them.
func TestRestorePreservesScrollbackOnShortTerminal(t *testing.T) {
	const (
		rows  = 5
		lines = 20
	)
	term := newTerminal(t, rows, 40)

	var want []string
	var sb strings.Builder
	for i := range lines {
		line := "unique-line-" + string(rune('a'+i%26))
		want = append(want, line)
		sb.WriteString(line + "\r\n")
	}
	if err := term.Write([]byte(sb.String())); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	_, after := roundTrip(t, term)
	got := strings.Split(after, "\n")

	if len(got) != len(want) {
		t.Fatalf("restored %d lines, want %d:\n%s", len(got), len(want), after)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("restored line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Regression test for a SIGBUS.
//
// ghostty_terminal_set takes a *pointer to* the value being set, so installing callbacks from Go
// handed libghostty the address of a Go local. It survived until the stack slot was reused, then
// crashed inside vt_write. None of the other tests installed callbacks, so nothing caught it.
func TestCallbacksFireWithoutCrashing(t *testing.T) {
	var (
		gotTitle string
		gotPwd   string
		gotPty   [][]byte
	)

	term, err := New(24, 80, Callbacks{
		WritePty:     func(data []byte) { gotPty = append(gotPty, data) },
		TitleChanged: func(title string) { gotTitle = title },
		PwdChanged:   func(pwd string) { gotPwd = pwd },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	// Enough writes that a reused stack slot would have been noticed.
	for i := range 50 {
		if err := term.Write([]byte("\x1b]2;title-x\x07some output\r\n")); err != nil {
			t.Fatalf("Write() %d error = %v", i, err)
		}
	}
	if gotTitle != "title-x" {
		t.Errorf("title callback gave %q, want %q", gotTitle, "title-x")
	}

	if err := term.Write([]byte("\x1b]7;file:///tmp/somewhere\x1b\\")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if !strings.Contains(gotPwd, "/tmp/somewhere") {
		t.Errorf("pwd callback gave %q, want it to contain %q", gotPwd, "/tmp/somewhere")
	}

	// A device status report must produce a reply, since a program that asks blocks until it
	// gets one. This is the callback that matters for correctness rather than display.
	if err := term.Write([]byte("\x1b[6n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if len(gotPty) == 0 {
		t.Error("no write_pty callback for a cursor position report; a program asking would hang")
	}
}
