package client

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// overlayKey is one keypress the overlay understands.
type overlayKey struct {
	// Rune is the character typed, set when Kind is keyRune.
	Rune rune
	// Kind names a key that is not a character.
	Kind overlayKeyKind
}

// overlayKeyKind classifies what one decoded piece of input is.
type overlayKeyKind int

const (
	// keyRune is a character, which the prompt types and the action table looks up.
	keyRune overlayKeyKind = iota
	keyEnter
	keyBackspace
	// keyKillLine is ctrl-u, which clears the line, as readline does.
	keyKillLine
	// keyUp and keyDown move a selection: the arrows, and ctrl-p and ctrl-n.
	//
	// Both spellings because the picker filters on every printable key, so j and k cannot also mean
	// movement. That is fzf's arrangement and the reason for it is the same.
	keyUp
	keyDown
	// keyCancel closes the overlay: escape or ctrl-c.
	keyCancel
	// keyIgnore is input the overlay drops. A key release, a repeat, or a keypress it does not bind.
	keyIgnore
	// keyPassThrough is input the overlay forwards to the session untouched.
	keyPassThrough
)

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
	case b == '\r':
		return overlayKey{Kind: keyEnter}, 1
	case b == '\n':
		// ctrl-j, which is 0x0a and would otherwise be a second spelling of enter. fzf binds it to "down"
		// and the muscle memory that comes with it is what this overlay is being measured against, so it
		// moves rather than submits. Return itself is CR, which is what a terminal sends for it.
		return overlayKey{Kind: keyDown}, 1
	case b == 0x0b:
		// ctrl-k, up, for the same reason.
		return overlayKey{Kind: keyUp}, 1
	case b == 0x7f || b == 0x08:
		return overlayKey{Kind: keyBackspace}, 1
	case b == 0x15:
		return overlayKey{Kind: keyKillLine}, 1
	case b == 0x10:
		// ctrl-p and ctrl-n, readline's spelling of the same movement.
		return overlayKey{Kind: keyUp}, 1
	case b == 0x0e:
		return overlayKey{Kind: keyDown}, 1
	case b == 0x03:
		return overlayKey{Kind: keyCancel}, 1
	case b < 0x20:
		// Any other control byte is dropped rather than forwarded. Nothing a terminal sends as an *answer*
		// is a bare control byte, so the rule above does not apply, and forwarding a stray ctrl-g into a
		// program while the user is typing at cm would be worse than losing it.
		return overlayKey{Kind: keyIgnore}, 1
	default:
		r, size := utf8.DecodeRune(p)
		if r == utf8.RuneError && size <= 1 {
			return overlayKey{Kind: keyIgnore}, 1
		}
		return overlayKey{Rune: r}, size
	}
}

// decodeEscape classifies a sequence starting with ESC.
func decodeEscape(p []byte) (overlayKey, int) {
	if len(p) == 1 {
		// Escape on its own closes, which is what a prompt is expected to do. See decodeKey on the split
		// sequence this cannot tell apart from a real escape.
		return overlayKey{Kind: keyCancel}, 1
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
		// SS3, which is how an application-mode terminal sends the arrow and F1-F4 keys. Up and down are
		// bound; the rest are keypresses nothing here wants.
		if len(p) >= 3 {
			switch p[2] {
			case 'A':
				return overlayKey{Kind: keyUp}, 3
			case 'B':
				return overlayKey{Kind: keyDown}, 3
			}
			return overlayKey{Kind: keyIgnore}, 3
		}
		return overlayKey{Kind: keyPassThrough}, len(p)
	default:
		// ESC followed by a character is alt-<key> in most terminals. Not bound, and not an answer.
		return overlayKey{Kind: keyIgnore}, 2
	}
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

	switch p[final] {
	case 'u':
		// The kitty keyboard protocol, and the only encoding here that carries a *character*. Which is why
		// this case exists at all: with report-all-keys on, a program in the session has made the terminal
		// send even plain letters this way, and an overlay that only read bytes would answer no keys.
		return decodeKittyKey(params), n
	case '~':
		// A function or editing key, and CSI 27;m;cp~ is modifyOtherKeys reporting a modified one. Both are
		// keypresses. Bracketed paste markers arrive here too, and dropping them is what lets a paste land
		// in the prompt as text.
		return overlayKey{Kind: keyIgnore}, n
	case 'A':
		return overlayKey{Kind: keyUp}, n
	case 'B':
		return overlayKey{Kind: keyDown}, n
	case 'C', 'D', 'E', 'F', 'H', 'P', 'Q', 'S':
		// Left, right, home, end and F1-F4 in their CSI forms. Keypresses, none bound.
		return overlayKey{Kind: keyIgnore}, n
	default:
		// Everything else is an answer or an event the program asked for: CSI R is a cursor position
		// report, CSI n and CSI t are replies, CSI I and CSI O are focus, CSI M and CSI m are mouse.
		return overlayKey{Kind: keyPassThrough}, n
	}
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
		// Ctrl. These have to be listed, because a program that turned on report-all-keys makes the terminal
		// send even ctrl-c this way: without them the overlay's ctrl-c, ctrl-u and the fzf movement keys stop
		// working under exactly the full-screen programs this feature exists for. The two keys cm intercepts
		// are matched before anything is decoded, so they are not here.
		switch code {
		case 'j', 'n':
			return overlayKey{Kind: keyDown}
		case 'k', 'p':
			return overlayKey{Kind: keyUp}
		case 'u':
			return overlayKey{Kind: keyKillLine}
		case 'c':
			return overlayKey{Kind: keyCancel}
		}
		return overlayKey{Kind: keyIgnore}
	}
	if mods != 1 {
		// Any other modifier, which nothing here binds.
		return overlayKey{Kind: keyIgnore}
	}

	switch code {
	case 13:
		return overlayKey{Kind: keyEnter}
	case 57352:
		// The kitty protocol's own codepoints for the arrow keys, which a terminal in that mode sends
		// instead of the CSI A and B forms.
		return overlayKey{Kind: keyUp}
	case 57353:
		return overlayKey{Kind: keyDown}
	case 27:
		return overlayKey{Kind: keyCancel}
	case 127, 8:
		return overlayKey{Kind: keyBackspace}
	}
	if code < 0x20 {
		return overlayKey{Kind: keyIgnore}
	}
	return overlayKey{Rune: rune(code)}
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
