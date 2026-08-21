//go:build cgo

package vt

import "testing"

// A session's terminal model must not claim left/right margin support, because the real terminal
// behind cm usually has none.
//
// This is the test that would have caught the bug, and it goes through NewSessionTerminal rather than
// DenyMarginMode so a fix that is not wired into the callback fails here. The unit test beside it
// passes on the rewrite alone.
//
// The symptom was nvim scrolling both halves of a vertical split. nvim asks this exact question once
// at startup, and any answer other than 0 makes it use DECSLRM to scroll one column range; cm then
// forwards that to a terminal that ignores it, so the insert-line and delete-line operations apply
// full width. Measured in one kitty window: cm answered "\x1b[?69;2$y" where bare kitty and zmx both
// answered "\x1b[?69;0$y".
func TestSessionTerminalDeniesMarginMode(t *testing.T) {
	st, err := NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer st.Close()

	// DECRQM for private mode 69, byte for byte what nvim sends.
	if err := st.Write([]byte("\x1b[?69$p")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	pending := st.TakePending()
	if len(pending) != 1 {
		t.Fatalf("DECRQM ?69 produced %d replies, want 1: %q", len(pending), pending)
	}
	// Ps 0 is "mode not recognized", which is what a terminal without the capability answers.
	if got, want := string(pending[0]), "\x1b[?69;0$y"; got != want {
		t.Errorf("DECRQM ?69 reply = %q, want %q", got, want)
	}
}

// Enabling the mode does not change the answer.
//
// Worth asserting separately because the emulator really does implement margins: after DECSET 69 its
// honest report is ";1", so this is the case where the model and the reply deliberately disagree. A
// fix that passed the reply through whenever the mode was on would leave the bug reachable by any
// program that sets the mode before asking.
func TestSessionTerminalDeniesMarginModeWhenSet(t *testing.T) {
	st, err := NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer st.Close()

	if err := st.Write([]byte("\x1b[?69h\x1b[?69$p")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	pending := st.TakePending()
	if len(pending) != 1 {
		t.Fatalf("DECRQM ?69 produced %d replies, want 1: %q", len(pending), pending)
	}
	if got, want := string(pending[0]), "\x1b[?69;0$y"; got != want {
		t.Errorf("DECRQM ?69 reply with the mode set = %q, want %q", got, want)
	}
}

// Other mode queries still report the model's own state.
//
// The control for the two tests above: it shows they constrain mode 69 specifically rather than
// breaking DECRQM in general. Without this, replacing every DECRPM with ";0" would pass.
func TestSessionTerminalStillReportsOtherModes(t *testing.T) {
	st, err := NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer st.Close()

	// Autowrap, which libghostty has on by default, so the honest answer is ";1" and not ";0".
	if err := st.Write([]byte("\x1b[?7$p")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	pending := st.TakePending()
	if len(pending) != 1 {
		t.Fatalf("DECRQM ?7 produced %d replies, want 1: %q", len(pending), pending)
	}
	if got, want := string(pending[0]), "\x1b[?7;1$y"; got != want {
		t.Errorf("DECRQM ?7 reply = %q, want %q", got, want)
	}
}
