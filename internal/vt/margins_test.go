package vt

import (
	"bytes"
	"testing"
)

// A DECRPM reply about mode 69 is rewritten to "not recognized", whatever the model's real answer
// was, and nothing else in the chunk is disturbed.
//
// The reset case is the one that matters and the one that looks harmless. nvim treats set, reset, and
// permanently-set alike: all three mean "this mode can be changed", so ";2" enables margin scrolling
// just as ";1" would. A fix that only rewrote ";1" would leave the bug exactly as it is, since ";2"
// is what cm actually answered.
func TestDenyMarginMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		// What cm really answered, measured against a live session.
		{name: "reset is rewritten", in: "\x1b[?69;2$y", want: "\x1b[?69;0$y"},
		{name: "set is rewritten", in: "\x1b[?69;1$y", want: "\x1b[?69;0$y"},
		{name: "permanently set is rewritten", in: "\x1b[?69;3$y", want: "\x1b[?69;0$y"},
		// Already not-recognized. The output is what a terminal without the capability says, so this
		// is a no-op rather than a case needing special handling.
		{name: "not recognized is unchanged", in: "\x1b[?69;0$y", want: "\x1b[?69;0$y"},

		// Other modes are cm's to answer and must pass through untouched. 1049 and 2004 are the
		// alternate screen and bracketed paste, which a reattaching TUI depends on.
		{name: "another mode is untouched", in: "\x1b[?1049;2$y", want: "\x1b[?1049;2$y"},
		{name: "bracketed paste is untouched", in: "\x1b[?2004;2$y", want: "\x1b[?2004;2$y"},
		// A mode whose number merely ends in 69, which a prefix match on "69" would corrupt.
		{name: "a mode ending in 69 is untouched", in: "\x1b[?169;2$y", want: "\x1b[?169;2$y"},
		{name: "mode 6 is untouched", in: "\x1b[?6;1$y", want: "\x1b[?6;1$y"},

		// Not a query reply at all. The emulator's replies are the only thing this sees today, but a
		// malformed or partial sequence must not be rewritten into something well-formed.
		{name: "plain text is untouched", in: "no replies here", want: "no replies here"},
		{name: "empty is untouched", in: "", want: ""},
		{name: "a truncated report is untouched", in: "\x1b[?69;2", want: "\x1b[?69;2"},
		{name: "a report with no parameter is untouched", in: "\x1b[?69;$y", want: "\x1b[?69;$y"},
		{name: "a non-numeric parameter is untouched", in: "\x1b[?69;x$y", want: "\x1b[?69;x$y"},

		// Ordering matters: a reply is written into a stream that already holds others, and the
		// rewrite must not move anything around it.
		{
			name: "surrounding replies keep their order",
			in:   "\x1b[?2026;2$y\x1b[?69;2$y\x1b[?2004;2$y",
			want: "\x1b[?2026;2$y\x1b[?69;0$y\x1b[?2004;2$y",
		},
		{
			name: "two margin reports are both rewritten",
			in:   "\x1b[?69;1$y\x1b[?69;2$y",
			want: "\x1b[?69;0$y\x1b[?69;0$y",
		},
		{
			name: "a report embedded in output keeps the output",
			in:   "before\x1b[?69;2$yafter",
			want: "before\x1b[?69;0$yafter",
		},
		// A malformed sequence carrying the prefix must not shield a real report behind it. The first
		// version of this bailed on the whole chunk at the first non-numeric parameter, which would
		// have let the bug through for any program that printed the prefix as text.
		{
			name: "a malformed report does not hide a real one",
			in:   "\x1b[?69;x$y\x1b[?69;2$y",
			want: "\x1b[?69;x$y\x1b[?69;0$y",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DenyMarginMode([]byte(tc.in))
			if string(got) != tc.want {
				t.Errorf("DenyMarginMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The input is not modified in place, so a caller holding the original bytes still sees them.
//
// The emulator hands cm a buffer it copied out of C, and the log and the pty write are different
// consumers; a rewrite that scribbled on the shared slice would change bytes elsewhere.
func TestDenyMarginModeDoesNotMutateInput(t *testing.T) {
	in := []byte("\x1b[?69;2$y")
	orig := append([]byte(nil), in...)

	DenyMarginMode(in)

	if !bytes.Equal(in, orig) {
		t.Errorf("DenyMarginMode modified its input: got %q, want %q", in, orig)
	}
}
