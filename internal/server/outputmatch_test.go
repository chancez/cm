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

// A skipped echo is not matched against.
//
// The bug this closes was measured, not hypothetical: writing to a pty makes the shell echo the line back,
// and the echo contains the command, so `send 'sh -c "sleep 2; echo UNIQUEWORD"' --match UNIQUEWORD`
// resolved in 11ms against the echo while the real output arrived 2s later. Same class of wrong answer as a
// wait for idle satisfied by the idle a session was already in.
func TestOutputMatcherSkipsEcho(t *testing.T) {
	const echoed = "sh -c \"sleep 2; echo UNIQUEWORD\"\r\n"

	m := newOutputMatcher("UNIQUEWORD", false)
	// Counted in text bytes, which is what the budget consumes: the carriage return is stripped, so the
	// echo is one byte shorter than it was written. A caller passes the raw length and over-shoots by
	// exactly that, which is the trade the budget accepts.
	m.skipEcho(len(echoed) - 1)

	// The echo itself must not satisfy it, even though the pattern is plainly in there.
	if m.feed([]byte(echoed)) {
		t.Fatal("matched the echoed command line, want the echo skipped")
	}
	// The real output does.
	if !m.feed([]byte("UNIQUEWORD\r\n")) {
		t.Error("did not match the command's own output after skipping the echo")
	}
}

// An echo split across chunks is skipped in full.
func TestOutputMatcherSkipsEchoAcrossChunks(t *testing.T) {
	const echoed = "echo NEEDLE\r\n"
	for cut := 1; cut < len(echoed); cut++ {
		m := newOutputMatcher("NEEDLE", false)
		m.skipEcho(len(echoed) - 1)

		if m.feed([]byte(echoed[:cut])) {
			t.Errorf("split at %d: matched inside the echo", cut)
			continue
		}
		if m.feed([]byte(echoed[cut:])) {
			t.Errorf("split at %d: matched the rest of the echo", cut)
			continue
		}
		if !m.feed([]byte("NEEDLE\r\n")) {
			t.Errorf("split at %d: did not match the output after the echo", cut)
		}
	}
}

// The skip is a budget in text bytes, and running past the echo swallows real output.
//
// Recorded as a limit rather than papered over, since it is the cost of counting bytes instead of matching
// the echo as a string. Two things make it acceptable: the count comes from the input just written, so it
// over-shoots only by what a terminal adds or removes -- a stripped carriage return, a wrap -- and a
// pattern a caller waits for arrives in the output *after* that, not within a few bytes of the prompt.
//
// It is a real edge though: a skip much larger than the echo eats output that follows. This test pins the
// arithmetic so a later change to how the budget is counted is visible rather than silent.
func TestOutputMatcherSkipIsAByteBudget(t *testing.T) {
	m := newOutputMatcher("TARGET", false)
	// Deliberately far more than the echo below.
	m.skipEcho(100)

	// 12 raw bytes, but the carriage return is stripped, so 11 count against the budget.
	if m.feed([]byte("short echo\r\n")) {
		t.Fatal("matched during the skip")
	}
	// Enough to exhaust the remaining 89.
	if m.feed([]byte(strings.Repeat("x", 89))) {
		t.Fatal("matched during the skip")
	}
	if !m.feed([]byte("TARGET\r\n")) {
		t.Error("did not match once the budget was exhausted")
	}
}

// A bare wait skips nothing, so the first output it sees can match.
func TestOutputMatcherWithoutSkip(t *testing.T) {
	m := newOutputMatcher("FIRST", false)
	if !m.feed([]byte("FIRST\r\n")) {
		t.Error("did not match the first chunk when no echo was skipped")
	}
}
