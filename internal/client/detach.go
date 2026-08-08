// Package client attaches a terminal to a session.
//
// It owns the local terminal's state: raw mode on the way in, a full reset on the way out,
// and detach-key detection so leaving a session never reaches the shell.
package client

import "bytes"

// DetachKey is Ctrl-\ (0x1C), the byte a terminal sends for it in raw mode.
//
// Detection happens here rather than in the server because the server cannot tell a
// keystroke from output that happens to contain the same byte, and because a detach must
// work even when the connection to the server is what is broken.
const DetachKey = 0x1C

// detachSequences are the ways a terminal may encode Ctrl-\ .
//
// A terminal in kitty keyboard mode or with modifyOtherKeys enabled reports the key as a
// CSI sequence instead of a control byte, so checking only for 0x1C would silently stop
// detecting detach for exactly the users most likely to have those modes on.
var detachSequences = [][]byte{
	// Kitty keyboard protocol: CSI 92 ; 5 u, where 92 is '\' and 5 is ctrl.
	[]byte("\x1b[92;5u"),
	// xterm modifyOtherKeys: CSI 27 ; 5 ; 92 ~
	[]byte("\x1b[27;5;92~"),
}

// FindDetach reports the offset of a detach request in p, or -1 if there is none.
//
// The returned offset is where the input to forward stops. Bytes after the detach request
// are discarded: the user asked to leave, so anything typed after it belongs to whatever
// comes next, not to this session.
func FindDetach(p []byte) int {
	best := -1
	if i := bytes.IndexByte(p, DetachKey); i >= 0 {
		best = i
	}
	for _, seq := range detachSequences {
		if i := bytes.Index(p, seq); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// mightStartDetach reports whether the tail of p could be the start of a longer detach
// sequence that has not fully arrived.
//
// Terminal input arrives in arbitrary pieces, so a CSI-encoded detach can straddle two
// reads. Without holding back a possible prefix, the two halves would be forwarded to the
// shell and the detach missed.
func mightStartDetach(p []byte) bool {
	for _, seq := range detachSequences {
		for n := min(len(p), len(seq)-1); n > 0; n-- {
			if bytes.Equal(p[len(p)-n:], seq[:n]) {
				return true
			}
		}
	}
	return false
}
