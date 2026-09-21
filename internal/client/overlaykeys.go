package client

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/chancez/cm/internal/keymap"
)

// overlayKey is one piece of input the overlay has classified.
type overlayKey struct {
	// Press names the key for the keymap, which is what decides the action. Zero unless Kind is keyPress.
	//
	// Identity rather than meaning, and that inversion is the point of this type. This decoder used to
	// answer "down" for ctrl-j, so the binding lived in the byte parsing and nothing else could have a
	// different opinion. It now answers "ctrl-j", and internal/keymap says what ctrl-j does.
	Press keymap.Press
	// Kind is what to do with it at all: bind it, drop it, or forward it to the session.
	Kind overlayKeyKind
}

// overlayKeyKind classifies what one decoded piece of input is.
//
// Three cases, and the split between the last two is the load-bearing one: see decodeKey.
type overlayKeyKind int

const (
	// keyPress is a keypress, whose Press the keymap can look up.
	keyPress overlayKeyKind = iota
	// keyIgnore is input the overlay drops. A key release, a repeat, or a keypress it cannot name.
	keyIgnore
	// keyPassThrough is input the overlay forwards to the session untouched.
	keyPassThrough
)

// press builds a keypress of a named key, and ctrlPress one of a control combination.
//
// Both are set for the keys that are both: tab is ctrl-i, enter is ctrl-m, escape is ctrl-[ and
// backspace is ctrl-?. A terminal sends one byte for each pair, so a binding written either way has to
// match it, and a chord written the other way would otherwise look like a key the terminal never sends.
func press(name string, ctrl rune) overlayKey {
	return overlayKey{Kind: keyPress, Press: keymap.Press{Named: name, Ctrl: ctrl}}
}

func ctrlPress(c rune) overlayKey {
	return overlayKey{Kind: keyPress, Press: keymap.Press{Ctrl: c}}
}

func runePress(r rune) overlayKey {
	return overlayKey{Kind: keyPress, Press: keymap.Press{Rune: r}}
}

// controlByteKey names the press a bare control byte is.
//
// Every one of them is named rather than only the few the overlay used to bind, because which of them
// mean anything is now the keymap's business: a byte dropped here could not be bound at all, whatever
// the config file said. Dropping is still the answer for anything that is not a control code of a key
// somebody can press.
func controlByteKey(b byte) (overlayKey, bool) {
	switch {
	case b == '\r':
		return press("enter", 'm'), true
	case b == '\n':
		// ctrl-j, which is LF. fzf binds it to "down" and so does cm's default keymap, but that is now a
		// default rather than something decided here.
		return press("newline", 'j'), true
	case b == '\t':
		return press("tab", 'i'), true
	case b == 0x7f, b == 0x08:
		// Both spellings of backspace, since terminals disagree about which they send.
		return press("backspace", '?'), true
	case b == 0x00:
		// NUL, which is what a terminal sends for ctrl-space where it sends anything at all.
		return ctrlPress(' '), true
	case b >= 0x01 && b <= 0x1a:
		return ctrlPress(rune('a' + b - 1)), true
	case b == 0x1c:
		return ctrlPress('\\'), true
	case b == 0x1d:
		return ctrlPress(']'), true
	case b == 0x1e:
		return ctrlPress('^'), true
	case b == 0x1f:
		return ctrlPress('_'), true
	}
	return overlayKey{Kind: keyIgnore}, false
}

