package ansi

import (
	"strings"
	"testing"
)

func TestTrackerInSequence(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"plain text", "hello", false},
		{"complete CSI", "\x1b[0m", false},
		{"text after a complete CSI", "\x1b[0mhello", false},
		{"bare ESC", "\x1b", true},
		{"CSI with no final byte", "\x1b[38:2:232", true},
		{"CSI with parameters only", "\x1b[7;9", true},
		{"complete OSC, BEL terminated", "\x1b]2;title\x07", false},
		{"complete OSC, ST terminated", "\x1b]2;title\x1b\\", false},
		{"OSC with no terminator", "\x1b]7;kitty-shell-cwd://host/tmp", true},
		{"OSC that has just seen ESC", "\x1b]2;title\x1b", true},
		{"complete DCS", "\x1bP+q4D73\x1b\\", false},
		{"DCS with no terminator", "\x1bP+q4D73", true},
		{"complete APC", "\x1b_Gf=100\x1b\\", false},
		{"APC with no terminator", "\x1b_Gf=100", true},
		{"two-byte escape", "\x1bc", false},
		{"charset selection, an intermediate then a final", "\x1b(B", false},
		{"charset selection cut after the intermediate", "\x1b(", true},
		// The sequence from the bug: a truecolor SGR cut where a pty read ended.
		{"the reported case", ":40:44:52m 30 \x1b(B\x1b[m\x1b[38:2:232", true},
		// A UTF-8 continuation byte must not be read as an 8-bit C1 control.
		{"multi-byte rune", "⋅", false},
		{"an unterminated sequence is given up on", "\x1b[" + strings.Repeat("1;", maxPending), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var tr Tracker
			tr.Feed([]byte(tc.in))
			if got := tr.InSequence(); got != tc.want {
				t.Errorf("InSequence() = %v, want %v, after %q", got, tc.want, tc.in)
			}
		})
	}
}

// TestTrackerAcrossFeeds is the case the tracker exists for: a sequence split the way a pty read
// splits one, where the state has to survive the boundary between two calls.
func TestTrackerAcrossFeeds(t *testing.T) {
	var tr Tracker
	var got []bool
	for _, chunk := range []string{"text\x1b[38", ":2:232", ":102:113m", "more"} {
		tr.Feed([]byte(chunk))
		got = append(got, tr.InSequence())
	}
	want := []bool{true, true, false, false}
	if len(got) != len(want) {
		t.Fatalf("InSequence() after each feed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("InSequence() after each feed = %v, want %v", got, want)
		}
	}
}

// TestTrackerByteAtATime feeds the same stream one byte per call, since a caller has no control over
// how a stream arrives and the answer must not depend on the chunking.
func TestTrackerByteAtATime(t *testing.T) {
	const stream = "a\x1b[1mb\x1b]2;t\x07c\x1bP+q\x1b\\d"
	var whole Tracker
	whole.Feed([]byte(stream))

	var split Tracker
	for i := 0; i < len(stream); i++ {
		split.Feed([]byte(stream[i : i+1]))
	}

	if whole.InSequence() != split.InSequence() {
		t.Errorf("InSequence() = %v fed whole, %v fed byte at a time", whole.InSequence(), split.InSequence())
	}
	if whole.InSequence() {
		t.Errorf("InSequence() = true after a stream of complete sequences %q", stream)
	}
}
