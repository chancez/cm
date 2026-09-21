// Package keymap is the one place that says which key does what in cm's own interfaces.
//
// Two interfaces bind keys: the overlay inside an attached session (internal/client) and the session
// picker (internal/tui). They had nothing in common. The overlay matched runes in a switch, its chooser
// decided that ctrl-j meant "down" while decoding the byte, and the picker's keys were a bubbles key.Map
// plus whatever bubbles/list defaulted to. Nothing was configurable except the two keys cm intercepts,
// and each place spelled keys its own way.
//
// So the tables live here, once, and each interface asks what a keypress means rather than deciding. The
// consequences worth knowing:
//
//   - One spelling for the whole tool. A chord is written the way `cm send --key`, `detach_key` and
//     `prefix_key` already accept: a name from internal/input's table, "ctrl-<key>", or a single
//     character. A second grammar for bindings would be a second thing to get wrong.
//   - An action can have several chords, and a configured list replaces the defaults rather than adding
//     to them, which is the only way to take a default away.
//   - A chord carries both spellings it needs: the bytes a terminal sends, for the overlay, which reads
//     a pty; and bubbletea's name, for the picker, which reads decoded key messages. Deriving one from
//     the other at each call site is how they drift.
package keymap

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/chancez/cm/internal/input"
)

// Chord is one keypress an action can be bound to.
//
// Not a key *sequence*: everything here is a single press. cm's one two-key gesture is the overlay's
// prefix followed by an action key, and the prefix is matched before any of this by inputGate.
type Chord struct {
	// Name is the canonical spelling, which is what help text shows.
	//
	// Canonical rather than what the user typed, so "esc" and "escape" do not appear as two different
	// keys in the same help line, and so two chords can be compared by name.
	Name string
	// Bytes is what a terminal sends for this press in its default modes.
	//
	// From internal/input, the same table `cm send --key` uses. That is deliberate: a key someone
	// configures and then sends by name has to be one keystroke, and two tables would eventually
	// disagree about which.
	Bytes []byte
	// Sequences are the forms a terminal sends for this press when a keyboard protocol is active,
	// checked in addition to Bytes.
	//
	// Only a ctrl- chord has any. A terminal with the kitty keyboard protocol or xterm's
	// modifyOtherKeys reports ctrl-\ as a CSI sequence rather than as 0x1c, and a client that matched
	// only the control byte stopped detecting the key for exactly the users most likely to have those
	// modes on.
	Sequences [][]byte
	// Tea is bubbletea's name for this press, or empty when bubbletea has none.
	//
	// Empty is a real case rather than an oversight: "newline" is a distinct byte to a pty and not a
	// distinct key to bubbletea, so a picker action bound to it would silently never fire. Build reports
	// that instead.
	Tea string

	// kind and r are how a decoded keypress is matched. See Chord.Matches.
	kind chordKind
	r    rune
}

// chordKind is what sort of press a chord describes.
type chordKind int

const (
	// chordRune is a character: "s", "/", "G". Case matters, since G and g are different presses.
	chordRune chordKind = iota
	// chordCtrl is ctrl plus a character, held as that character rather than as its control byte so
	// ctrl-i and tab stay distinguishable in help text even though the byte is the same.
	chordCtrl
	// chordNamed is a key with a name of its own: enter, up, f5.
	chordNamed
)

// named maps every accepted spelling of a named key to its canonical one.
//
// The aliases are internal/input's, so anything that table accepts is accepted here and means the same
// press. Canonicalizing at parse time is what lets Matches and the collision check compare names.
var named = map[string]string{
	"enter": "enter", "return": "enter", "cr": "enter",
	"newline": "newline", "lf": "newline",
	"tab":    "tab",
	"space":  "space",
	"escape": "escape", "esc": "escape",
	"backspace": "backspace", "bs": "backspace",
	"delete": "delete", "del": "delete",
	"insert": "insert",
	"up":     "up", "down": "down", "left": "left", "right": "right",
	"home": "home", "end": "end",
	"pageup": "pageup", "pgup": "pageup",
	"pagedown": "pagedown", "pgdn": "pagedown",
	"f1": "f1", "f2": "f2", "f3": "f3", "f4": "f4", "f5": "f5", "f6": "f6",
	"f7": "f7", "f8": "f8", "f9": "f9", "f10": "f10", "f11": "f11", "f12": "f12",
}

// teaNames maps a canonical name to bubbletea's spelling, where the two differ.
//
// Only the differences, since most names match. Measured against bubbletea v2.0.9 by asking it what it
// calls each press rather than reading its source. The pairing is pinned by a test in internal/tui,
// which is the package that imports bubbletea: a rename upstream fails there instead of silently
// unbinding a key here.
var teaNames = map[string]string{
	"escape":   "esc",
	"pageup":   "pgup",
	"pagedown": "pgdown",
	// A pty distinguishes LF from CR; bubbletea does not report it as a key of its own.
	"newline": "",
}

