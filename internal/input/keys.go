// Keys turns the names people use for keys into the bytes a terminal sends for them.
//
// This exists because `cm send` writes bytes to a pty, which is right: input that did not go through
// the pty would not reach a program's line discipline, and a shell would never see it as typing. The
// consequence is that a caller wanting ctrl-c had to produce the byte itself, and every natural
// spelling -- "C-c", "ctrl-c", "^C", "\003" -- was sent as literal text instead. Silently: the session
// ended up with "C-cctrl-c^C\003" on its command line and a script believed it had interrupted a build
// that was still running.
package input

import (
	"fmt"
	"strings"
)

// namedKeys maps a key name to the bytes a terminal sends for it.
//
// The escape sequences are the forms a terminal sends in its default modes, which is what a program
// reading a pty expects. Not the kitty-protocol or modifyOtherKeys variants: those are what a terminal
// sends to cm when a user presses a key with a modifier, and a program inside the session has not
// negotiated them with whoever is calling `cm send`.
//
// Arrow and editing keys use the CSI forms rather than the SS3 forms an application-mode terminal
// sends. Both are widely accepted, and CSI is what a terminal sends when nothing has switched modes,
// which is the state a session is in unless a full-screen program changed it.
var namedKeys = map[string][]byte{
	// Characters that are awkward to pass as arguments, or that a caller means by name.
	"enter":     {'\r'},
	"return":    {'\r'},
	"cr":        {'\r'},
	"newline":   {'\n'},
	"lf":        {'\n'},
	"tab":       {'\t'},
	"space":     {' '},
	"escape":    {0x1b},
	"esc":       {0x1b},
	"backspace": {0x7f},
	"bs":        {0x7f},
	"delete":    []byte("\x1b[3~"),
	"del":       []byte("\x1b[3~"),
	"insert":    []byte("\x1b[2~"),

	// Navigation.
	"up":       []byte("\x1b[A"),
	"down":     []byte("\x1b[B"),
	"right":    []byte("\x1b[C"),
	"left":     []byte("\x1b[D"),
	"home":     []byte("\x1b[H"),
	"end":      []byte("\x1b[F"),
	"pageup":   []byte("\x1b[5~"),
	"pgup":     []byte("\x1b[5~"),
	"pagedown": []byte("\x1b[6~"),
	"pgdn":     []byte("\x1b[6~"),

	// Function keys. F1-F4 are SS3 forms, which is what terminals actually send for them, while F5
	// upward are CSI. That inconsistency is in the terminals, not here.
	"f1":  []byte("\x1bOP"),
	"f2":  []byte("\x1bOQ"),
	"f3":  []byte("\x1bOR"),
	"f4":  []byte("\x1bOS"),
	"f5":  []byte("\x1b[15~"),
	"f6":  []byte("\x1b[17~"),
	"f7":  []byte("\x1b[18~"),
	"f8":  []byte("\x1b[19~"),
	"f9":  []byte("\x1b[20~"),
	"f10": []byte("\x1b[21~"),
	"f11": []byte("\x1b[23~"),
	"f12": []byte("\x1b[24~"),
}

