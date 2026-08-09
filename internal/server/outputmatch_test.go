package server

import (
	"strings"
	"testing"
)

// A pattern in a single chunk is found.
func TestOutputMatcherSingleChunk(t *testing.T) {
	m := newOutputMatcher("DONE", false)
	if m.feed([]byte("working\n")) {
		t.Error("matched before the pattern appeared")
	}
	if !m.feed([]byte("all DONE here\n")) {
		t.Error("did not match a pattern present in one chunk")
	}
	// Latched: output afterwards cannot unmatch it.
	if !m.feed([]byte("more output\n")) {
		t.Error("the match was not latched")
	}
	if !m.matched() {
		t.Error("matched() = false after a match")
	}
}

// A pattern split across chunks is found.
//
// The failure this prevents is silent: a pty read is bounded by the kernel buffer, so "DONE" arrives as
// "DO" then "NE" often enough to matter, and a per-chunk scan misses it while burning the caller's whole
// timeout on output that already contained what was asked for.
//
// Every split point is checked, so no single lucky boundary passes for the wrong reason.
func TestOutputMatcherSplitAcrossChunks(t *testing.T) {
	const full = "before DONE after"
	for cut := 1; cut < len(full); cut++ {
		m := newOutputMatcher("DONE", false)
		first := m.feed([]byte(full[:cut]))
		second := m.feed([]byte(full[cut:]))
		if !first && !second {
			t.Errorf("split at %d: never matched %q fed as %q + %q",
				cut, full, full[:cut], full[cut:])
		}
	}
}

// A byte-at-a-time feed is the worst case and must still match.
func TestOutputMatcherOneByteAtATime(t *testing.T) {
	m := newOutputMatcher("DONE", false)
	var matched bool
	for _, b := range []byte("xx DONE xx") {
		if m.feed([]byte{b}) {
			matched = true
		}
	}
	if !matched {
		t.Error("did not match when fed one byte at a time")
	}
}

// Escape sequences between the characters do not prevent a match.
//
// A program that colours its output writes "DO\x1b[0mNE", so matching raw bytes finds nothing while a
// person looking at the screen plainly sees DONE. Stripping is the default for exactly this.
func TestOutputMatcherIgnoresEscapeSequences(t *testing.T) {
	m := newOutputMatcher("DONE", false)
	if !m.feed([]byte("\x1b[32mDO\x1b[0mNE\x1b[0m\n")) {
		t.Error("did not match a pattern interrupted by escape sequences")
	}
}

// An escape sequence split across chunks does not break stripping.
//
// ansi.Stripper is stateful for this reason, and the matcher has to keep one instance rather than
// stripping each chunk independently: a filter recreated per chunk would pass the tail of a split
// sequence through as text and could match on it.
func TestOutputMatcherHandlesSplitEscapeSequences(t *testing.T) {
	full := "\x1b[32mDONE\x1b[0m"
	for cut := 1; cut < len(full); cut++ {
		m := newOutputMatcher("DONE", false)
		a := m.feed([]byte(full[:cut]))
		b := m.feed([]byte(full[cut:]))
		if !a && !b {
			t.Errorf("split at %d: did not match %q", cut, full)
		}
	}
}

// A chunk of nothing but escape sequences must not clear the retained tail.
//
// A repainting program emits cursor moves between the characters it prints, so a match spanning a repaint
// would be lost if each such chunk reset what had been seen.
func TestOutputMatcherKeepsTailAcrossEscapeOnlyChunks(t *testing.T) {
	m := newOutputMatcher("DONE", false)
	if m.feed([]byte("DO")) {
		t.Fatal("matched too early")
	}
	// Entirely escape sequences, producing no text.
	if m.feed([]byte("\x1b[0m\x1b[1;1H")) {
		t.Fatal("matched on escape sequences alone")
	}
	if !m.feed([]byte("NE")) {
		t.Error("did not match across a chunk that held only escape sequences")
	}
}

// --raw matches the bytes as the program emitted them.
//
// For the case where the sequences are the point. It also means a pattern that stripping would have
// joined no longer matches, which is the trade a caller accepts by asking for raw.
func TestOutputMatcherRaw(t *testing.T) {
	// Present in the raw bytes, absent from the rendered text.
	m := newOutputMatcher("\x1b[32m", true)
	if !m.feed([]byte("\x1b[32mgreen\x1b[0m")) {
		t.Error("raw matcher did not find an escape sequence")
	}

	// And the inverse: a pattern only visible after stripping is not found in raw mode.
	raw := newOutputMatcher("DONE", true)
	if raw.feed([]byte("DO\x1b[0mNE")) {
		t.Error("raw matcher matched across an escape sequence, want it to see the bytes")
	}
	// The same input does match with stripping, which is what makes the two modes meaningfully different.
	rendered := newOutputMatcher("DONE", false)
	if !rendered.feed([]byte("DO\x1b[0mNE")) {
		t.Error("rendered matcher did not match across an escape sequence")
	}
}

// The retained tail is bounded, so a session printing for days does not grow it.
func TestOutputMatcherTailIsBounded(t *testing.T) {
	m := newOutputMatcher("NEEDLE", false)
	for range 500 {
		m.feed([]byte(strings.Repeat("x", 100)))
	}
	if m.matched() {
		t.Fatal("matched output that does not contain the pattern")
	}
	// At most len(pattern)-1 bytes are needed to complete a match next time.
	if len(m.tail) >= len("NEEDLE") {
		t.Errorf("tail is %d bytes, want fewer than the pattern's %d",
			len(m.tail), len("NEEDLE"))
	}
}

// A pattern longer than any single chunk still matches once enough has arrived.
func TestOutputMatcherPatternLongerThanChunks(t *testing.T) {
	m := newOutputMatcher("abcdefghij", false)
	var matched bool
	for _, part := range []string{"ab", "cd", "ef", "gh", "ij"} {
		if m.feed([]byte(part)) {
			matched = true
		}
	}
	if !matched {
		t.Error("did not match a pattern spread across five chunks")
	}
}

// An empty chunk changes nothing.
func TestOutputMatcherEmptyChunk(t *testing.T) {
	m := newOutputMatcher("DONE", false)
	if m.feed(nil) || m.feed([]byte{}) {
		t.Error("an empty chunk reported a match")
	}
	if !m.feed([]byte("DONE")) {
		t.Error("did not match after empty chunks")
	}
}

// Matching is case-sensitive, which is what a plain substring means.
//
// Stated as a test rather than left implicit, since the alternative is a reasonable thing to expect and
// this is the behavior a caller has to write their pattern against.
func TestOutputMatcherIsCaseSensitive(t *testing.T) {
	m := newOutputMatcher("DONE", false)
	if m.feed([]byte("done\n")) {
		t.Error("matched a different case, want a plain substring match")
	}
	if !m.feed([]byte("DONE\n")) {
		t.Error("did not match the exact case")
	}
}