// decodeKey reads one keypress off the front of p and reports how many bytes it took.
//
// The rule that matters is the classification, not the parsing: **a sequence that could be an answer is
// forwarded, and a sequence that can only be a keypress is dropped.** While the overlay is open it is
// holding the keyboard, and this stream carries more than keys -- a program inside the session may have
// asked the terminal a question, and its reply arrives here. cm has had six bugs in that family, and the
// expensive shape of it is a program blocked forever on an answer something else consumed. So a cursor
// position report, an OSC colour reply, a graphics response, a focus event and a mouse report all go to
// the session, and only what is unmistakably a keypress is dropped.
//
// Naming a key and binding it are different things, and only the naming happens here. Every form below
// is classified exactly as it was before the keymap existed; what changed is that a keypress arrives at
// the overlay as itself rather than as a meaning. One consequence is worth stating because it looks like
// an omission: f3 cannot be bound in the overlay. Its CSI form is CSI R, which is also a cursor position
// report, and the classification wins -- a bound f3 would eat an answer a program is blocked on.
//
// Known cost: a sequence split across two reads. The overlay does not hold bytes back the way inputGate
// does, so an escape arriving alone closes the overlay and the tail of that sequence is forwarded
// without it. That window is one read wide and only while the overlay is open, which is a few seconds a
// day, against a holdback that would delay every keystroke typed at the prompt.
func decodeKey(p []byte) (overlayKey, int) {
	if len(p) == 0 {
		return overlayKey{Kind: keyIgnore}, 0
	}

	switch b := p[0]; {
	case b == 0x1b:
		return decodeEscape(p)
	case b < 0x20 || b == 0x7f:
		key, ok := controlByteKey(b)
		if !ok {
			// Dropped rather than forwarded. Nothing a terminal sends as an *answer* is a bare control byte,
			// so the forwarding rule does not apply, and sending a stray control character into a program
			// while the user is typing at cm would be worse than losing it.
			return overlayKey{Kind: keyIgnore}, 1
		}
		return key, 1
	default:
		r, size := utf8.DecodeRune(p)
		if r == utf8.RuneError && size <= 1 {
			return overlayKey{Kind: keyIgnore}, 1
		}
		return runePress(r), size
	}
}

// decodeEscape classifies a sequence starting with ESC.
func decodeEscape(p []byte) (overlayKey, int) {
	if len(p) == 1 {
		// Escape on its own, which the overlay treats as a step back whatever the keymap says. See decodeKey
		// on the split sequence this cannot tell apart from a real escape.
		return press("escape", '['), 1
	}

	switch p[1] {
	case '[':
		return decodeCSI(p)
	case ']', 'P', '_', '^', 'X':
		// OSC, DCS, APC, PM and SOS. Every one of these that arrives on *input* is an answer: an OSC 11
		// background colour, a DCS response to XTGETTCAP, an APC kitty graphics response. Forwarded whole,
		// including the terminator, since the program is blocked waiting for it.
		return overlayKey{Kind: keyPassThrough}, stringControlLen(p)
	case 'O':
		// SS3, which is how an application-mode terminal sends the arrow and F1-F4 keys.
		if len(p) >= 3 {
			if name, ok := ss3Keys[p[2]]; ok {
				return press(name, 0), 3
			}
			return overlayKey{Kind: keyIgnore}, 3
		}
		return overlayKey{Kind: keyPassThrough}, len(p)
	default:
		// ESC followed by a character is alt-<key> in most terminals. Not bound -- see keymap.ParseChord on
		// why alt cannot be -- and not an answer.
		return overlayKey{Kind: keyIgnore}, 2
	}
}

// ss3Keys names the keys an application-mode terminal sends as SS3.
var ss3Keys = map[byte]string{
	'A': "up", 'B': "down", 'C': "right", 'D': "left",
	'H': "home", 'F': "end",
	'P': "f1", 'Q': "f2", 'R': "f3", 'S': "f4",
}

// csiLetterKeys names the keys whose CSI form ends in a letter.
//
// R is deliberately absent, and this is the one place where a key cannot be bound because of what else
// shares its encoding: CSI R is also a cursor position report, which a program may be blocked on. F3
// therefore reaches the session rather than the overlay. Answering a query beats binding a key.
var csiLetterKeys = map[byte]string{
	'A': "up", 'B': "down", 'C': "right", 'D': "left",
	'H': "home", 'F': "end",
	'P': "f1", 'Q': "f2", 'S': "f4",
}

