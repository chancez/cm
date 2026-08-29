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
// This is the only escape-sequence state machine in cm, and Stripper next door drives it too. It did
// not start that way: Stripper had a near-copy, and the copy was missing the string controls -- DCS,
// APC, PM, SOS -- so it treated `ESC P` as a complete two-byte sequence and emitted everything after
// it as text. Measured before the fix, `Strip("a\x1bP+q4D73\x1b\\b")` returned `"a+q4D73b"`, which is
// cm's own XTGETTCAP query leaking into what `cm read --follow` hands an agent. Two machines meant one
// of them was wrong and nothing said which.
type Tracker struct {
	state state
	// inside counts the bytes consumed since the current sequence opened, so an unterminated one
	// cannot make InSequence true forever. See maxPending.
	inside int
	// fed is how many bytes have passed through, and boundary is the value fed had the last time the
	// stream was not mid-sequence.
	//
	// Positions rather than a bare flag, because one caller needs to know *where* the last safe point was
	// rather than whether it is at one. Attaching a client replays a serialized screen and then streams
	// from where the model stopped, and a screen cannot express "a half-parsed CSI is pending": those
	// bytes are in neither the snapshot nor the stream, so the client received `:2:3m` with the
	// `ESC [ 38:2:1` that opened it missing. Resuming at the boundary replays the partial sequence
	// instead, which the client completes. See Session.attach.
	fed      int64
	boundary int64
}

// state is what the stream is in the middle of, if anything.
type state int

const (
	// stateText is ordinary output, and the only state in which nothing is half-written.
	stateText state = iota
	// stateEscape is just after ESC, deciding what kind of sequence this is.
	stateEscape
	// stateCSI is inside a control sequence, which ends at a byte in 0x40..0x7e.
	stateCSI
	// stateString is inside a string control -- OSC, DCS, APC, PM, SOS -- which ends at BEL or ST.
	//
	// One state for all five rather than one each, because nothing here needs to tell them apart: they
	// share a terminator, and the question asked of this machine is only whether a sequence is open.
	stateString
	// stateStringEscape is inside a string control having just seen ESC, which may begin ST.
	stateStringEscape
)

// Feed consumes output bytes, advancing the tracker.
func (t *Tracker) Feed(p []byte) {
	for _, b := range p {
		t.feedByte(b)
		t.fed++
		if t.state == stateText {
			t.boundary = t.fed
		}
	}
}

// feedByte advances the tracker by one byte.
//
// Separate from Feed so Stripper, which decides byte by byte whether to emit, does not allocate a
// one-byte slice per byte of the stream it is filtering.
func (t *Tracker) feedByte(b byte) {
	switch t.state {
	case stateText:
		if b == 0x1b {
			t.state = stateEscape
			t.inside = 1
			return
		}
		// The 8-bit C1 forms of these controls (0x90 for DCS, 0x9d for OSC) are deliberately not
		// recognized. In a UTF-8 stream those bytes are continuation bytes of a multi-byte rune, so
		// treating them as controls would report a sequence in the middle of ordinary text.
		return

	case stateEscape:
		t.inside++
		switch {
		case b == '[':
			t.state = stateCSI
		case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
			// OSC, DCS, SOS, PM, APC. All run until BEL or ST. Missing these four is what made
			// Stripper leak a DCS payload as text.
			t.state = stateString
		case b >= 0x20 && b <= 0x2f:
			// An intermediate byte, so the sequence continues. ESC ( B is the common one.
		default:
			// A final byte, so this was a short sequence such as ESC c or ESC =.
			t.state = stateText
			t.inside = 0
		}

	case stateCSI:
		t.inside++
		if b >= 0x40 && b <= 0x7e {
			t.state = stateText
			t.inside = 0
		}

	case stateString:
		t.inside++
		switch b {
		case 0x07:
			t.state = stateText
			t.inside = 0
		case 0x1b:
			t.state = stateStringEscape
		}

	case stateStringEscape:
		t.inside++
		if b == '\\' {
			t.state = stateText
			t.inside = 0
		} else {
			// Not ST after all. A real terminal treats a bare ESC inside a string control as an abort,
			// but "still inside" is the conservative answer for this question: a caller is deciding
			// whether it is safe to interrupt, and reporting safe when it is not is the failure that
			// matters.
			t.state = stateString
		}
	}

	// An unterminated sequence must not hold the stream hostage. Past this the likeliest explanation is
	// that the bytes are not what they claim to be, and a caller waiting for a boundary that never comes
	// would never speak again. Stripper emits what it withheld at the same bound, for the same reason.
	if t.inside > maxPending {
		t.state = stateText
		t.inside = 0
	}
}

// InSequence reports whether the stream is mid-sequence, so writing anything else now would split it.
func (t *Tracker) InSequence() bool { return t.state != stateText }

// Fed is how many bytes have been fed.
func (t *Tracker) Fed() int64 { return t.fed }

// Boundary is how many bytes had been fed at the last point the stream was not mid-sequence.
//
// Equal to Fed when the stream is at a boundary now. Behind it by the length of the partial sequence
// otherwise, so Fed minus Boundary is how many bytes a reader would have to rewind to start somewhere a
// terminal can parse.
//
// Only advanced by Feed, not by feedByte, because Stripper drives feedByte directly and does not count
// bytes: it decides what to emit rather than where it could restart.
func (t *Tracker) Boundary() int64 { return t.boundary }
