package input

import "testing"

func TestIsUserInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// Typing.
		{"printable", "a", true},
		{"word", "ls -la", true},
		{"return", "\r", true},
		{"newline", "\n", true},
		{"tab", "\t", true},
		{"backspace", "\x7f", true},
		{"ctrl-c", "\x03", true},
		{"escape alone", "\x1b", false}, // incomplete, so undecidable
		{"alt-key", "\x1bb", true},
		{"multibyte rune", "\xc3\xa9", true},
		{"arrow up", "\x1b[A", true},
		{"modified arrow", "\x1b[1;5C", true},
		{"function key", "\x1b[15~", true},
		{"application cursor key", "\x1bOA", true},
		{"kitty key press", "\x1b[97;5u", true},
		{"modifyOtherKeys", "\x1b[27;5;92~", true},

		// Not typing. These are the cases that matter: each one would otherwise let a window nobody
		// touched take over the session's size.
		{"empty", "", false},
		{"cursor position report", "\x1b[24;80R", false},
		{"device attributes", "\x1b[?62;22c", false},
		{"device status report", "\x1b[0n", false},
		{"window size report", "\x1b[8;24;80t", false},
		{"focus in", "\x1b[I", false},
		{"focus out", "\x1b[O", false},
		{"sgr mouse press", "\x1b[<0;10;20M", false},
		{"sgr mouse release", "\x1b[<0;10;20m", false},
		{"sgr mouse motion", "\x1b[<35;10;20M", false},
		{"x10 mouse", "\x1b[M !!", false},
		{"osc clipboard reply", "\x1b]52;c;aGk=\x07", false},
		{"dcs reply", "\x1bP>|kitty\x1b\\", false},

		// A key *release* alone should not claim sizing: letting go of a key in a window you are
		// leaving is not a reason to take it over.
		{"kitty key release", "\x1b[97;5:3u", false},
		{"kitty key repeat", "\x1b[97;5:2u", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUserInput([]byte(tt.input)); got != tt.want {
				t.Errorf("IsUserInput(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// A mouse report followed by real typing must still register as typing, since a terminal batches
// whatever arrived together.
func TestIsUserInputMixedBuffer(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"mouse then key", "\x1b[<0;1;1Ma", true},
		{"focus then key", "\x1b[Ix", true},
		{"report then key", "\x1b[24;80Rq", true},
		{"several reports only", "\x1b[I\x1b[24;80R\x1b[0n", false},
		{"x10 mouse then key", "\x1b[M !!z", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsUserInput([]byte(tt.input)); got != tt.want {
				t.Errorf("IsUserInput(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// An x10 mouse report carries raw coordinate bytes that can look printable. They must be consumed
// with the sequence, or a mouse move over an idle window would read as typing.
func TestX10MouseCoordinatesAreConsumed(t *testing.T) {
	// Coordinates chosen to be printable ASCII, which is the dangerous case.
	if got := IsUserInput([]byte("\x1b[M" + "ABC")); got {
		t.Error("x10 mouse coordinates were read as typing")
	}
}

// An incomplete sequence must not be guessed at, since the remainder arrives in the next read and
// guessing could only produce a false positive.
func TestIncompleteSequencesAreNotTyping(t *testing.T) {
	for _, in := range []string{"\x1b", "\x1b[", "\x1b[97;5", "\x1b]52;c", "\x1bP>|kit"} {
		if got := IsUserInput([]byte(in)); got {
			t.Errorf("IsUserInput(%q) = true for an incomplete sequence, want false", in)
		}
	}
}
