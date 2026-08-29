package ansi

// Tracker follows a stream of terminal output and reports whether it currently sits inside an
// unterminated escape sequence.
//
// This exists because cm has more than one thing to say to a client's terminal: the session's output,
// and bytes cm generates itself -- a window title, a proxied query, an outage notice. The session's
// output arrives in chunks bounded by a pty read, so a chunk boundary lands inside an escape sequence
// routinely: measured at 6 to 8 of the roughly 90 writes in one nvim repaint. Writing cm's own bytes
// at such a boundary splits the program's sequence in two, and the terminal then discards the front
// and prints the rest as text.
//
// That was a real bug, not a hypothetical. A window title written from OnMetadata landed inside
// `ESC [ 38:2:232`, so the terminal received `ESC [ 38:2:232 ESC ] 2;nvim ... BEL :102:113m` and
// printed `:102:113m` on screen. The stray text shifted the line, the line shifted the screen, and
// every cell nvim did not happen to repaint afterwards kept stale content until a ctrl-l.
//
// Deliberately only a tracker, not a filter. Stripper next door has a state machine of its own that
// looks similar, and the two are not shared for one reason: Stripper's machine also decides what to
// drop, so folding them together would mean changing what `cm read --follow` emits. Worth doing later
// on purpose rather than as a side effect of a bug fix. This one also handles the string controls
// Stripper does not -- DCS, APC, PM, SOS -- because cm's own query proxy writes a DCS and a program
// that emits one would otherwise look like it had finished a sequence it was still inside.
type Tracker struct {
	state trackState
	// inside counts the bytes consumed since the current sequence opened, so an unterminated one
	// cannot make InSequence true forever. See maxPending.
	inside int
}

// trackState is what the tracker is in the middle of, if anything.
//
// Its own type rather than Stripper's `state`, so adding the string-control states here cannot reach
// a switch over there that does not handle them.
type trackState int

const (
	// trackText is ordinary output, and the only state in which nothing is half-written.
	trackText trackState = iota
	// trackEscape is just after ESC, deciding what kind of sequence this is.
	trackEscape
	// trackCSI is inside a control sequence, which ends at a byte in 0x40..0x7e.
	trackCSI
	// trackString is inside a string control -- OSC, DCS, APC, PM, SOS -- which ends at BEL or ST.
	trackString
	// trackStringEscape is inside a string control having just seen ESC, which may begin ST.
	trackStringEscape
)

// Feed consumes output bytes, advancing the tracker.
func (t *Tracker) Feed(p []byte) {
	for _, b := range p {
		switch t.state {
		case trackText:
			if b == 0x1b {
				t.state = trackEscape
				t.inside = 1
				continue
			}
			// The 8-bit C1 forms of these controls (0x90 for DCS, 0x9d for OSC) are deliberately not
			// recognized. In a UTF-8 stream those bytes are continuation bytes of a multi-byte rune, so
			// treating them as controls would report a sequence in the middle of ordinary text.

		case trackEscape:
			t.inside++
			switch {
			case b == '[':
				t.state = trackCSI
			case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
				// OSC, DCS, SOS, PM, APC. All run until BEL or ST.
				t.state = trackString
			case b >= 0x20 && b <= 0x2f:
				// An intermediate byte, so the sequence continues. ESC ( B is the common one.
			default:
				// A final byte, so this was a short sequence such as ESC c or ESC =.
				t.state = trackText
				t.inside = 0
			}

		case trackCSI:
			t.inside++
			if b >= 0x40 && b <= 0x7e {
				t.state = trackText
				t.inside = 0
			}

		case trackString:
			t.inside++
			switch b {
			case 0x07:
				t.state = trackText
				t.inside = 0
			case 0x1b:
				t.state = trackStringEscape
			}

		case trackStringEscape:
			t.inside++
			if b == '\\' {
				t.state = trackText
				t.inside = 0
			} else {
				// Not ST after all. A real terminal treats a bare ESC inside a string control as an
				// abort, but "still inside" is the conservative answer for this question: the caller is
				// deciding whether it is safe to interrupt, and reporting safe when it is not is the
				// failure that matters.
				t.state = trackString
			}
		}

		// An unterminated sequence must not hold the stream hostage. Past this the likeliest explanation
		// is that the bytes are not what they claim to be, and a caller waiting for a boundary that never
		// comes would never speak again. Same bound and same reasoning as Stripper's maxPending.
		if t.inside > maxPending {
			t.state = trackText
			t.inside = 0
		}
	}
}

// InSequence reports whether the stream is mid-sequence, so writing anything else now would split it.
func (t *Tracker) InSequence() bool { return t.state != trackText }
