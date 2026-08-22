package graphics

import "bytes"

// Segment is one run of a chunk of output: either ordinary bytes or one graphics command.
//
// Ordered segments rather than "the commands, plus everything else" because the caller has to rebuild the
// stream, and rebuilding needs to know where each command sat. Returning the two separately loses that: a
// chunk of "BEFORE cmd1 MIDDLE cmd2 AFTER" collapses to "BEFOREMIDDLEAFTER" plus two commands, and any
// reassembly then puts both commands at one end.
//
// That was a real corruption rather than a hypothetical. A refused command's payload appeared as text on
// the prompt line, and the probe beside it reached the terminal with no payload at all, which kitty
// reported as "ENODATA: Insufficient image data: 0 < 3".
type Segment struct {
	// Cmd is the command, when Graphics is true.
	Cmd Command
	// Data is ordinary output, when Graphics is false.
	Data []byte
	// Graphics distinguishes the two, rather than testing Data for nil: a command carrying no payload is
	// legitimate, so nil Data is not a reliable signal.
	Graphics bool
}

// Scanner finds graphics commands in a stream of session output.
//
// Stateful because a command can be split across reads. A pty read returns at most 1022 bytes on darwin
// whatever buffer size is passed, measured at 4096 and 65536, so an image transmission is *guaranteed*
// to arrive in pieces rather than occasionally: a stateless matcher would miss every image over that
// size rather than a rare one at a boundary.
//
// Not safe for concurrent use. In cm one output pump feeds one scanner, the same discipline as the OSC
// trackers beside it.
type Scanner struct {
	// held accumulates a command that has not finished arriving.
	//
	// One buffer, grown in place, rather than a fresh slice per chunk. That is closer to correctness
	// than to micro-optimization: a held fragment grows by one chunk on every read for the length of a
	// transmission, so re-copying it each time is quadratic. Measured at 8.6 MB copied to reassemble a
	// 132 KB image, costing 4.175ms against the pump's 460.5us for the same bytes, a 9x regression from
	// the copying alone.
	held []byte
	// segs is the result buffer, reused across calls so a steady stream of images does not allocate one
	// slice per chunk.
	segs []Segment
	// out backs the ordinary-output segments, and must not be held itself.
	//
	// Separate because held is compacted at the end of every Scan: the leftovers are moved to the front
	// of that buffer, which overwrites the bytes any segment pointing into it was returning. That is not
	// hypothetical -- it turned "text" into "\x1b_Gt" the first time this was written without it, since
	// the held introducer was copied over the emitted text.
	out []byte
	// searched is how far into the current command's body the terminator scan has already looked.
	//
	// Without this the scan is quadratic in a second place, and profiling put 92.77% of the time in
	// stringEnd: an image arriving in 129 chunks re-scans the whole accumulated payload on every one of
	// them looking for a terminator that is not there yet. Remembering the position makes each byte
	// examined once.
	searched int
}

// maxHeld bounds an unfinished command.
//
// 1 MiB. What has to fit is one command, not a whole image, since a transmission arrives as many
// commands each carrying a chunk of payload. zellij caps its own interceptor's capture at the same
// figure, which is a useful second opinion on the order of magnitude.
//
// Bounded at all because a program that writes an introducer and never terminates it would otherwise
// buffer the rest of the session.
const maxHeld = 1 << 20

// Reset discards anything held.
//
// Called when a session's output is discontinuous, such as after a gap, where the bytes that would have
// completed a held command are exactly the ones that were lost.
func (s *Scanner) Reset() {
	s.held = s.held[:0]
	s.searched = 0
}

// Pending reports how many bytes are held, for diagnostics and tests.
func (s *Scanner) Pending() int { return len(s.held) }