// csiTildeKeys names the keys whose CSI form is a number and a tilde.
var csiTildeKeys = map[int]string{
	2: "insert", 3: "delete", 5: "pageup", 6: "pagedown",
	15: "f5", 17: "f6", 18: "f7", 19: "f8", 20: "f9", 21: "f10", 23: "f11", 24: "f12",
}

// decodeCSI classifies a CSI sequence, which is where both keypresses and answers live.
func decodeCSI(p []byte) (overlayKey, int) {
	final := -1
	for i := 2; i < len(p); i++ {
		if p[i] >= 0x40 && p[i] <= 0x7e {
			final = i
			break
		}
	}
	if final < 0 {
		// Incomplete. Forwarded rather than held, on the same reasoning as decodeKey's split sequence: a
		// partial answer reaching the program late is recoverable, and holding input at a prompt is not.
		return overlayKey{Kind: keyPassThrough}, len(p)
	}
	params := string(p[2:final])
	n := final + 1

	switch {
	case p[final] == 'u':
		// The kitty keyboard protocol, and the only encoding here that carries a *character*. Which is why
		// this case exists at all: with report-all-keys on, a program in the session has made the terminal
		// send even plain letters this way, and an overlay that only read bytes would answer no keys.
		return decodeKittyKey(params), n
	case p[final] == '~':
		return decodeTildeKey(params), n
	default:
		if name, ok := csiLetterKeys[p[final]]; ok {
			return press(name, 0), n
		}
		// Everything else is an answer or an event the program asked for: CSI R is a cursor position
		// report, CSI n and CSI t are replies, CSI I and CSI O are focus, CSI M and CSI m are mouse.
		return overlayKey{Kind: keyPassThrough}, n
	}
}

// decodeTildeKey classifies a CSI <params> ~ sequence.
//
// Three shapes arrive here, and all three are keypresses or paste markers rather than answers, which is
// why this branch never forwards. A bare number is a function or editing key. CSI 27;mods;codepoint~ is
// xterm's modifyOtherKeys reporting a modified key, and the ctrl forms of it are named for the same reason
// the kitty ones are: a program that turned the mode on makes the terminal send the overlay's own keys
// that way, and they stopped working under exactly the programs the overlay exists for. CSI 200~ and
// CSI 201~ are bracketed paste, dropped so a paste lands in the prompt as text.
func decodeTildeKey(params string) overlayKey {
	fields := strings.Split(params, ";")
	first, err := strconv.Atoi(strings.SplitN(fields[0], ":", 2)[0])
	if err != nil {
		return overlayKey{Kind: keyIgnore}
	}

	if first == 27 && len(fields) == 3 {
		mods, _ := strconv.Atoi(strings.SplitN(fields[1], ":", 2)[0])
		code, err := strconv.Atoi(strings.SplitN(fields[2], ":", 2)[0])
		if err != nil || mods != 5 || code <= 0 || code > 0x10ffff {
			return overlayKey{Kind: keyIgnore}
		}
		return ctrlPress(rune(code))
	}

	if name, ok := csiTildeKeys[first]; ok {
		return press(name, 0)
	}
	return overlayKey{Kind: keyIgnore}
}

