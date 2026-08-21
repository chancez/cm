package vt

import (
	"bytes"
	"testing"
)

// A DECRPM reply about a denied mode is rewritten to "not recognized", whatever the model's real
// answer was, and nothing else in the chunk is disturbed.
//
// The reset case is the one that matters and the one that looks harmless. nvim treats set, reset, and
// permanently-set alike: all three mean "this mode can be changed", so ";2" enables the behavior just
// as ";1" would. A fix that only rewrote ";1" would leave both bugs exactly as they are, since ";2" is
// what cm actually answered for each.
func TestDenyModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		// Mode 69, left/right margins. What cm really answered, measured against a live session.
		{name: "margins reset is rewritten", in: "\x1b[?69;2$y", want: "\x1b[?69;0$y"},
		{name: "margins set is rewritten", in: "\x1b[?69;1$y", want: "\x1b[?69;0$y"},
		{name: "margins permanently set is rewritten", in: "\x1b[?69;3$y", want: "\x1b[?69;0$y"},
		// Already not-recognized. The output is what a terminal without the capability says, so this
		// is a no-op rather than a case needing special handling.
		{name: "margins not recognized is unchanged", in: "\x1b[?69;0$y", want: "\x1b[?69;0$y"},

		// Mode 2048, in-band size reports. Also measured: cm answered ";2", and any answer but 0 makes
		// nvim stop reacting to SIGWINCH and wait for reports cm never sends.
		{name: "size reports reset is rewritten", in: "\x1b[?2048;2$y", want: "\x1b[?2048;0$y"},
		{name: "size reports set is rewritten", in: "\x1b[?2048;1$y", want: "\x1b[?2048;0$y"},
		{name: "size reports not recognized is unchanged", in: "\x1b[?2048;0$y", want: "\x1b[?2048;0$y"},

		// Other modes are cm's to answer and must pass through untouched. 1049 and 2004 are the
		// alternate screen and bracketed paste, which a reattaching TUI depends on.
		{name: "another mode is untouched", in: "\x1b[?1049;2$y", want: "\x1b[?1049;2$y"},
		{name: "bracketed paste is untouched", in: "\x1b[?2004;2$y", want: "\x1b[?2004;2$y"},
		// Modes whose numbers merely contain a denied one, which a substring match would corrupt.
		{name: "a mode ending in 69 is untouched", in: "\x1b[?169;2$y", want: "\x1b[?169;2$y"},
		{name: "mode 6 is untouched", in: "\x1b[?6;1$y", want: "\x1b[?6;1$y"},
		{name: "a mode ending in 2048 is untouched", in: "\x1b[?12048;2$y", want: "\x1b[?12048;2$y"},
		{name: "mode 204 is untouched", in: "\x1b[?204;1$y", want: "\x1b[?204;1$y"},
		// 2026 is synchronized output, which shares a prefix with 2048 and is one cm really does own:
		// denying it would make a TUI stop batching its repaints.
		{name: "synchronized output is untouched", in: "\x1b[?2026;2$y", want: "\x1b[?2026;2$y"},

		// Not a query reply at all. The emulator's replies are the only thing this sees today, but a
		// malformed or partial sequence must not be rewritten into something well-formed.
		{name: "plain text is untouched", in: "no replies here", want: "no replies here"},
		{name: "empty is untouched", in: "", want: ""},
		{name: "a truncated report is untouched", in: "\x1b[?69;2", want: "\x1b[?69;2"},
		{name: "a truncated 2048 report is untouched", in: "\x1b[?2048;2", want: "\x1b[?2048;2"},
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
		// Both denied modes in one chunk, which is what nvim's startup probing produces. A rewrite that
		// stopped after the first mode would leave the second claim in place.
		{
			name: "both denied modes in one chunk are rewritten",
			in:   "\x1b[?69;2$y\x1b[?2048;2$y",
			want: "\x1b[?69;0$y\x1b[?2048;0$y",
		},
		{
			name: "both denied modes in the other order",
			in:   "\x1b[?2048;2$y\x1b[?69;2$y",
			want: "\x1b[?2048;0$y\x1b[?69;0$y",
		},
		{
			name: "denied modes interleaved with kept ones",
			in:   "\x1b[?2048;2$y\x1b[?2026;1$y\x1b[?69;1$y\x1b[?7;1$y",
			want: "\x1b[?2048;0$y\x1b[?2026;1$y\x1b[?69;0$y\x1b[?7;1$y",
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
		{
			name: "a malformed 2048 report does not hide a real one",
			in:   "\x1b[?2048;x$y\x1b[?2048;2$y",
			want: "\x1b[?2048;x$y\x1b[?2048;0$y",
		},
		// In-band size reports, which mode 2048 turns on. Dropped rather than rewritten, because they
		// are notifications rather than answers: there is no "not supported" form to send.
		//
		// A chunk that is nothing but a report is the case that matters and the one a nil check on the
		// output slice gets wrong: deleting everything leaves an empty result, which is a different
		// answer from "nothing matched". The first version returned the report unchanged for exactly
		// this input.
		{name: "a lone size report is dropped", in: "\x1b[48;40;100;0;0t", want: ""},
		{name: "a size report with no pixel geometry is dropped", in: "\x1b[48;40;100t", want: ""},
		{
			name: "a size report is dropped from surrounding output",
			in:   "before\x1b[48;40;100;0;0tafter",
			want: "beforeafter",
		},
		{
			name: "two size reports are both dropped",
			in:   "\x1b[48;20;100;0;0t\x1b[48;40;100;0;0t",
			want: "",
		},
		{
			name: "a size report is dropped without disturbing a denied mode reply",
			in:   "\x1b[?2048;2$y\x1b[48;40;100;0;0t",
			want: "\x1b[?2048;0$y",
		},
		// Sequences sharing the prefix that are not size reports. CSI 48 ; 5 ; n m sets a 256-colour
		// background and is extremely common, so matching it would corrupt ordinary styled output.
		{name: "an SGR background colour is untouched", in: "\x1b[48;5;236m", want: "\x1b[48;5;236m"},
		{name: "an SGR truecolor background is untouched", in: "\x1b[48;2;40;44;52m", want: "\x1b[48;2;40;44;52m"},
		{name: "a truncated size report is untouched", in: "\x1b[48;40;100", want: "\x1b[48;40;100"},
		{name: "a non-numeric size report is untouched", in: "\x1b[48;4x;100t", want: "\x1b[48;4x;100t"},
		{name: "a size report with no parameters is untouched", in: "\x1b[48;t", want: "\x1b[48;t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DenyModes([]byte(tc.in))
			if string(got) != tc.want {
				t.Errorf("DenyModes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The input is not modified in place, so a caller holding the original bytes still sees them.
//
// The emulator hands cm a buffer it copied out of C, and the log and the pty write are different
// consumers; a rewrite that scribbled on the shared slice would change bytes elsewhere.
//
// Both modes are covered because they take different paths now that the scan runs once per denied
// mode: the first rewrite allocates a fresh slice, and the second operates on that copy rather than on
// the caller's buffer. A mutation bug is only reachable through the first.
func TestDenyModesDoesNotMutateInput(t *testing.T) {
	for _, in := range []string{
		"\x1b[?69;2$y",
		"\x1b[?2048;2$y",
		"\x1b[?69;2$y\x1b[?2048;2$y",
	} {
		t.Run(in, func(t *testing.T) {
			buf := []byte(in)
			orig := append([]byte(nil), buf...)

			DenyModes(buf)

			if !bytes.Equal(buf, orig) {
				t.Errorf("DenyModes modified its input: got %q, want %q", buf, orig)
			}
		})
	}
}
