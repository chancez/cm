package ansi

import (
	"fmt"
)

// Write is one write to a terminal, and Injected says whether cm generated it rather than the program.
//
// A slice of these is a transcript: the ordered record of what a terminal actually received, which is
// the only view that can settle this class of bug. See Validate.
type Write struct {
	Data     []byte
	Injected bool
}

// Validate checks a transcript against the two rules cm has to obey when talking to a terminal, and
// returns every violation rather than the first.
//
// **A sequence the program wrote must arrive intact.** cm splits the session's output wherever a pty
// read ended, so it may hand a terminal half a sequence; what it must not do is put anything else in
// the gap. This is stated as a rule about the program's sequences rather than about cm's injections on
// purpose: it does not need to know which sequences are cm's, so it catches an injection nobody thought
// to look for, from a writer nobody knew existed. That is the exact failure mode of the bug it comes
// from, where the offending writer bypassed cm's own abstraction and every capture taken inside cm
// missed it.
//
// **Injected bytes must be whole sequences.** Half a title is worse than no title, and an unterminated
// injection would swallow whatever the program wrote next.
//
// Deliberately silent about ordering between injections and session bytes, because both orders are
// legitimate: a title may land before or after a given chunk. Only the overlap is a defect.
func Validate(writes []Write) []error {
	var problems []error

	// One tracker over the stream as the terminal saw it, session bytes and injections together, since
	// that is what a terminal parses.
	var stream Tracker
	// Whether the program's own bytes are currently mid-sequence, tracked separately: that is the state
	// an injection is not allowed to interrupt.
	var session Tracker

	for i, w := range writes {
		if w.Injected {
			if session.InSequence() {
				problems = append(problems, fmt.Errorf(
					"write %d injects %q while the program is mid-sequence: the terminal will abort the "+
						"program's sequence and print its remainder as text", i, w.Data))
			}
			// An injection has to be self-contained, or it leaves the stream open over the next chunk.
			var alone Tracker
			alone.Feed(w.Data)
			if alone.InSequence() {
				problems = append(problems, fmt.Errorf(
					"write %d injects %q, which is an incomplete sequence: whatever the program writes "+
						"next will be swallowed by it", i, w.Data))
			}
			stream.Feed(w.Data)
			continue
		}
		session.Feed(w.Data)
		stream.Feed(w.Data)
	}

	return problems
}

// SessionBytes returns just the program's own bytes from a transcript, in order.
//
// For the other half of the check: that what the terminal received, with cm's own additions removed, is
// exactly what the program wrote. A transcript proves nothing about missing bytes on its own, since it
// only records what was sent; comparing this against the program's intended output is what closes that
// gap.
func SessionBytes(writes []Write) []byte {
	var out []byte
	for _, w := range writes {
		if !w.Injected {
			out = append(out, w.Data...)
		}
	}
	return out
}
