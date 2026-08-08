package ansi

import (
	"bytes"
	"strings"
	"testing"
)

// Escape sequences are removed and the text is kept.
//
// The sequences here are the ones cm and a shell actually emit, not invented ones: SGR colour, cursor
// positioning, the bracketed-paste toggles a shell sets around its prompt, and the OSC 133 markers cm reads for
// busy state. Each appeared in real `--follow` output before this filter existed.
func TestStrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{name: "plain text is untouched", in: "hello world\n", want: "hello world\n"},
		{name: "SGR colour", in: "\x1b[31mred\x1b[0m\n", want: "red\n"},
		{name: "SGR with several parameters", in: "\x1b[1;38;5;196mbold red\x1b[0m", want: "bold red"},
		{name: "cursor positioning", in: "a\x1b[2;1Hb", want: "ab"},
		{name: "erase line", in: "before\x1b[Kafter", want: "beforeafter"},
		// The toggles a shell puts around its prompt, which appeared as [?2004l in followed output.
		{name: "bracketed paste toggles", in: "\x1b[?2004hprompt\x1b[?2004l", want: "prompt"},
		// OSC 133, terminated by BEL, which is what cm's own markers use.
		{name: "OSC 133 with BEL", in: "\x1b]133;C\x07running", want: "running"},
		// OSC 7 carrying a path, terminated by ST rather than BEL.
		{name: "OSC with ST terminator", in: "\x1b]7;file://host/tmp\x1b\\done", want: "done"},
		// A title, which is an OSC with text inside that must not leak through.
		{name: "OSC title", in: "\x1b]2;my title\x07text", want: "text"},
		// A short sequence with no parameters at all.
		{name: "full reset", in: "\x1bctext", want: "text"},
		{name: "keypad mode", in: "\x1b=text\x1b>", want: "text"},
		// CR is dropped: a pty writes CRLF, and keeping it would give a redirected file CRLF endings.
		{name: "carriage returns are dropped", in: "one\r\ntwo\r\n", want: "one\ntwo\n"},
		// Tabs and other printable control characters stay, since they are content.
		{name: "tabs are kept", in: "a\tb\n", want: "a\tb\n"},
		// Backspace, which a line editor emits to redraw a prompt. Dropped rather than applied: undoing it
		// would mean deleting the character before, which needs a screen to delete from. So "p\bx" becomes
		// "px", not "x" -- the stray byte goes, the text it would have overwritten stays. A filter cannot do
		// better, and `cm read` is the renderer for when that matters.
		{name: "backspace is dropped, not applied", in: "p\bprompt\n", want: "pprompt\n"},
		// A bell in a log file is a stray byte, not content.
		{name: "bell is dropped", in: "done\x07\n", want: "done\n"},
		// Newlines and tabs survive, since they are the layout a program chose.
		{name: "layout characters survive", in: "col1\tcol2\nrow2\n", want: "col1\tcol2\nrow2\n"},
		{name: "empty input", in: "", want: ""},
		// A realistic line, combining several of the above.
		{
			name: "a real followed line",
			in:   "\x1b]133;C\x07\x1b[0m\x1b[32mPASS\x1b[0m\r\n",
			want: "PASS\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(Strip([]byte(tc.in))); got != tc.want {
				t.Errorf("Strip(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A sequence split across writes is still recognized.
//
// This is why the filter is stateful, and the bug it exists to prevent: a stream delivers whatever has arrived,
// so an escape can be the last byte of one chunk and its terminator the first of the next. A stateless filter
// would pass the tail through as text, so following a session would show fragments like "31mred" -- worse than
// leaving the escapes intact, because it looks like corrupted output rather than colour codes.
func TestStripAcrossWrites(t *testing.T) {
	const in = "\x1b[31mred\x1b[0m\n\x1b]133;C\x07after\n"
	const want = "red\nafter\n"

	// Every possible split point, so no single boundary is special-cased by accident.
	for i := 1; i < len(in); i++ {
		var out bytes.Buffer
		s := NewStripper(&out)
		if _, err := s.Write([]byte(in[:i])); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if _, err := s.Write([]byte(in[i:])); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if got := out.String(); got != want {
			t.Errorf("split at %d: got %q, want %q", i, got, want)
		}
	}
}

// One byte at a time produces the same result.
//
// The extreme case of the above, and the one a slow stream approaches: a pty read can return a single byte.
func TestStripByteAtATime(t *testing.T) {
	const in = "\x1b[1;31mx\x1b[0m\r\n\x1b]2;title\x07y"
	const want = "x\ny"

	var out bytes.Buffer
	s := NewStripper(&out)
	for i := range len(in) {
		if _, err := s.Write([]byte{in[i]}); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	if got := out.String(); got != want {
		t.Errorf("byte at a time: got %q, want %q", got, want)
	}
}

// Write reports every byte consumed, as an io.Writer must.
//
// A short count would be read as an error by anything wrapping this, even though the missing bytes were dropped
// on purpose. io.Copy in particular treats a short write as ErrShortWrite.
func TestStripWriteReportsFullLength(t *testing.T) {
	// Deliberately all escapes, so nothing is written through and the count cannot come from the output.
	in := []byte("\x1b[31m\x1b[0m")

	var out bytes.Buffer
	n, err := NewStripper(&out).Write(in)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if n != len(in) {
		t.Errorf("Write() = %d, want %d: a short count reads as an error", n, len(in))
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want nothing for input that is all escapes", out.String())
	}
}

// An unterminated sequence is eventually emitted rather than held forever.
//
// The bound matters because the alternative is losing output silently: a stream that opens an escape and never
// closes it would otherwise buffer without limit and print nothing. Showing bytes that look odd is the honest
// failure.
func TestStripEmitsUnterminatedSequence(t *testing.T) {
	var out bytes.Buffer
	s := NewStripper(&out)

	// An escape followed by more than maxPending bytes that never terminate it.
	if _, err := s.Write([]byte("\x1b[")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	// Parameter bytes, which keep a CSI open indefinitely.
	if _, err := s.Write(bytes.Repeat([]byte{';'}, maxPending+10)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if out.Len() == 0 {
		t.Error("nothing was emitted for an unterminated sequence, so output would be lost silently")
	}
	// And the filter recovers, so text after the giveaway point is passed through.
	out.Reset()
	if _, err := s.Write([]byte("recovered\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "recovered") {
		t.Errorf("after an unterminated sequence, got %q, want the text through", got)
	}
}