// Scan splits a chunk of output into ordered segments.
//
// Returns nil when the chunk holds no graphics and nothing was held, which is the overwhelmingly common
// case: nil means "forward the input unchanged", so that path allocates nothing and costs one search for
// the introducer.
//
// Segment data may alias the scanner's buffer, so a caller must use the result before calling Scan again.
func (s *Scanner) Scan(p []byte) []Segment {
	// Fast path. The trailing-prefix check is the part that is easy to omit and wrong to omit, because a
	// chunk ending in ESC or ESC _ holds no complete introducer, and passing it through makes the command
	// unrecognizable once the rest arrives.
	if len(s.held) == 0 && !bytes.Contains(p, []byte(intro)) {
		n := prefixLen(p)
		if n == 0 {
			return nil
		}
		s.held = append(s.held[:0], p[len(p)-n:]...)
		s.segs = s.segs[:0]
		if rest := p[:len(p)-n]; len(rest) > 0 {
			s.segs = append(s.segs, Segment{Data: rest})
		}
		return s.segs
	}

	// Accumulate into the held buffer and scan from there, so reassembly grows one allocation rather
	// than copying the accumulated bytes on every chunk.
	s.held = append(s.held, p...)
	buf := s.held
	s.segs = s.segs[:0]
	s.out = s.out[:0]

	read := 0
	for read < len(buf) {
		i := bytes.Index(buf[read:], []byte(intro))
		if i < 0 {
			// No further command. Emit the rest, less a trailing introducer fragment to hold.
			tail := buf[read:]
			if n := prefixLen(tail); n > 0 {
				tail = tail[:len(tail)-n]
				read = len(buf) - n
			} else {
				read = len(buf)
			}
			if len(tail) > 0 {
				s.segs = append(s.segs, Segment{Data: s.appendOut(tail)})
			}
			break
		}

		if i > 0 {
			s.segs = append(s.segs, Segment{Data: s.appendOut(buf[read : read+i])})
		}
		read += i

		// Resume the terminator search where the last chunk left off, in one scan rather than checking
		// and then locating. Both halves were measured: restarting put 92.77% of the time in stringEnd
		// while reassembling a 129 chunk image, and scanning twice cost 62% on a chunk that completes a
		// command.
		cmd, n, ok := ParseFrom(buf[read:], s.searched)
		if !ok {
			// An introducer whose terminator has not arrived. Everything from here is held, and how much
			// of the *body* was examined is remembered, less one byte, since an ESC at the very end may
			// begin a terminator the next chunk completes.
			//
			// Measured from the body rather than the command: ParseFrom's offset skips the introducer, so
			// counting from the command start overshoots by its three bytes and steps the resume point
			// past a terminator landing exactly there. The split-at-every-offset test caught that at cut
			// 37 rather than it shipping.
			s.searched = max(0, len(buf)-read-len(intro)-1)
			break
		}
		s.segs = append(s.segs, Segment{Cmd: cmd, Graphics: true})
		read += n
		s.searched = 0
	}

	// Whatever was not consumed is held for the next chunk, moved to the front of the same buffer. copy
	// handles the overlap, since the destination never starts after the source.
	remaining := buf[read:]
	if len(remaining) > maxHeld {
		// Dropped rather than truncated: a truncated fragment would parse as a malformed command later,
		// so the bytes would be neither forwarded nor understood. What is lost is a command a real
		// terminal would have drawn, which is the honest cost of a bound and applies only to a program
		// that never terminates its sequence.
		remaining = nil
	}
	n := copy(s.held, remaining)
	s.held = s.held[:n]
	if n == 0 {
		s.searched = 0
	}

	return s.segs
}

// appendOut copies output bytes into the scanner's own buffer and returns the copy.
//
// Necessary because held is compacted at the end of Scan, moving the leftovers to the front and
// overwriting whatever a segment pointing into it was returning. Copying is also why out is grown rather
// than reallocated: a chunk's worth per call, reused.
//
// The returned slice is stable until the next Scan, which is the contract Scan documents. It cannot alias
// out's earlier contents either, since append only ever extends.
func (s *Scanner) appendOut(p []byte) []byte {
	start := len(s.out)
	s.out = append(s.out, p...)
	return s.out[start:len(s.out):len(s.out)]
}
