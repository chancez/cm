package client

import (
	"bytes"
	"fmt"
	"strings"
)

// DetachKeySpec describes the key that detaches a client.
type DetachKeySpec struct {
	// Byte is the control character the terminal sends in raw mode.
	Byte byte
	// Sequences are the ways a terminal may encode the same key when a keyboard protocol is
	// active, checked in addition to Byte.
	Sequences [][]byte
	// Name is the spelling the user configured, for error messages.
	Name string
	// Disabled reports that no key detaches, so the chosen key reaches the session instead.
	Disabled bool
}

// DefaultDetachKey is ctrl-\, matching zmx.
const DefaultDetachKey = `ctrl-\`

// ParseDetachKey resolves a configured detach key.
//
// Accepts "ctrl-X" for a letter or one of the punctuation characters that have control codes, and
// "none" to disable detaching by key. Configurable because ctrl-\ is awkward or unreachable on
// some keyboard layouts, and disableable because a program inside the session may want the key
// itself.
func ParseDetachKey(spec string) (DetachKeySpec, error) {
	s := strings.ToLower(strings.TrimSpace(spec))
	switch s {
	case "":
		return ParseDetachKey(DefaultDetachKey)
	case "none", "off", "disabled":
		return DetachKeySpec{Name: "none", Disabled: true}, nil
	}

	rest, ok := strings.CutPrefix(s, "ctrl-")
	if !ok {
		rest, ok = strings.CutPrefix(s, "c-")
	}
	if !ok || len(rest) != 1 {
		return DetachKeySpec{}, fmt.Errorf(
			"detach key %q must be \"ctrl-<key>\" or \"none\"", spec)
	}

	c := rest[0]
	code, ok := controlCode(c)
	if !ok {
		return DetachKeySpec{}, fmt.Errorf("no control code exists for ctrl-%c", c)
	}

	return DetachKeySpec{
		Byte:      code,
		Sequences: encodingsFor(c),
		Name:      "ctrl-" + string(c),
	}, nil
}

// controlCode returns the byte a terminal sends for ctrl plus the given character.
//
// Letters map to 1..26. The punctuation entries are the remaining control codes, which exist
// because ctrl-\ and ctrl-] are common choices and would otherwise be unavailable.
func controlCode(c byte) (byte, bool) {
	switch {
	case c >= 'a' && c <= 'z':
		return c - 'a' + 1, true
	case c == '@' || c == ' ':
		return 0x00, true
	case c == '[':
		return 0x1B, true
	case c == '\\':
		return 0x1C, true
	case c == ']':
		return 0x1D, true
	case c == '^':
		return 0x1E, true
	case c == '_':
		return 0x1F, true
	case c == '?':
		return 0x7F, true
	default:
		return 0, false
	}
}

// encodingsFor returns the CSI forms a terminal may send instead of the control byte.
//
// A terminal with the kitty keyboard protocol or xterm's modifyOtherKeys reports a modified key as
// a sequence rather than a control character, so checking only the byte silently stops detecting
// detach for exactly the users most likely to have those modes on. zmx hit this with Claude Code,
// which enables modifyOtherKeys on startup, making ctrl-\ unable to detach at all.
func encodingsFor(c byte) [][]byte {
	// Both protocols identify the key by its unmodified codepoint, with 5 meaning ctrl.
	cp := int(c)
	return [][]byte{
		// kitty keyboard protocol: CSI <codepoint> ; 5 u
		[]byte(fmt.Sprintf("\x1b[%d;5u", cp)),
		// xterm modifyOtherKeys: CSI 27 ; 5 ; <codepoint> ~
		[]byte(fmt.Sprintf("\x1b[27;5;%d~", cp)),
	}
}

// Find reports the offset of a detach request in p, or -1 if there is none.
//
// The offset is where forwarded input stops. Bytes after it are discarded: the user asked to
// leave, so anything typed afterwards belongs to whatever comes next.
func (k DetachKeySpec) Find(p []byte) int {
	if k.Disabled {
		return -1
	}

	best := -1
	if i := bytes.IndexByte(p, k.Byte); i >= 0 {
		best = i
	}
	for _, seq := range k.Sequences {
		if i := bytes.Index(p, seq); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// MightStart reports whether the tail of p could begin a longer detach sequence that has not fully
// arrived.
//
// Terminal input arrives in arbitrary pieces, so a CSI-encoded detach can straddle two reads.
// Without holding back a possible prefix, both halves reach the shell and the detach is missed.
func (k DetachKeySpec) MightStart(p []byte) bool {
	if k.Disabled {
		return false
	}
	for _, seq := range k.Sequences {
		for n := min(len(p), len(seq)-1); n > 0; n-- {
			if bytes.Equal(p[len(p)-n:], seq[:n]) {
				return true
			}
		}
	}
	return false
}

// HoldBack returns how many trailing bytes to retain when a partial sequence may be in flight.
func (k DetachKeySpec) HoldBack(p []byte) int {
	if !k.MightStart(p) {
		return 0
	}
	keep := len(p)
	for _, seq := range k.Sequences {
		if n := len(seq) - 1; n < keep {
			keep = n
		}
	}
	if keep < 0 {
		return 0
	}
	return keep
}