// decodeKittyKey turns the parameters of a CSI ... u sequence into a keypress.
//
// Two fields matter. The first is the codepoint of the key, which is the character. The second is
// modifiers, and its sub-parameter is the event type: 1 press, 2 repeat, 3 release. Reading the event
// type is not optional. A terminal told to report event types sends a release after every press,
// including the release of the prefix key that opened the overlay, so an overlay that treated a release
// as a keypress closed itself the instant the key came up.
func decodeKittyKey(params string) overlayKey {
	fields := strings.Split(params, ";")
	if len(fields) == 0 || fields[0] == "" {
		return overlayKey{Kind: keyIgnore}
	}
	// The codepoint may carry shifted and base-layout alternates after a colon, which are not wanted here.
	code, err := strconv.Atoi(strings.SplitN(fields[0], ":", 2)[0])
	if err != nil || code <= 0 || code > 0x10ffff {
		return overlayKey{Kind: keyIgnore}
	}

	mods, event := 1, 1
	if len(fields) > 1 && fields[1] != "" {
		sub := strings.SplitN(fields[1], ":", 2)
		if v, err := strconv.Atoi(sub[0]); err == nil && v > 0 {
			mods = v
		}
		if len(sub) == 2 {
			if v, err := strconv.Atoi(sub[1]); err == nil && v > 0 {
				event = v
			}
		}
	}
	if event != 1 {
		return overlayKey{Kind: keyIgnore}
	}
	if mods == 5 {
		// Ctrl, named rather than resolved to a meaning. These have to be decoded at all because a program
		// that turned on report-all-keys makes the terminal send even ctrl-c this way: without them the
		// overlay's own control keys stop working under exactly the full-screen programs this feature exists
		// for. The two keys cm intercepts are matched before anything is decoded, so they are not here.
		if code <= 0x7f {
			return ctrlPress(rune(code))
		}
		return overlayKey{Kind: keyIgnore}
	}
	if mods != 1 {
		// Any other modifier, which nothing here binds: see keymap.ParseChord.
		return overlayKey{Kind: keyIgnore}
	}

	switch code {
	case 13:
		return press("enter", 'm')
	case 9:
		return press("tab", 'i')
	case 27:
		return press("escape", '[')
	case 127, 8:
		return press("backspace", '?')
	}
	if name, ok := kittyFunctionalKeys[code]; ok {
		return press(name, 0)
	}
	if code < 0x20 {
		return overlayKey{Kind: keyIgnore}
	}
	return runePress(rune(code))
}

// kittyFunctionalKeys are the private-use codepoints the kitty protocol gives keys that have no
// character, which a terminal in that mode sends instead of the CSI forms.
var kittyFunctionalKeys = map[int]string{
	57352: "up", 57353: "down", 57351: "left", 57354: "right",
	57356: "home", 57357: "end", 57358: "pageup", 57359: "pagedown",
	57348: "insert", 57349: "delete",
	57364: "f1", 57365: "f2", 57366: "f3", 57367: "f4", 57368: "f5", 57369: "f6",
	57370: "f7", 57371: "f8", 57372: "f9", 57373: "f10", 57374: "f11", 57375: "f12",
}

// stringControlLen returns the length of a string control (OSC, DCS, APC, PM, SOS) at the front of p,
// which ends at ST or BEL, or the whole of p when the terminator has not arrived yet.
func stringControlLen(p []byte) int {
	for i := 2; i < len(p); i++ {
		if p[i] == 0x07 {
			return i + 1
		}
		if p[i] == 0x1b && i+1 < len(p) && p[i+1] == '\\' {
			return i + 2
		}
	}
	return len(p)
}

// splitCommandLine turns what was typed into an argv.
//
// Quote-aware because a cm command takes values with spaces -- `tag note="fixing the parser"` and
// `run -- sh -c "..."` -- and strings.Fields would split them into arguments that mean something else.
// Deliberately not a shell: no expansion, no escapes, no operators. What is typed here runs the cm
// binary directly with these arguments and no shell in between, so there is nothing for a `;` or a `$` to
// do, and pretending otherwise would invite a user to expect it.
//
// A leading "cm" is dropped, since typing it is the habit and rejecting it would be pedantic.
func splitCommandLine(line string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inWord  bool
		quote   rune
		unquote bool
	)
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			// A quoted empty string is still an argument, which this flag is what remembers: `tag note=""`
			// has to reach cm as one argument rather than being dropped for having no characters.
			unquote = true
			inWord = true
		case r == ' ' || r == '\t':
			if inWord || unquote {
				args = append(args, cur.String())
				cur.Reset()
				inWord, unquote = false, false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if inWord || unquote {
		args = append(args, cur.String())
	}
	if len(args) > 0 && args[0] == "cm" {
		args = args[1:]
	}
	return args, nil
}
