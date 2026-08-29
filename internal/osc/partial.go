package osc

import "bytes"

// PartialMarkerLen reports how many trailing bytes of buf belong to an OSC 133 sequence that has not
// finished, and zero when nothing is pending.
//
// It exists so the output pump can hold those bytes back until the rest arrives, which is what makes every
// consumer of a prompt marker see a whole one. Without it RewritePromptRedraw silently skipped a marker
// split by a pty read boundary, and the marker reached the client carrying redraw=1: the client's terminal
// then clears the prompt lines on resize and waits for a repaint that arrives in the pty's coordinates
// rather than the window's, so the prompt is cleared and does not come back. Measured as every split
// strictly inside the marker, a 26-byte window for one carrying parameters.
//
// Distinct from partialPrefixLen next door, which returns everything from the last introducer whether or
// not it is terminated, because its caller re-scans the buffer it holds. Holding a *terminated* sequence
// back would delay a marker that is already complete, so this checks for the terminator, and it scans
// forward rather than backward for the reason recorded in the body.
//
// Bounded by maxPartial for the reason CommandTracker's holdback is: a stream emitting an ESC and then
// megabytes of ordinary text must not grow this without limit. Past the bound the bytes are treated as not
// a marker, which is the safe direction here, since the alternative is withholding session output.
func PartialMarkerLen(buf []byte) int {
	// Scanned forward past every complete sequence rather than searching backwards for the last
	// introducer, which was the first version and was wrong: with a terminated marker followed by the
	// start of another, the backward search found the terminated one, saw its terminator and reported
	// nothing pending, so the second marker's introducer went out and was lost. Caught by sweeping the
	// boundary through a stream carrying two prompts.
	i := 0
	for {
		j := bytes.Index(buf[i:], []byte(commandIntro))
		if j < 0 {
			break
		}
		start := i + j
		end, termLen := oscEnd(buf[start:])
		if end < 0 {
			// Unterminated, so everything from here on belongs to a sequence still arriving.
			if keep := len(buf) - start; keep <= maxPartial {
				return keep
			}
			// Past the bound this is not a marker. Treating it as one would withhold session output
			// indefinitely, which is worse than the rewrite it would recover.
			return 0
		}
		i = start + end + termLen
	}

	// No unfinished sequence, but the tail may be part way through an introducer. A trailing "\x1b]13" is
	// the start of a marker whose remaining bytes are in the next read, and cutting there is what loses the
	// rewrite.
	tail := buf[i:]
	for n := min(len(commandIntro)-1, len(tail)); n > 0; n-- {
		if bytes.HasSuffix(tail, []byte(commandIntro[:n])) {
			return n
		}
	}
	return 0
}
