package input

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want []byte
	}{
		// Control combinations, in every spelling a caller might reach for. All four of these were
		// previously sent as literal text, which is the failure this parser exists to remove.
		{name: "ctrl dash", key: "ctrl-c", want: []byte{0x03}},
		{name: "c dash", key: "c-c", want: []byte{0x03}},
		{name: "caret", key: "^C", want: []byte{0x03}},
		{name: "caret lowercase", key: "^c", want: []byte{0x03}},
		// Uppercase folds, since the control code comes from the lowercase letter and a caller writing
		// ctrl-C means the same keystroke.
		{name: "ctrl uppercase", key: "ctrl-C", want: []byte{0x03}},
		{name: "ctrl-d", key: "ctrl-d", want: []byte{0x04}},
		{name: "ctrl-z", key: "ctrl-z", want: []byte{0x1a}},
		// Punctuation control codes, which exist because they are real choices.
		{name: "ctrl-backslash", key: `ctrl-\`, want: []byte{0x1c}},
		{name: "ctrl-bracket", key: "ctrl-[", want: []byte{0x1b}},
		// A named key after ctrl- resolves to that key's control code: space is NUL, and ctrl-tab is
		// ctrl-i since tab already is one.
		{name: "ctrl-space", key: "ctrl-space", want: []byte{0x00}},

		// Named keys.
		{name: "enter", key: "enter", want: []byte{'\r'}},
		{name: "return", key: "return", want: []byte{'\r'}},
		{name: "tab", key: "tab", want: []byte{'\t'}},
		{name: "escape", key: "escape", want: []byte{0x1b}},
		{name: "backspace", key: "backspace", want: []byte{0x7f}},
		{name: "case insensitive", key: "Enter", want: []byte{'\r'}},

		// Navigation, in the CSI forms a terminal sends when nothing has changed modes.
		{name: "up", key: "up", want: []byte("\x1b[A")},
		{name: "left", key: "left", want: []byte("\x1b[D")},
		{name: "home", key: "home", want: []byte("\x1b[H")},
		{name: "pageup", key: "pageup", want: []byte("\x1b[5~")},
		{name: "delete", key: "delete", want: []byte("\x1b[3~")},

		// Function keys. F1-F4 are SS3 and F5 up are CSI, which is what terminals send.
		{name: "f1", key: "f1", want: []byte("\x1bOP")},
		{name: "f5", key: "f5", want: []byte("\x1b[15~")},
		{name: "f12", key: "f12", want: []byte("\x1b[24~")},

		// Alt/meta, sent as an ESC prefix.
		{name: "alt letter", key: "alt-x", want: []byte{0x1b, 'x'}},
		{name: "m dash", key: "m-x", want: []byte{0x1b, 'x'}},
		{name: "meta", key: "meta-x", want: []byte{0x1b, 'x'}},
		// Resolved recursively, so a modifier composes with a named key rather than only a letter.
		{name: "alt named key", key: "alt-enter", want: []byte{0x1b, '\r'}},
		{name: "alt arrow", key: "alt-up", want: []byte("\x1b\x1b[A")},

		// A single character is itself, so one keystroke needs no special case.
		{name: "single letter", key: "a", want: []byte("a")},
		{name: "single digit", key: "7", want: []byte("7")},
		{name: "single punctuation", key: "/", want: []byte("/")},
		// A multi-byte character is one key rather than a rejected name.
		{name: "multibyte character", key: "é", want: []byte("é")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseKey(tc.key)
			if err != nil {
				t.Fatalf("ParseKey(%q) error = %v", tc.key, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseKey(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// An unrecognized multi-character name is an error, not literal text.
//
// This is the whole point. Sending "ctrlc" as five characters because a dash was missed is exactly the
// silent failure that made this parser necessary: a script believes it interrupted a build and instead
// typed a word onto the command line.
func TestParseKeyRejectsUnknownNames(t *testing.T) {
	for _, key := range []string{
		"ctrlc",     // missing dash
		"control-c", // not a spelling this accepts
		"ctrl-",     // no key after the modifier
		"ctrl-abc",  // more than one character
		"f13",       // beyond the table
		"nosuchkey",
		"",
	} {
		got, err := ParseKey(key)
		if err == nil {
			t.Errorf("ParseKey(%q) = %v with no error, want it refused", key, got)
		}
	}
}

// The error names what is accepted, since a rejected key is usually a spelling a caller has to fix.
func TestParseKeyErrorIsActionable(t *testing.T) {
	_, err := ParseKey("nosuchkey")
	if err == nil {
		t.Fatal("ParseKey() = nil error, want one")
	}
	for _, want := range []string{"enter", "ctrl-c", "alt-"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// No control code exists for every character, and that is reported rather than guessed at.
func TestParseKeyRejectsImpossibleControlCodes(t *testing.T) {
	for _, key := range []string{"ctrl-1", "ctrl-!", "^1"} {
		if _, err := ParseKey(key); err == nil {
			t.Errorf("ParseKey(%q) = nil error, want it refused", key)
		}
	}
}

func TestParseKeys(t *testing.T) {
	// A sequence is what a caller usually means: interrupt, then press enter.
	got, err := ParseKeys([]string{"ctrl-c", "enter"})
	if err != nil {
		t.Fatalf("ParseKeys() error = %v", err)
	}
	if want := []byte{0x03, '\r'}; !reflect.DeepEqual(got, want) {
		t.Errorf("ParseKeys() = %v, want %v", got, want)
	}

	// Concatenated with nothing between them, which is what a terminal sends for keys pressed in turn.
	got, err = ParseKeys([]string{"h", "i", "enter"})
	if err != nil {
		t.Fatalf("ParseKeys() error = %v", err)
	}
	if want := []byte("hi\r"); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseKeys() = %q, want %q", got, want)
	}

	// One bad name fails the whole call rather than sending part of it. A partially delivered key
	// sequence is worse than none: it leaves the session in a state the caller did not intend.
	if _, err := ParseKeys([]string{"ctrl-c", "nosuchkey"}); err == nil {
		t.Error("ParseKeys() with a bad name = nil error, want it refused")
	}
}

// The returned bytes must not alias the table, or a caller could corrupt it for every later call.
func TestParseKeyDoesNotAliasTheTable(t *testing.T) {
	first, err := ParseKey("enter")
	if err != nil {
		t.Fatalf("ParseKey() error = %v", err)
	}
	first[0] = 'X'

	second, err := ParseKey("enter")
	if err != nil {
		t.Fatalf("ParseKey() error = %v", err)
	}
	if second[0] != '\r' {
		t.Errorf("ParseKey(\"enter\") = %q after a caller mutated an earlier result, want %q",
			second, "\r")
	}
}

// ControlCode is shared with the detach-key parser, so the two describe the same keystroke.
//
// They had better agree: a user who configures a detach key and then sends that key by name would
// otherwise be naming one keystroke two ways, with only one of them right.
func TestControlCode(t *testing.T) {
	tests := []struct {
		in   byte
		want byte
		ok   bool
	}{
		{in: 'a', want: 0x01, ok: true},
		{in: 'c', want: 0x03, ok: true},
		{in: 'z', want: 0x1a, ok: true},
		// Uppercase folds, which is the behavior the send path needs and the detach parser gets for free.
		{in: 'C', want: 0x03, ok: true},
		{in: '@', want: 0x00, ok: true},
		{in: '[', want: 0x1b, ok: true},
		{in: '\\', want: 0x1c, ok: true},
		{in: ']', want: 0x1d, ok: true},
		{in: '?', want: 0x7f, ok: true},
		{in: '1', ok: false},
		{in: '!', ok: false},
	}
	for _, tc := range tests {
		got, ok := ControlCode(tc.in)
		if ok != tc.ok {
			t.Errorf("ControlCode(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("ControlCode(%q) = %#x, want %#x", tc.in, got, tc.want)
		}
	}
}

// Every named key must produce something, or the help text advertises a key that fails.
func TestEveryNamedKeyParses(t *testing.T) {
	for _, name := range KeyNames() {
		got, err := ParseKey(name)
		if err != nil {
			t.Errorf("ParseKey(%q) error = %v, but it is listed as a key name", name, err)
			continue
		}
		if len(got) == 0 {
			t.Errorf("ParseKey(%q) returned no bytes", name)
		}
	}
}
