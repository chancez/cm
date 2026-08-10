package osc

import "testing"

func TestStripAnsweredQueries(t *testing.T) {
	tests := []struct {
		name string
		in   string
		out  string
		held string
	}{
		// Nothing to do. The overwhelmingly common case, and the fast path.
		{"plain text", "hello world", "hello world", ""},
		{"no queries but escapes", "\x1b[1;32mgreen\x1b[0m", "\x1b[1;32mgreen\x1b[0m", ""},

		// Queries the emulator answers, so they must not reach the real terminal.
		{"DA1 bare", "A\x1b[cB", "AB", ""},
		{"DA1 explicit zero", "A\x1b[0cB", "AB", ""},
		{"DA2", "A\x1b[>cB", "AB", ""},
		{"DA2 explicit zero", "A\x1b[>0cB", "AB", ""},
		{"DA3", "A\x1b[=cB", "AB", ""},
		{"DECID", "A\x1bZB", "AB", ""},
		{"DSR status", "A\x1b[5nB", "AB", ""},
		{"DSR cursor position", "A\x1b[6nB", "AB", ""},
		{"XTVERSION", "A\x1b[>qB", "AB", ""},
		{"XTVERSION explicit zero", "A\x1b[>0qB", "AB", ""},
		{"kitty keyboard", "A\x1b[?uB", "AB", ""},
		{"DECRQM private", "A\x1b[?2026$pB", "AB", ""},

		// Replies must survive. A reply reaches this code whenever the shell echoes it back, and both
		// of the first two re-trigger an answer if fed to the emulator, so stripping them as if they
		// were queries would remove real output.
		{"DA1 reply", "\x1b[?62;22c", "\x1b[?62;22c", ""},
		{"kitty DA1 reply", "\x1b[?62;52;c", "\x1b[?62;52;c", ""},
		{"DA2 reply", "\x1b[>1;0;0c", "\x1b[>1;0;0c", ""},
		{"kitty keyboard reply", "\x1b[?0u", "\x1b[?0u", ""},
		{"DSR status reply", "\x1b[0n", "\x1b[0n", ""},
		{"cursor position reply", "\x1b[12;34R", "\x1b[12;34R", ""},
		{"DECRQM reply", "\x1b[?2026;2$y", "\x1b[?2026;2$y", ""},

		// Queries only the real terminal can answer must pass through, or a program hangs.
		{"OSC 11 background", "\x1b]11;?\x07", "\x1b]11;?\x07", ""},
		{"OSC 10 foreground", "\x1b]10;?\x07", "\x1b]10;?\x07", ""},
		{"OSC 52 clipboard read", "\x1b]52;c;?\x07", "\x1b]52;c;?\x07", ""},
		{"XTGETTCAP", "\x1bP+q544e\x1b\\", "\x1bP+q544e\x1b\\", ""},
		{"XTWINOPS pixel size", "\x1b[14t", "\x1b[14t", ""},
		{"XTWINOPS char size", "\x1b[18t", "\x1b[18t", ""},
		{"color scheme query", "\x1b[?996n", "\x1b[?996n", ""},

		// Ordinary output that shares a final byte with a query family.
		{"cursor position move", "\x1b[10;20H", "\x1b[10;20H", ""},
		{"cursor style", "\x1b[2 q", "\x1b[2 q", ""},
		{"ANSI DECRQM", "\x1b[4$p", "\x1b[4$p", ""},

		// Several in one chunk, which is what a shell startup looks like.
		{"multiple queries", "a\x1b[cb\x1b[6nc", "abc", ""},
		{"query beside a reply", "\x1b[c\x1b[?62;22c", "\x1b[?62;22c", ""},

		// A query split across reads. Held rather than forwarded, since the real terminal would
		// otherwise reassemble it and answer after all.
		{"partial ESC", "abc\x1b", "abc", "\x1b"},
		{"partial CSI", "abc\x1b[", "abc", "\x1b["},
		{"partial CSI with params", "abc\x1b[?202", "abc", "\x1b[?202"},
		{"partial DECRQM", "abc\x1b[?2026$", "abc", "\x1b[?2026$"},

		// Any incomplete sequence is held, not just one that could become a query. The outer
		// terminal's parser would hold the fragment until its final byte arrived anyway, so this
		// delays nothing observable and keeps the rule simple enough to be obviously correct.
		{"partial SGR", "abc\x1b[1;3", "abc", "\x1b[1;3"},
		{"partial OSC", "abc\x1b]11;", "abc", "\x1b]11;"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, held := StripAnsweredQueries([]byte(tt.in))
			if string(out) != tt.out || string(held) != tt.held {
				t.Errorf("StripAnsweredQueries(%q) = (%q, %q), want (%q, %q)",
					tt.in, out, held, tt.out, tt.held)
			}
		})
	}
}

// A partial sequence must be answerable once its remainder arrives, which is the whole point of
// returning it rather than dropping it.
func TestStripAnsweredQueriesAcrossChunks(t *testing.T) {
	first, held := StripAnsweredQueries([]byte("before\x1b["))
	if string(first) != "before" || string(held) != "\x1b[" {
		t.Fatalf("first chunk = (%q, %q), want (%q, %q)", first, held, "before", "\x1b[")
	}

	// The caller prepends what was held to the next chunk.
	second, held2 := StripAnsweredQueries(append(held, []byte("cafter")...))
	if string(second) != "after" || string(held2) != "" {
		t.Errorf("rejoined chunk = (%q, %q), want (%q, %q): a query split across two reads must "+
			"still be stripped, or the real terminal reassembles it and answers a second time",
			second, held2, "after", "")
	}
}

// An unbounded hold would let a stream of ESC followed by ordinary text grow the buffer without
// limit, so a long run of plausible parameters is forwarded instead of retained.
func TestStripAnsweredQueriesBoundsHeldBytes(t *testing.T) {
	long := make([]byte, 0, MaxQueryPartial+16)
	long = append(long, 0x1b, '[')
	for len(long) < MaxQueryPartial+10 {
		long = append(long, '1')
	}

	out, held := StripAnsweredQueries(long)
	if len(held) != 0 {
		t.Errorf("held %d bytes for a %d-byte partial, want 0: a partial longer than "+
			"MaxQueryPartial (%d) must be forwarded rather than retained", len(held), len(long), MaxQueryPartial)
	}
	if string(out) != string(long) {
		t.Errorf("out = %q, want the input unchanged %q", out, long)
	}
}

// The input must not be modified in place. The pump feeds the original bytes to the terminal model
// after this runs, so mutating them would corrupt the model's view.
func TestStripAnsweredQueriesDoesNotMutateInput(t *testing.T) {
	in := []byte("a\x1b[cb")
	before := string(in)

	out, _ := StripAnsweredQueries(in)
	if string(in) != before {
		t.Errorf("input became %q, want it left as %q", in, before)
	}
	if string(out) != "ab" {
		t.Errorf("out = %q, want %q", out, "ab")
	}
}
