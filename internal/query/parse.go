package query

// Shared escape-sequence parsing, so the answerable and terminal-only classifiers agree about where a
// sequence begins and ends.
//
// Split out because the two classifiers must never disagree about extent. If one thought a sequence was
// four bytes and the other five, a stream scan would step into the middle of it and misclassify whatever
// followed, which is the class of mistake that makes a query look like ordinary output.

// parseCSI splits a CSI sequence into its parameter bytes, intermediate bytes, and final byte.
//
// Parsed structurally rather than matched against whole strings, because the byte ranges are what
// distinguish sequences that look alike. The parameter range is 0x30-0x3f, which includes the private
// markers ? > = <, and the intermediate range is 0x20-0x2f, which includes the space in the cursor-style
// sequence CSI 2 SP q. Without the intermediate split that sequence reads as the XTVERSION query CSI > q.
//
// ok is false when the sequence is incomplete, so a caller can tell "not yet" from "not a CSI".
func parseCSI(p []byte) (params, inter []byte, final byte, length int, ok bool) {
	if len(p) < 2 || p[0] != 0x1b || p[1] != '[' {
		return nil, nil, 0, 0, false
	}
	i := 2
	for i < len(p) && p[i] >= 0x30 && p[i] <= 0x3f {
		i++
	}
	paramEnd := i
	for i < len(p) && p[i] >= 0x20 && p[i] <= 0x2f {
		i++
	}
	interEnd := i
	if i >= len(p) || p[i] < 0x40 || p[i] > 0x7e {
		return nil, nil, 0, 0, false
	}
	return p[2:paramEnd], p[paramEnd:interEnd], p[i], i + 1, true
}

// dcsEnd reports the length of a DCS sequence, including its terminator.
//
// ST is ESC backslash. A bare BEL also terminates in practice, and accepting it matters because cm's own
// OSC replies use BEL: docs/restore.md records that a real kitty rendered the *following* sequence as
// literal text when an ST-terminated one preceded it, which is why BEL is used there and why anything
// parsing these has to handle both.
func dcsEnd(p []byte) (n int, ok bool) {
	if len(p) < 2 || p[0] != 0x1b || p[1] != 'P' {
		return 0, false
	}
	for i := 2; i < len(p); i++ {
		if p[i] == 0x07 {
			return i + 1, true
		}
		if p[i] == 0x1b && i+1 < len(p) && p[i+1] == '\\' {
			return i + 2, true
		}
	}
	return 0, false
}

// oscBody returns an OSC sequence's length and its payload, excluding introducer and terminator.
func oscBody(p []byte) (n int, body []byte, ok bool) {
	if len(p) < 2 || p[0] != 0x1b || p[1] != ']' {
		return 0, nil, false
	}
	for i := 2; i < len(p); i++ {
		if p[i] == 0x07 {
			return i + 1, p[2:i], true
		}
		if p[i] == 0x1b && i+1 < len(p) && p[i+1] == '\\' {
			return i + 2, p[2:i], true
		}
	}
	return 0, nil, false
}