// ParseKey resolves one key name into the bytes a terminal sends for it.
//
// Accepts a named key ("enter", "up", "f5"), a control combination ("ctrl-c", "c-c", "^C"), an
// alt/meta combination ("alt-x", "m-x"), or a single literal character. Case-insensitive for names and
// for the modified letter, since "C-C" and "c-c" mean the same key and a caller should not have to
// know that ctrl codes come from the lowercase letter.
//
// A single character is taken literally, so `--key a` sends "a". That makes the flag usable for one
// keystroke without a special case, and it is why an unrecognized multi-character name is an error
// rather than being sent as text: silently typing "ctrlc" because of a missing dash is the failure this
// whole file exists to remove.
func ParseKey(name string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("empty key name")
	}

	lower := strings.ToLower(strings.TrimSpace(name))
	if b, ok := namedKeys[lower]; ok {
		// Copied, so a caller cannot mutate the table through the returned slice.
		return append([]byte(nil), b...), nil
	}

	// Caret notation, as a terminal displays a control character: ^C.
	if rest, ok := strings.CutPrefix(name, "^"); ok && len(rest) == 1 {
		code, ok := ControlCode(rest[0])
		if !ok {
			return nil, fmt.Errorf("no control code exists for ^%c", rest[0])
		}
		return []byte{code}, nil
	}

	if rest, ok := cutModifier(lower, "ctrl-", "c-"); ok {
		// A named key after ctrl- resolves first, so "ctrl-space" is NUL and "ctrl-[" is escape rather
		// than either being rejected for not being one character. Both are real keystrokes with real
		// control codes, and a caller naming the key rather than the punctuation is the likelier form.
		if named, ok := namedKeys[rest]; ok && len(named) == 1 {
			if code, ok := ControlCode(named[0]); ok {
				return []byte{code}, nil
			}
		}
		if len(rest) != 1 {
			return nil, fmt.Errorf(
				"ctrl- takes a single character or a named key, got %q", rest)
		}
		code, ok := ControlCode(rest[0])
		if !ok {
			return nil, fmt.Errorf("no control code exists for ctrl-%c", rest[0])
		}
		return []byte{code}, nil
	}

	if rest, ok := cutModifier(lower, "alt-", "m-", "meta-"); ok {
		// ESC prefix, which is how a terminal sends a meta-modified key by default. The alternative,
		// setting the high bit, is a mode nothing enables now.
		//
		// Resolved recursively so "alt-enter" and "alt-up" work rather than only single letters.
		inner, err := ParseKey(rest)
		if err != nil {
			return nil, fmt.Errorf("alt-%s: %w", rest, err)
		}
		return append([]byte{0x1b}, inner...), nil
	}

	// A single character is itself. Measured in runes rather than bytes so a multi-byte character is
	// one key rather than a rejected name.
	if r := []rune(name); len(r) == 1 {
		return []byte(name), nil
	}

	return nil, fmt.Errorf(
		"unknown key %q; want a name like enter, tab, up, f5, a control key like ctrl-c, "+
			"an alt key like alt-x, or a single character", name)
}

// ParseKeys resolves several key names into one byte string, in order.
//
// Several rather than one, because a key sequence is usually what a caller means: interrupting and then
// pressing enter, or typing a control key and a letter. Concatenated with nothing between them, since
// that is what a terminal sends when someone presses them in turn.
func ParseKeys(names []string) ([]byte, error) {
	var out []byte
	for _, n := range names {
		b, err := ParseKey(n)
		if err != nil {
			return nil, err
		}
		out = append(out, b...)
	}
	return out, nil
}

// cutModifier strips whichever of the given prefixes s starts with.
func cutModifier(s string, prefixes ...string) (string, bool) {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest, true
		}
	}
	return "", false
}

// ControlCode returns the byte a terminal sends for ctrl plus the given character.
//
// Letters map to 1..26. The punctuation entries are the remaining control codes, which exist because
// ctrl-\ and ctrl-] are real choices a caller may need and would otherwise be unavailable.
//
// Exported so the detach-key parser and this share one definition. They had better agree: a user who
// configures a detach key and then sends the same key by name would otherwise be describing the same
// keystroke two ways, and only one of them would be right.
func ControlCode(c byte) (byte, bool) {
	// Uppercase folds to lowercase, since control codes come from the lowercase letter and a caller
	// writing ctrl-C means the same key.
	if c >= 'A' && c <= 'Z' {
		c += 'a' - 'A'
	}
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

// KeyNames lists the named keys, for help text and error messages.
func KeyNames() []string {
	out := make([]string, 0, len(namedKeys))
	for name := range namedKeys {
		out = append(out, name)
	}
	return out
}
