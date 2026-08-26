//go:build cgo

package vt

import "testing"

// KittyKeyboardProtocol has to track what the program actually did, since the server drops keyboard
// events on the strength of it.
//
// Goes through NewSessionTerminal rather than the model directly, so a fix that is not wired into the
// adapter fails here. The sequences are the ones codex really emits, captured from a pty: it pushes
// flags 7 at startup and pops twice on exit, and the second pop is against an empty stack. Both real
// kitty and libghostty reset to zero on that rather than ignoring it, measured, which is what makes the
// state after a program exits unambiguous.
func TestKittyKeyboardProtocolTracksTheProgram(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  string
		want bool
	}{
		{"fresh session", "", false},
		{"codex has pushed flags 7", "\x1b[>7u", true},
		{"codex has exited", "\x1b[>7u\x1b[<1u\x1b[<u", false},
		{"pushed with the set form", "\x1b[=13;1u", true},
		{"set back to zero", "\x1b[=13;1u\x1b[=0;1u", false},
		{"pushed on the alternate screen", "\x1b[?1049h\x1b[>7u", true},
		// A pop with nothing pushed must not be read as "on", which would disable the filter for the
		// whole session.
		{"pop with an empty stack", "\x1b[<u", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := NewSessionTerminal(24, 80, 0)
			if err != nil {
				t.Fatalf("NewSessionTerminal() error = %v", err)
			}
			defer st.Close()

			if tc.seq != "" {
				if err := st.Write([]byte(tc.seq)); err != nil {
					t.Fatalf("Write(%q) error = %v", tc.seq, err)
				}
			}
			if got := st.KittyKeyboardProtocol(); got != tc.want {
				t.Errorf("after %q, KittyKeyboardProtocol() = %v, want %v.\n"+
					"The server drops stale keyboard events on this answer, so a false negative makes a "+
					"session ignore releases a program asked for and a false positive lets a stale one "+
					"reach the shell.", tc.seq, got, tc.want)
			}
		})
	}
}