// ParseChord resolves one written key.
//
// Rejects "alt-" and the caret form, both of which internal/input accepts for `cm send`, and the reason
// is the overlay rather than taste. An alt-modified key reaches a terminal as ESC followed by the key,
// which cannot be told apart from the escape key at a read boundary: the overlay already documents that
// a lone escape closes it and the tail is forwarded. A binding whose recognition depends on two reads
// arriving together is one that works until the machine is busy.
func ParseChord(spec string) (Chord, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return Chord{}, fmt.Errorf("empty key")
	}
	lower := strings.ToLower(s)

	switch {
	case strings.HasPrefix(lower, "alt-"), strings.HasPrefix(lower, "m-"), strings.HasPrefix(lower, "meta-"):
		return Chord{}, fmt.Errorf(
			"alt is not available for cm's own keys: a terminal sends it as escape then the key, "+
				"which cannot be told from the escape key (%q)", spec)
	case strings.HasPrefix(s, "^"):
		return Chord{}, fmt.Errorf("write %q as ctrl-%s", s, strings.TrimPrefix(s, "^"))
	}

	if canonical, ok := named[lower]; ok {
		b, err := input.ParseKey(canonical)
		if err != nil {
			return Chord{}, err
		}
		tea, ok := teaNames[canonical]
		if !ok {
			tea = canonical
		}
		return Chord{Name: canonical, Bytes: b, Tea: tea, kind: chordNamed}, nil
	}

	if rest, ok := cutCtrl(lower); ok {
		// A named key after ctrl- resolves to its character first, so ctrl-space is NUL, which is what
		// internal/input does with the same spelling.
		if canonical, isNamed := named[rest]; isNamed {
			if b, err := input.ParseKey(canonical); err == nil && len(b) == 1 {
				rest = string(b)
			}
		}
		if r := []rune(rest); len(r) != 1 {
			return Chord{}, fmt.Errorf("ctrl- takes a single character or a named key, got %q", rest)
		}
		c := []rune(rest)[0]
		if c > 0x7f {
			return Chord{}, fmt.Errorf("no control code exists for ctrl-%c", c)
		}
		code, ok := input.ControlCode(byte(c))
		if !ok {
			return Chord{}, fmt.Errorf("no control code exists for ctrl-%c", c)
		}
		name := "ctrl-" + string(c)
		if c == ' ' {
			// Named rather than a literal space, which would read as a stray character in a help line.
			name = "ctrl-space"
		}
		return Chord{
			Name:      name,
			Bytes:     []byte{code},
			Sequences: encodingsFor(byte(c)),
			Tea:       "ctrl+" + strings.TrimPrefix(name, "ctrl-"),
			kind:      chordCtrl,
			r:         c,
		}, nil
	}

	// A single character is itself, case kept: the picker's list binds both g and G.
	if r := []rune(s); len(r) == 1 {
		return Chord{
			Name:  s,
			Bytes: []byte(s),
			Tea:   s,
			kind:  chordRune,
			r:     r[0],
		}, nil
	}

	return Chord{}, fmt.Errorf(
		"unknown key %q; want a name like enter, tab, up or f5, a control key like ctrl-c, "+
			"or a single character", spec)
}

// cutCtrl strips a ctrl- prefix in either spelling.
func cutCtrl(s string) (string, bool) {
	if rest, ok := strings.CutPrefix(s, "ctrl-"); ok {
		return rest, true
	}
	return strings.CutPrefix(s, "c-")
}

// encodingsFor returns the CSI forms a terminal may send for a ctrl- press instead of the control byte.
//
// Both protocols identify the key by its unmodified codepoint, with 5 meaning ctrl. zmx hit the absence
// of this with Claude Code, which enables modifyOtherKeys on startup, and its detach key stopped working
// entirely.
func encodingsFor(c byte) [][]byte {
	cp := int(c)
	return [][]byte{
		// kitty keyboard protocol: CSI <codepoint> ; 5 u
		[]byte(fmt.Sprintf("\x1b[%d;5u", cp)),
		// xterm modifyOtherKeys: CSI 27 ; 5 ; <codepoint> ~
		[]byte(fmt.Sprintf("\x1b[27;5;%d~", cp)),
	}
}

// Press is a keypress something has already decoded, offered to a Map to be named.
//
// Three fields rather than one, because the two callers know different things. The overlay reads bytes
// off a pty and can say "this was ctrl-n" or "this was the up arrow"; the picker is handed a decoded key
// message. Both can fill this in, and neither has to know how a chord is stored.
type Press struct {
	// Rune is the character typed, or 0.
	Rune rune
	// Ctrl is the character of a control press, 'n' for ctrl-n, or 0.
	Ctrl rune
	// Named is the canonical name of a named key, or empty.
	Named string
}

// Matches reports whether this chord is the press given.
func (c Chord) Matches(p Press) bool {
	switch c.kind {
	case chordCtrl:
		return p.Ctrl != 0 && p.Ctrl == c.r
	case chordNamed:
		return p.Named != "" && p.Named == c.Name
	default:
		return p.Rune != 0 && p.Rune == c.r
	}
}

// Same reports whether two chords are the same keystroke.
//
// Compared by bytes rather than by name, which matters for exactly the pairs that look distinct and are
// not: tab is 0x09 and so is ctrl-i, enter is 0x0d and so is ctrl-m, escape is 0x1b and so is ctrl-[.
// Two actions bound to "tab" and "ctrl-i" are a collision, and a check on names would have missed it.
func (c Chord) Same(other Chord) bool {
	return bytes.Equal(c.Bytes, other.Bytes)
}
