package ansi

// PartialTailLen reports how many trailing bytes of buf belong to an escape sequence that has not finished,
// and zero when buf ends at a boundary.
//
// It exists so the output pump can hold those bytes until the rest arrives, which is what lets every scanner
// downstream assume it is looking at whole sequences. Several did not, and each one failed differently:
//
//   - RewritePromptRedraw skipped a prompt marker it received in pieces, so the marker reached the client
//     with redraw=1 and the client's terminal cleared the prompt on the next resize and waited for a repaint
//     that arrives in the wrong coordinates.
//   - noteQueries did not record a terminal-only query it received in pieces. The stream is forwarded
//     verbatim, so the client's terminal answered anyway, and answerFromClient discarded the reply as
//     unsolicited because nothing was outstanding. The program that asked waits forever. `wallfacer -h`
//     blocking on OSC 11 is the recorded shape of this.
//
// Both were the same bug in two places, which is the argument for fixing it once here rather than adding a
// holdback to each scanner. Tracker is already the only escape-sequence state machine in cm, so it is the
// only thing that should be deciding where a sequence ends.
//
// A fresh Tracker per call rather than one carried across chunks, because the caller prepends whatever was
// held last time: the buffer always starts at a boundary, so there is no state to carry. That also means a
// caller that stops prepending gets a wrong answer, which is why this is a free function taking the whole
// buffer rather than a method that could be fed piecemeal.
func PartialTailLen(buf []byte) int {
	var t Tracker
	t.Feed(buf)
	return int(t.Fed() - t.Boundary())
}
