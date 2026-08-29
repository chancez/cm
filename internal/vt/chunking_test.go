package vt

import (
	"fmt"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/ansi"
)

// tuiStream is output shaped like what a real full-screen program emits, built from primitives rather
// than recorded, so it is readable, deterministic, and committed as code rather than as a blob.
//
// Every element is one that has caused a bug in cm: colon-separated truecolor SGR, absolute cursor
// positioning, the alternate screen, synchronized-update brackets, a DCS, an APC, an OSC carrying a
// path, wide runes, and a line that ends exactly at the right margin, which is where a terminal's
// deferred-wrap state decides whether the next byte scrolls the screen.
func tuiStream(cols int) string {
	var b strings.Builder
	b.WriteString("\x1b[?1049h\x1b[H\x1b[2J")     // alternate screen, home, clear
	b.WriteString("\x1b[?2026h")                  // begin synchronized update
	b.WriteString("\x1b]2;chunking\x07")          // a title
	b.WriteString("\x1b]7;file://host/tmp\x1b\\") // cwd, ST terminated
	for row := 1; row <= 6; row++ {
		fmt.Fprintf(&b, "\x1b[%d;1H", row)
		fmt.Fprintf(&b, "\x1b[38:2:%d:%d:%dm", row*20, 100+row, 200-row)
		fmt.Fprintf(&b, "\x1b[48:2:40:44:52m")
		fmt.Fprintf(&b, "%3d ", row)
		b.WriteString("\x1b(B\x1b[m")
		// A row of content, then padding to exactly the right margin so the following write starts in the
		// deferred-wrap state.
		line := fmt.Sprintf("row %d with a wide rune ⭐ and a combining é", row)
		b.WriteString(line)
		if pad := cols - len([]rune(line)) - 4; pad > 0 {
			b.WriteString(strings.Repeat("-", pad))
		}
	}
	b.WriteString("\x1bP+q4D73\x1b\\")     // XTGETTCAP, a DCS
	b.WriteString("\x1b_Gf=100;abc\x1b\\") // kitty graphics, an APC
	b.WriteString("\x1b[?2026l")           // end synchronized update
	b.WriteString("\x1b[7;1H\x1b[Kstatus line")
	return b.String()
}

// TestModelIsChunkingInvariant sweeps the split point across a realistic stream and asserts the screen
// is the same however the bytes arrived.
//
// This is the property that most of cm's escape-sequence bugs violate. A pty read ends wherever the
// kernel buffer did, and the server, the shim, and the client each re-chunk the stream on the way
// through, so the same output is split differently on every run. A model that is correct on whole
// sequences and wrong on split ones is correct in a table test and wrong in production, which is the
// exact profile of the bugs in docs/architecture.md.
//
// Sweeping every offset rather than sampling a few sizes: the interesting splits are inside a sequence,
// and which offsets those are depends on the fixture, so enumerating is both simpler and stronger than
// guessing. 1 to len(stream) is a few thousand renders and runs in well under a second.
func TestModelIsChunkingInvariant(t *testing.T) {
	const rows, cols = 24, 80
	stream := tuiStream(cols)

	whole := renderChunked(t, rows, cols, stream, len(stream))

	for size := 1; size <= len(stream); size++ {
		got := renderChunked(t, rows, cols, stream, size)
		if got != whole {
			t.Fatalf("the screen depends on how the stream was chunked.\n"+
				"fed in %d-byte writes the screen is:\n%s\nfed whole it is:\n%s",
				size, got, whole)
		}
	}
}

// TestModelIsChunkingInvariantAtEveryBoundary splits the stream in exactly two pieces at every possible
// point, which is the shape a single pty read boundary actually has.
//
// Distinct from the sweep above: uniform chunks of size n never produce a lone split at an arbitrary
// offset, so the two cover different sets. This one is what reproduces "the boundary landed here".
func TestModelIsChunkingInvariantAtEveryBoundary(t *testing.T) {
	const rows, cols = 24, 80
	stream := tuiStream(cols)
	whole := renderChunked(t, rows, cols, stream, len(stream))

	for at := 1; at < len(stream); at++ {
		term, err := NewSessionTerminal(rows, cols, 100)
		if err != nil {
			t.Fatal(err)
		}
		if err := term.Write([]byte(stream[:at])); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		if err := term.Write([]byte(stream[at:])); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		out, err := term.Tail(rows, false)
		if err != nil {
			t.Fatalf("Tail() error = %v", err)
		}
		term.Close()
		if string(out) != whole {
			t.Fatalf("the screen depends on where the single write boundary fell.\n"+
				"split at byte %d of %d the screen is:\n%s\nfed whole it is:\n%s",
				at, len(stream), out, whole)
		}
	}
}

// renderChunked feeds a stream in fixed-size writes and returns the resulting screen.
func renderChunked(t *testing.T, rows, cols uint16, stream string, size int) string {
	t.Helper()
	term, err := NewSessionTerminal(rows, cols, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()

	for off := 0; off < len(stream); off += size {
		end := min(off+size, len(stream))
		if err := term.Write([]byte(stream[off:end])); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}
	out, err := term.Tail(int(rows), false)
	if err != nil {
		t.Fatalf("Tail() error = %v", err)
	}
	return string(out)
}

// TestChunkingFixtureSplitsSequences is the control for the two tests above.
//
// Both of them pass trivially if the fixture contains no escape sequences, or if every boundary happens
// to fall between them: they would then be sweeping splits through plain text and proving nothing. A
// test that cannot fail is worse than no test, so this asserts the fixture really does put boundaries
// inside sequences, and says how many.
//
// The threshold is a floor rather than an exact count, so editing the fixture to add a shape does not
// break this, while emptying it of sequences does.
func TestChunkingFixtureSplitsSequences(t *testing.T) {
	stream := tuiStream(80)

	inside := 0
	for at := 1; at < len(stream); at++ {
		var tr ansi.Tracker
		tr.Feed([]byte(stream[:at]))
		if tr.InSequence() {
			inside++
		}
	}

	// Measured at 335 of 884 offsets for the current fixture. A floor well below that catches an edit
	// that removes the sequences without being brittle about the exact shape.
	const floor = 100
	if inside < floor {
		t.Fatalf("only %d of %d split points fall inside an escape sequence, want at least %d: the "+
			"chunking tests would be sweeping through plain text and proving nothing",
			inside, len(stream)-1, floor)
	}
	t.Logf("%d of %d split points fall inside an escape sequence", inside, len(stream)-1)
}
