package client

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/chancez/cm/internal/input"
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
	code, ok := input.ControlCode(c)
	if !ok {
		return DetachKeySpec{}, fmt.Errorf("no control code exists for ctrl-%c", c)
	}

	return DetachKeySpec{
		Byte:      code,
		Sequences: encodingsFor(c),
		Name:      "ctrl-" + string(c),
	}, nil
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
	return k.HoldBack(p) > 0
}

// HoldBack returns how many trailing bytes to retain when a partial sequence may be in flight.
//
// The answer is the length of the longest suffix of p that is a proper prefix of some encoding, and
// nothing more. Retaining a byte that cannot begin a detach does not merely delay it, it corrupts a
// conversation: a program that queries the terminal and blocks for the answer sees a reply cut
// short, and the missing tail surfaces later, pasted into the shell's line editor by the next
// keystroke that flushes it.
//
// This used to derive the count from the *shortest* configured encoding instead, so any chunk whose
// tail matched by even one byte gave up six. The symptom was `wallfacer -h` leaving
// ";rgb:2828/2c2c/3434" and a stray cursor position report on screen, with "execute: 2828/2c2c/3434"
// in the prompt afterwards: an OSC 11 background-color reply had arrived in a chunk ending with the
// ESC of its ST terminator, and the five bytes before that ESC were held hostage. The same
// arithmetic erred the other way for the longer encoding, holding 6 of the 7 bytes of a partial
// "\x1b[27;5;" and forwarding the first, which would miss a detach split at exactly that point.
func (k DetachKeySpec) HoldBack(p []byte) int {
	if k.Disabled {
		return 0
	}
	keep := 0
	for _, seq := range k.Sequences {
		// A complete sequence is not a partial one: Find handles that case, and holding the whole
		// thing here would mean a detach never fires.
		for n := min(len(p), len(seq)-1); n > keep; n-- {
			if bytes.Equal(p[len(p)-n:], seq[:n]) {
				keep = n
				break
			}
		}
	}
	return keep
}
