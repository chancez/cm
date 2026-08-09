package server

import (
	"bytes"
	"strings"

	"github.com/chancez/cm/internal/ansi"
)

// outputMatcher reports when a pattern appears in a session's output.
//
// Fed the same chunks the log numbers, in order, and stateful for two reasons that a caller cannot work
// around by scanning each chunk itself.
//
// A match can straddle a chunk boundary. A pty read is bounded by the kernel buffer rather than by
// anything the program intends, so "DONE" arrives as "DO" and "NE" often enough to matter, and a
// per-chunk scan misses it silently -- which for a wait means burning the whole timeout on output that
// already contained what was asked for.
//
// And escape sequences sit between the characters. A program that colours its output writes
// "DO\x1b[0mNE", so matching the bytes finds nothing while a person looking at the screen sees DONE.
// Stripping is therefore the default rather than an option, and it has to be stateful too: an escape can
// itself be split across chunks, which is what ansi.Stripper already handles.
//
// Built as its own type rather than inlined into the wait so that `cm watch` can consume the same
// evaluation. The two want the same question answered at the same moment and differ only in what they do
// with the answer, so a second implementation would be a second set of these bugs.
type outputMatcher struct {
	// pattern is the text being looked for.
	pattern string
	// raw skips stripping, so the pattern is matched against the bytes the program emitted.
	//
	// For the case where the escape sequences are the point. It changes whether a pattern can match at
	// all rather than merely how output looks, which is why it is a modifier a caller sets deliberately.
	raw bool
	// stripped accumulates text with escape sequences removed, when raw is false.
	stripped bytes.Buffer
	// filter writes into stripped, and holds any partial escape sequence across chunks.
	filter *ansi.Stripper
	// tail retains the last pattern-length-1 bytes of text already examined, so a match spanning a
	// chunk boundary is still found.
	//
	// Only that much is kept: a match needs at most the previous len(pattern)-1 bytes to complete, so
	// retaining more would grow without bound for a session that prints for days without matching.
	tail []byte
	// found records that the pattern has appeared, which is latched: a caller asks "has this happened",
	// and output arriving afterwards cannot unhappen it.
	found bool
	// skip is text still to be discarded before matching begins, used to step over a shell's echo of the
	// input that was just sent.
	//
	// Necessary because writing to a pty means the shell echoes the line back, and that echo contains the
	// command -- so a pattern naming anything in the command matches the echo rather than the output.
	// Measured: `send 'sh -c "sleep 2; echo UNIQUEWORD"' --match UNIQUEWORD` resolved in 11ms against the
	// echo while the real output arrived 2s later, which is the same class of wrong answer as a wait for
	// idle being satisfied by the idle a session was already in.
	//
	// Counted in bytes of text rather than matched as a string, because a terminal does not echo verbatim:
	// it may wrap, and a shell with line editing redraws the line as it goes. A byte count is robust to
	// both, and over-skipping by a few characters costs nothing here since the pattern a caller waits for
	// is in the output that follows.
	skip int
}

// skipEcho tells the matcher to discard n bytes of text before matching.
//
// Set by a send, which knows how much input it wrote, and not by a bare wait, which caused no echo.
func (m *outputMatcher) skipEcho(n int) { m.skip = n }

// newOutputMatcher builds a matcher for a pattern.
//
// An empty pattern is rejected by the caller rather than treated as matching everything, since a wait
// satisfied immediately by a pattern nobody meant to set is indistinguishable from a wait that worked.
func newOutputMatcher(pattern string, raw bool) *outputMatcher {
	m := &outputMatcher{pattern: pattern, raw: raw}
	if !raw {
		m.filter = ansi.NewStripper(&m.stripped)
	}
	return m
}

// feed consumes a chunk of output and reports whether the pattern has now been seen.
//
// Reports the latched value rather than "this chunk matched", so a caller polling after the fact gets the
// same answer as one that saw the chunk.
func (m *outputMatcher) feed(p []byte) bool {
	if m.found || len(p) == 0 {
		return m.found
	}

	text := p
	if !m.raw {
		// Errors are impossible here: the destination is a bytes.Buffer, whose Write never fails.
		m.stripped.Reset()
		_, _ = m.filter.Write(p)
		text = m.stripped.Bytes()
		if len(text) == 0 {
			// The chunk was entirely escape sequences, so there is no new text to examine and the tail
			// is unchanged. Returning early keeps a repainting program from clearing the tail and
			// breaking a match that spans the repaint.
			return m.found
		}
	}

	// Step over the echo before matching anything.
	//
	// Applied to the text rather than the raw bytes so a wrapped or redrawn echo still consumes the
	// intended amount, and the tail is left untouched: there is nothing before an echo worth completing a
	// match against.
	if m.skip > 0 {
		if len(text) <= m.skip {
			m.skip -= len(text)
			return false
		}
		text = text[m.skip:]
		m.skip = 0
	}

	// Prepended rather than searched separately, so a match that begins in the previous chunk and ends
	// in this one is found by a single scan.
	window := text
	if len(m.tail) > 0 {
		window = append(append(make([]byte, 0, len(m.tail)+len(text)), m.tail...), text...)
	}

	if m.contains(window) {
		m.found = true
		return true
	}

	// Retain just enough to complete a match next time.
	keep := len(m.pattern) - 1
	if keep > len(window) {
		keep = len(window)
	}
	if keep < 0 {
		keep = 0
	}
	m.tail = append(m.tail[:0], window[len(window)-keep:]...)
	return false
}

// contains reports whether the window holds the pattern.
func (m *outputMatcher) contains(window []byte) bool {
	return strings.Contains(string(window), m.pattern)
}

// matched reports whether the pattern has been seen.
func (m *outputMatcher) matched() bool { return m.found }
