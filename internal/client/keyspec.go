package client

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/chancez/cm/internal/input"
)

// KeySpec describes a key a client intercepts instead of forwarding to the session.
//
// Two of them exist: the detach key, and the prefix key that opens cm's overlay. One type because the
// matching is the hard part and it is identical for both -- a terminal has three ways to spell the same
// keystroke, and any of them can be split across two reads. Getting that right once is the point.
type KeySpec struct {
	// Byte is the control character the terminal sends in raw mode.
	Byte byte
	// Sequences are the ways a terminal may encode the same key when a keyboard protocol is
	// active, checked in addition to Byte.
	Sequences [][]byte
	// Name is the spelling the user configured, for error messages.
	Name string
	// Disabled reports that the key is not intercepted, so it reaches the session instead.
	Disabled bool
}

// DefaultDetachKey is ctrl-\, matching zmx.
const DefaultDetachKey = `ctrl-\`

// DefaultPrefixKey is ctrl-], which opens the overlay.
//
// Chosen against the alternatives on two counts. Ergonomics: left ctrl and a right-hand key, which
// ctrl-a and ctrl-b (screen and tmux) are not. And cost, which is what ruled the rest out: ctrl-o is
// vim's jumplist-back, ctrl-u ctrl-p ctrl-n ctrl-l are readline's, and every remaining right-hand
// control code is a key a program wants. ctrl-] costs vim's ctags tag-jump, which an LSP's gd has
// largely replaced. ctrl-space has the best ergonomics of all and was rejected for delivery: not every
// terminal sends NUL for it, and a key that silently does nothing on one machine is worse than a key
// that costs something everywhere.
const DefaultPrefixKey = "ctrl-]"

// ParseKeySpec resolves a configured key.
//
// Accepts "ctrl-X" for a letter, one of the punctuation characters that have control codes, or a named
// key that is itself a single character; and "none" to disable interception. Configurable because ctrl-\
// is awkward or unreachable on some keyboard layouts, and disableable because a program inside the
// session may want the key itself.
func ParseKeySpec(spec string) (KeySpec, error) {
	s := strings.ToLower(strings.TrimSpace(spec))
	switch s {
	case "":
		return KeySpec{}, fmt.Errorf("no key given")
	case "none", "off", "disabled":
		return KeySpec{Name: "none", Disabled: true}, nil
	}

	rest, ok := strings.CutPrefix(s, "ctrl-")
	if !ok {
		rest, ok = strings.CutPrefix(s, "c-")
	}
	if !ok {
		return KeySpec{}, fmt.Errorf(
			"key %q must be \"ctrl-<key>\" or \"none\"", spec)
	}

	// A named key resolves first, so "ctrl-space" is NUL rather than rejected for not being one
	// character. Same table `cm send --key` uses, deliberately: a user who configures a key and then
	// sends it by name is describing one keystroke, and two spellings that disagree would be a bug
	// nobody could see. Only single-byte names qualify, since ctrl plus an arrow key is not a control
	// code at all.
	name := "ctrl-" + rest
	if named, err := input.ParseKey(rest); err == nil && len(named) == 1 &&
		named[0] >= 0x20 && named[0] < 0x7f {
		// Printable only. "ctrl-esc" would otherwise resolve to 0x1b and then be reported as having no
		// control code, with the raw byte in the message: ctrl-[ is the spelling for that key, and a name
		// whose own byte is already a control code is not a ctrl- combination at all.
		rest = string(named)
	}
	if len(rest) != 1 {
		return KeySpec{}, fmt.Errorf(
			"key %q must be \"ctrl-<key>\" or \"none\"", spec)
	}

	c := rest[0]
	code, ok := input.ControlCode(c)
	if !ok {
		return KeySpec{}, fmt.Errorf("no control code exists for ctrl-%c", c)
	}

	return KeySpec{
		Byte:      code,
		Sequences: encodingsFor(c),
		Name:      name,
	}, nil
}

// ParseDetachKey resolves the detach key, defaulting when nothing is configured.
//
// Empty falls back to the default rather than disabling: an unset config setting must not silently
// remove the only way to leave a session.
// Errors name which key was wrong, which matters now that two of them are configurable: "key
// \"not-a-key\" must be ctrl-<key>" leaves a user checking both settings.
func ParseDetachKey(spec string) (KeySpec, error) {
	if strings.TrimSpace(spec) == "" {
		spec = DefaultDetachKey
	}
	key, err := ParseKeySpec(spec)
	if err != nil {
		return KeySpec{}, fmt.Errorf("detach %w", err)
	}
	return key, nil
}

// ParsePrefixKey resolves the overlay's prefix key, defaulting when nothing is configured.
func ParsePrefixKey(spec string) (KeySpec, error) {
	if strings.TrimSpace(spec) == "" {
		spec = DefaultPrefixKey
	}
	key, err := ParseKeySpec(spec)
	if err != nil {
		return KeySpec{}, fmt.Errorf("prefix %w", err)
	}
	return key, nil
}

// encodingsFor returns the CSI forms a terminal may send instead of the control byte.
//
// A terminal with the kitty keyboard protocol or xterm's modifyOtherKeys reports a modified key as a
// sequence rather than a control character, so checking only the byte silently stops detecting the key
// for exactly the users most likely to have those modes on. zmx hit this with Claude Code, which
// enables modifyOtherKeys on startup, making ctrl-\ unable to detach at all.
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

// live reports whether this spec describes a key that is intercepted at all.
//
// The zero value is not, and saying so here rather than at each call site is the point: a KeySpec that
// was never parsed has Byte 0, and 0 is NUL, which a terminal really does send for ctrl-space. An unset
// prefix key would otherwise swallow that keystroke while looking disabled. Every parsed spec carries
// its CSI encodings, so their absence is what distinguishes unset from configured.
func (k KeySpec) live() bool {
	return !k.Disabled && len(k.Sequences) > 0
}

// Find reports the offset of a press of this key in p, or -1 if there is none.
func (k KeySpec) Find(p []byte) int {
	i, _ := k.find(p)
	return i
}

// find reports where a press of this key starts in p and how many bytes it took.
//
// The length matters to a caller that has to keep going: the detach key discards what follows, but the
// prefix key hands it to the overlay, and a chunk can hold the prefix and the key after it when someone
// types quickly or pastes. Dropping the remainder there ate the second keystroke of every fast
// two-key sequence.
func (k KeySpec) find(p []byte) (offset, length int) {
	if !k.live() {
		return -1, 0
	}

	best, n := -1, 0
	if i := bytes.IndexByte(p, k.Byte); i >= 0 {
		best, n = i, 1
	}
	for _, seq := range k.Sequences {
		if i := bytes.Index(p, seq); i >= 0 && (best < 0 || i < best) {
			best, n = i, len(seq)
		}
	}
	return best, n
}

// MightStart reports whether the tail of p could begin a longer encoding of this key that has not fully
// arrived.
//
// Terminal input arrives in arbitrary pieces, so a CSI-encoded press can straddle two reads. Without
// holding back a possible prefix, both halves reach the shell and the press is missed.
func (k KeySpec) MightStart(p []byte) bool {
	return k.HoldBack(p) > 0
}

// HoldBack returns how many trailing bytes to retain when a partial sequence may be in flight.
//
// The answer is the length of the longest suffix of p that is a proper prefix of some encoding, and
// nothing more. Retaining a byte that cannot begin this key does not merely delay it, it corrupts a
// conversation: a program that queries the terminal and blocks for the answer sees a reply cut
// short, and the missing tail surfaces later, pasted into the shell's line editor by the next
// keystroke that flushes it.
//
// This used to derive the count from the *shortest* configured encoding instead, so any chunk whose
// tail matched by even one byte gave up six. An OSC 11 background-color reply arriving in a chunk
// that ended with the ESC of its ST terminator had the five bytes before that ESC held hostage. The
// same arithmetic erred the other way for the longer encoding, holding 6 of the 7 bytes of a partial
// "\x1b[27;5;" and forwarding the first, which would miss a detach split at exactly that point.
//
// That bug was real and is fixed. Note, though, that the symptom it was diagnosed from -- `wallfacer
// -h` leaving ";rgb:2828/2c2c/3434" and "execute: 2828/2c2c/3434" at the prompt -- came back
// afterwards with an unrelated cause: cm was injecting its own answer to a *different* query into the
// pty mid-read, so wallfacer consumed that and left the terminal's reply unclaimed. See
// Session.drainPending. Measured while chasing the recurrence: holdback retains exactly 1 byte of
// that reply for both ctrl-o and ctrl-\, and never false-detaches on it, so this code was not
// involved the second time.
//
// The lesson worth carrying is that this symptom has had two distinct causes, so seeing it again is
// not evidence about this function.
func (k KeySpec) HoldBack(p []byte) int {
	if !k.live() {
		return 0
	}
	keep := 0
	for _, seq := range k.Sequences {
		// A complete sequence is not a partial one: Find handles that case, and holding the whole
		// thing here would mean a press never fires.
		for n := min(len(p), len(seq)-1); n > keep; n-- {
			if bytes.Equal(p[len(p)-n:], seq[:n]) {
				keep = n
				break
			}
		}
	}
	return keep
}
