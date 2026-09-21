package client

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/chancez/cm/internal/keymap"
)

// KeySpec describes a key a client intercepts instead of forwarding to the session.
//
// Two of them exist: the detach key, and the prefix key that opens cm's overlay. One type because the
// matching is the hard part and it is identical for both -- a terminal has three ways to spell the same
// keystroke, and any of them can be split across two reads. Getting that right once is the point.
type KeySpec struct {
	// Name is the spelling the user configured, for error messages and for help text.
	Name string
	// Disabled reports that the key is not intercepted, so it reaches the session instead.
	Disabled bool

	// forms is every byte sequence that means this key, the one a terminal sends in its default modes
	// first.
	//
	// A list rather than a byte plus alternates, which is what this was: a control character, plus the CSI
	// forms a keyboard protocol sends for it. That shape could only describe a key whose default form is one
	// byte, so anything else -- f5, delete, an arrow -- could not be an intercepted key at all. The zero
	// value has no forms and matches nothing, which is what live() is for: a zero Byte used to mean NUL, so
	// an unparsed spec silently swallowed ctrl-space.
	forms [][]byte
}

// Primary is what a terminal sends for this key in its default modes, for forwarding it to the program.
func (k KeySpec) Primary() []byte {
	if len(k.forms) == 0 {
		return nil
	}
	return k.forms[0]
}

// DefaultDetachKey and DefaultPrefixKey live in internal/keymap with every other key cm binds, and are
// named here too because this is the package that parses them. See keymap.DefaultPrefixKey for why
// ctrl-] and not something easier to reach.
const (
	DefaultDetachKey = keymap.DefaultDetachKey
	DefaultPrefixKey = keymap.DefaultPrefixKey
)

// ParseKeySpec resolves a configured key.
//
// Through internal/keymap, which is the one place keys are spelled, so an intercepted key accepts exactly
// what a binding does: a name from the table, ctrl-<key>, or a single character. It used to accept ctrl-
// combinations alone, which quietly limited what could be a session key -- `detach = ["f12"]` parsed as a
// binding and then failed here, which is a config that takes a terminal away over a key nobody pressed.
//
// "none" is this layer's own, since it is about interception rather than about a key: it means the program
// gets the key instead.
func ParseKeySpec(spec string) (KeySpec, error) {
	switch strings.ToLower(strings.TrimSpace(spec)) {
	case "":
		return KeySpec{}, fmt.Errorf("no key given")
	case "none", "off", "disabled":
		return KeySpec{Name: "none", Disabled: true}, nil
	}

	chord, err := keymap.ParseChord(spec)
	if err != nil {
		return KeySpec{}, err
	}
	if chord.Typing() {
		// A bare character is refused however clearly the file asks for it. An intercepted key is taken from
		// every program in every session, so a detach key of "a" would make the letter unreachable in vim, in
		// a shell, everywhere, and a typo in a config file is a likelier explanation than the request. The
		// same character inside the overlay is fine, since nothing there competes for it.
		return KeySpec{}, fmt.Errorf(
			"%q is a character a program needs; a key cm takes from the session has to be a control "+
				"combination like ctrl-o or a named key like f5", spec)
	}
	return KeySpecFromChord(chord), nil
}

// KeySpecFromChord builds an intercepted-key matcher from a parsed chord.
//
// The chord already knows every byte form of the key, including the CSI encodings a keyboard protocol sends
// for a ctrl combination, so nothing here re-derives them: two tables of encodings would eventually
// disagree, and the disagreement would look like a key that works on one machine.
func KeySpecFromChord(c keymap.Chord) KeySpec {
	forms := make([][]byte, 0, 1+len(c.Sequences))
	forms = append(forms, c.Bytes)
	forms = append(forms, c.Sequences...)
	return KeySpec{Name: c.Name, forms: forms}
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

// live reports whether this spec describes a key that is intercepted at all.
//
// The zero value is not, and saying so here rather than at each call site is the point: a KeySpec that
// was never parsed has Byte 0, and 0 is NUL, which a terminal really does send for ctrl-space. An unset
// prefix key would otherwise swallow that keystroke while looking disabled. Every parsed spec carries
// its CSI encodings, so their absence is what distinguishes unset from configured.
func (k KeySpec) live() bool {
	return !k.Disabled && len(k.forms) > 0
}

// SameKey reports whether two specs describe the same keystroke.
//
// Compared on the primary form, which is the byte a terminal sends by default, so the pairs that are one
// keystroke under two names come out equal: tab and ctrl-i, enter and ctrl-m, escape and ctrl-[. A
// comparison on names would call those distinct and let a user bind both, leaving one silently unreachable.
func (k KeySpec) SameKey(other KeySpec) bool {
	return k.live() && other.live() && bytes.Equal(k.Primary(), other.Primary())
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
	for _, form := range k.forms {
		if i := bytes.Index(p, form); i >= 0 && (best < 0 || i < best) {
			best, n = i, len(form)
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
	for _, seq := range k.forms {
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

// keySet is an intercepted key and its alternates, matched as one.
//
// A list because a person can want two ways to reach the same thing: a home keyboard where ctrl-\ is
// comfortable and a laptop layout where it is not, or a spare f-key beside the habit. The alternative was
// one key and a second setting for a second key, which is how a list grows one element at a time.
//
// The first is the primary, and that distinction is not cosmetic: it is the key named in the overlay's bar
// and help, and the one `ctrl-] q` forwards to the program. A help line listing three spellings of detach
// would spend the width it has on a fact nobody needs twice.
type keySet []KeySpec

// primary is the key to name and to forward, or a disabled spec when the set is empty.
func (s keySet) primary() KeySpec {
	for _, k := range s {
		if k.live() {
			return k
		}
	}
	if len(s) > 0 {
		return s[0]
	}
	return KeySpec{Name: "none", Disabled: true}
}

// live reports whether any key in the set is intercepted.
func (s keySet) live() bool {
	for _, k := range s {
		if k.live() {
			return true
		}
	}
	return false
}

// allDisabled reports that this set was configured and every key in it is off.
//
// Empty is not the same thing and answers false, which is the distinction that matters for the detach key:
// an unset setting means the default key rather than no key, so an empty list must not read as "detaching
// is turned off". Getting this wrong made a read-only client stop reading the terminal, since
// Options.readsTerminal asks exactly this question.
func (s keySet) allDisabled() bool {
	if len(s) == 0 {
		return false
	}
	for _, k := range s {
		if k.live() {
			return false
		}
	}
	return true
}

// find reports where the earliest press of any key in the set starts, how many bytes it took, and which
// key it was.
//
// Earliest rather than first-in-the-list, because what matters is the order the *user* typed: a read can
// hold two of these keys and acting on the later one would reorder what they did.
//
// The key is returned because a message about a press has to name the key that arrived rather than the
// primary. The nested-handover notice is the case: it tells the user to press the key once more, and with
// several bound, naming a spelling they did not press is worse than naming none.
func (s keySet) find(p []byte) (offset, length int, matched KeySpec) {
	best, n := -1, 0
	var key KeySpec
	for _, k := range s {
		if i, l := k.find(p); i >= 0 && (best < 0 || i < best) {
			best, n, key = i, l, k
		}
	}
	return best, n, key
}

// holdBack is how many trailing bytes to retain for a partial encoding of any key in the set.
//
// The maximum, since a tail that could still become either key has to wait for whichever needs more bytes.
// Taking the minimum would forward the start of a longer encoding and miss that press.
func (s keySet) holdBack(p []byte) int {
	keep := 0
	for _, k := range s {
		keep = max(keep, k.HoldBack(p))
	}
	return keep
}
