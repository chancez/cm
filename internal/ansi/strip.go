// Package ansi strips terminal escape sequences from a byte stream.
//
// Separate from internal/vt, which models a terminal: that needs the whole screen and produces its contents,
// which is right for `cm read` and impossible for a stream. Following a session cannot re-render a screen per
// byte, so this is a filter rather than a model.
//
// The consequence is worth stating. Output from a program that repaints in place -- a progress bar, a
// full-screen TUI -- only makes sense with its cursor movements, so stripping them leaves every frame
// concatenated rather than overwritten. Nor can a cursor move be applied: dropping a backspace removes the
// stray byte but not the character it would have erased, since there is no screen to erase from. That is
// acceptable for the case this serves, which is watching a build or a test run print lines, and it is why the
// raw form stays available and why `cm read` renders properly.
package ansi

import (
	"bytes"
	"io"
)

// Stripper filters escape sequences out of a stream, writing the text through.
//
// Stateful because a sequence can be split across writes: a stream delivers whatever has arrived, so an escape
// can be the last byte of one chunk and its terminator can come in the next. A stateless filter would pass the
// tail of a split sequence through as text, which is the bug this exists to avoid.
type Stripper struct {
	w io.Writer
	// state is what the filter is in the middle of, if anything.
	state state
	// pending holds an incomplete sequence's bytes, so a terminator arriving in a later write is recognized.
	//
	// Bounded by maxPending: a stream that opens an escape and never terminates it would otherwise grow this
	// without limit, so past that the bytes are treated as text rather than held forever.
	pending []byte
}

type state int

const (
	// stateText is ordinary output.
	stateText state = iota
	// stateEscape is just after ESC, deciding what kind of sequence this is.
	stateEscape
	// stateCSI is inside a control sequence, which ends at a byte in 0x40..0x7e.
	stateCSI
	// stateOSC is inside an operating system command, which ends at BEL or ST.
	stateOSC
	// stateOSCEscape is inside an OSC and has just seen ESC, which may begin the ST terminator.
	stateOSCEscape
)

// maxPending bounds an unterminated sequence.
//
// A real sequence is a handful of bytes; the longest cm emits is an OSC 7 carrying a path, which is bounded by
// the path. 4 KiB is far beyond any of them, and past it the likeliest explanation is that the stream is not
// what it claims to be, in which case showing the bytes beats swallowing them.
const maxPending = 4096

// NewStripper returns a Stripper writing to w.
func NewStripper(w io.Writer) *Stripper {
	return &Stripper{w: w}
}

// Write filters p and writes the text through.
//
// Always reports len(p) consumed on success, as an io.Writer must: the caller wrote those bytes, and a short
// count would be read as an error even though the missing bytes were deliberately dropped.
func (s *Stripper) Write(p []byte) (int, error) {
	// Built into one buffer and written once rather than per byte, since the caller may be a socket.
	var out bytes.Buffer
	out.Grow(len(p))

	for _, b := range p {
		switch s.state {
		case stateText:
			switch b {
			case 0x1b: // ESC
				s.state = stateEscape
				s.pending = append(s.pending[:0], b)
			case '\r', '\b', 0x07:
			// Dropped, for the same reason as the escape sequences: these move the cursor or ring a bell
			// rather than carrying content, and there is no screen here to move a cursor on.
			//
			// CR because a pty translates newline to CRLF, so keeping it would give a redirected file
			// Windows line endings for output that never meant to have them. Backspace because a line
			// editor emits it to redraw a prompt, which showed up as a stray "p\b" at the start of
			// followed output. BEL because a bell in a log file is nothing but a stray byte.
			//
			// Tab is deliberately not in this list: it is layout a program chose, and columns in a build
			// log matter.
			default:
				out.WriteByte(b)
			}

		case stateEscape:
			s.pending = append(s.pending, b)
			switch {
			case b == '[':
				s.state = stateCSI
			case b == ']':
				s.state = stateOSC
			case b >= 0x20 && b <= 0x2f:
				// An intermediate byte, so the sequence continues. Stays in this state, which is enough for
				// the two-and-three-byte sequences that appear in practice.
			default:
				// A final byte, so this was a short sequence such as ESC c or ESC =. Done.
				s.state = stateText
				s.pending = s.pending[:0]
			}

		case stateCSI:
			s.pending = append(s.pending, b)
			// Parameters and intermediates are 0x20..0x3f; anything in 0x40..0x7e ends the sequence.
			if b >= 0x40 && b <= 0x7e {
				s.state = stateText
				s.pending = s.pending[:0]
			}

		case stateOSC:
			s.pending = append(s.pending, b)
			switch b {
			case 0x07: // BEL, the common terminator
				s.state = stateText
				s.pending = s.pending[:0]
			case 0x1b: // possibly ST, which is ESC backslash
				s.state = stateOSCEscape
			}

		case stateOSCEscape:
			s.pending = append(s.pending, b)
			if b == '\\' {
				s.state = stateText
				s.pending = s.pending[:0]
			} else {
				// Not ST after all, so still inside the OSC.
				s.state = stateOSC
			}
		}

		// An unterminated sequence is not held forever. Emitting what was collected is the honest failure:
		// dropping it would silently lose output, which is worse than showing bytes that look odd.
		if len(s.pending) > maxPending {
			out.Write(s.pending)
			s.pending = s.pending[:0]
			s.state = stateText
		}
	}

	if out.Len() > 0 {
		if _, err := s.w.Write(out.Bytes()); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Strip filters a complete buffer, for callers that have all the bytes at once.
func Strip(p []byte) []byte {
	var out bytes.Buffer
	// The error is from the buffer, which does not fail.
	_, _ = NewStripper(&out).Write(p)
	return out.Bytes()
}
