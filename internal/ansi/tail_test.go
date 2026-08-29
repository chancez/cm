package ansi

import (
	"strings"
	"testing"
)

// PartialTailLen decides what the output pump holds back, so holding too little loses a sequence to a
// scanner and holding too much withholds session output.
func TestPartialTailLen(t *testing.T) {
	for _, tc := range []struct {
		name string
		buf  string
		want int
	}{
		{name: "plain text ends at a boundary", buf: "hello", want: 0},
		{name: "an empty buffer", buf: "", want: 0},
		{name: "a complete CSI", buf: "a\x1b[31m", want: 0},
		{name: "a complete OSC with BEL", buf: "a\x1b]2;title\x07", want: 0},
		{name: "a complete OSC with ST", buf: "a\x1b]2;title\x1b\\", want: 0},
		{name: "a complete DCS", buf: "a\x1bP+q544e\x1b\\", want: 0},
		{name: "a complete APC", buf: "a\x1b_Ga=q;AAAA\x1b\\", want: 0},

		// The shapes the bugs were made of.
		{name: "a lone ESC", buf: "a\x1b", want: 1},
		{name: "an incomplete CSI", buf: "a\x1b[14", want: 4},
		{name: "an incomplete OSC", buf: "a\x1b]11;?", want: 6},
		{name: "an incomplete prompt marker", buf: "a\x1b]133;A", want: 7},
		{name: "an incomplete DCS", buf: "a\x1bP+q544", want: 7},
		{name: "a DCS awaiting its ST tail", buf: "a\x1bP+q544e\x1b", want: 9},
		{name: "an incomplete APC", buf: "a\x1b_Ga=q", want: 6},

		// A complete sequence followed by the start of another: only the second is pending. This is the case
		// that broke the first, OSC-specific version of this, which searched backwards, found the terminated
		// sequence and reported nothing pending.
		{name: "complete then partial", buf: "\x1b]133;A\x07text\x1b]133;", want: 6},
		{name: "two complete then a lone ESC", buf: "\x1b[31m\x1b[0m\x1b", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PartialTailLen([]byte(tc.buf)); got != tc.want {
				t.Errorf("PartialTailLen(%q) = %d, want %d", tc.buf, got, tc.want)
			}
		})
	}
}

// Prepending what was held has to reassemble the stream exactly, whatever the split, which is the property
// the pump depends on. Swept rather than spot-checked, since the offsets that matter depend on the payload.
func TestHoldingBackReassemblesEveryStream(t *testing.T) {
	for _, stream := range []string{
		"before\x1b]133;A;redraw=1\x07prompt$ ",
		"painting\x1b]11;?\x07more",
		"painting\x1b[14tmore",
		"painting\x1bP+q544e\x1b\\more",
		"a\x1b]133;A\x07p1$ b\x1b]133;A\x07p2$ ",
		"plain text with no sequences at all",
	} {
		t.Run(stream, func(t *testing.T) {
			for split := 1; split < len(stream); split++ {
				var out strings.Builder
				var held string
				for _, chunk := range []string{stream[:split], stream[split:]} {
					buf := held + chunk
					held = ""
					if n := PartialTailLen([]byte(buf)); n > 0 {
						held = buf[len(buf)-n:]
						buf = buf[:len(buf)-n]
					}
					out.WriteString(buf)
				}
				out.WriteString(held)

				if out.String() != stream {
					t.Fatalf("split at %d did not reassemble: got %q, want %q", split, out.String(), stream)
				}
			}
		})
	}
}
