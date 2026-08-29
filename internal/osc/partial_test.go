package osc

import "testing"

// PartialMarkerLen decides what the output pump holds back, so it is the whole of the fix for a prompt
// marker split by a pty read boundary. Holding too little loses the rewrite, which is the bug; holding too
// much withholds session output, which is worse.
func TestPartialMarkerLen(t *testing.T) {
	for _, tc := range []struct {
		name string
		buf  string
		want int
	}{
		{name: "no marker at all", buf: "plain output", want: 0},
		{name: "a complete BEL-terminated marker", buf: "a\x1b]133;A\x07b", want: 0},
		{name: "a complete ST-terminated marker", buf: "a\x1b]133;A\x1b\\b", want: 0},
		{name: "a complete marker at the very end", buf: "a\x1b]133;A\x07", want: 0},
		// The cases the bug was made of: an introducer with no terminator yet.
		{name: "an unterminated marker", buf: "a\x1b]133;A", want: 7},
		{name: "an unterminated marker with parameters", buf: "a\x1b]133;A;redraw=1", want: 16},
		// And the introducer itself arriving in pieces, which is the split that loses it earliest.
		{name: "a lone ESC", buf: "a\x1b", want: 1},
		{name: "ESC then bracket", buf: "a\x1b]", want: 2},
		{name: "most of the introducer", buf: "a\x1b]13", want: 4},
		{name: "the introducer exactly", buf: "a\x1b]133;", want: 6},
		// A completed marker followed by the start of another: only the second is pending.
		{name: "a complete marker then a partial one", buf: "\x1b]133;A\x07text\x1b]133;", want: 6},
		// Not a marker: some other OSC. Held only as far as the shared prefix, which is the price of not
		// knowing yet, and it is bounded and released on the next read.
		{name: "a different OSC, complete", buf: "a\x1b]2;title\x07", want: 0},
		{name: "an empty buffer", buf: "", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PartialMarkerLen([]byte(tc.buf)); got != tc.want {
				t.Errorf("PartialMarkerLen(%q) = %d, want %d", tc.buf, got, tc.want)
			}
		})
	}
}

// A run of bytes that begins like a marker and never finishes must not be held without limit.
//
// The bound is the same reason CommandTracker has one: a stream that emits an introducer and then megabytes
// of ordinary text would otherwise grow the buffer forever. Past the bound the bytes are treated as not a
// marker, which is the safe direction, since the alternative is withholding session output indefinitely.
func TestPartialMarkerLenIsBounded(t *testing.T) {
	huge := "\x1b]133;" + string(make([]byte, maxPartial+1))
	if got := PartialMarkerLen([]byte(huge)); got != 0 {
		t.Errorf("PartialMarkerLen on a %d-byte run starting with an introducer = %d, want 0: an "+
			"unbounded hold would withhold session output forever", len(huge), got)
	}
}

// Reassembly across a split has to produce the same rewrite as the whole stream, which is the property the
// pump exists to give RewritePromptRedraw. Asserted here rather than only in the pump, since this is where
// the arithmetic lives.
func TestHoldingBackRestoresTheRewriteAtEveryBoundary(t *testing.T) {
	for _, stream := range []string{
		"before\x1b]133;A\x07prompt$ ",
		"before\x1b]133;A\x1b\\prompt$ ",
		"before\x1b]133;A;redraw=1\x07prompt$ ",
		"before\x1b]133;A;cl=m;redraw=1;k=i\x07prompt$ ",
		"a\x1b]133;A\x07p1$ b\x1b]133;A\x07p2$ ",
	} {
		t.Run(stream, func(t *testing.T) {
			whole := string(RewritePromptRedraw([]byte(stream)))

			for split := 1; split < len(stream); split++ {
				// The pump's algorithm: prepend anything held, hold back a trailing partial, rewrite the
				// rest.
				var out []byte
				var held []byte
				for _, chunk := range []string{stream[:split], stream[split:]} {
					buf := append(held, chunk...)
					held = nil
					if n := PartialMarkerLen(buf); n > 0 {
						held = append([]byte(nil), buf[len(buf)-n:]...)
						buf = buf[:len(buf)-n]
					}
					out = append(out, RewritePromptRedraw(buf)...)
				}
				// Anything still held at the end of the stream is released, as it would be when the shell
				// writes again or the session ends.
				out = append(out, RewritePromptRedraw(held)...)

				if string(out) != whole {
					t.Errorf("split at %d still changes the rewrite.\n split: %q\n whole: %q",
						split, out, whole)
					return
				}
			}
		})
	}
}
