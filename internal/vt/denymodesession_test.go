//go:build cgo

package vt

import "testing"

// A session's terminal model must not claim a capability whose promise cm cannot keep.
//
// These are the tests that would have caught the two bugs, and they go through NewSessionTerminal
// rather than DenyModes so a fix that is not wired into the callback fails here. The unit tests beside
// them pass on the rewrite alone.
//
// Both modes are asked byte for byte what nvim sends at startup, and any answer other than 0 is a bug:
//
//   - Mode 69 makes nvim scroll one column range with DECSLRM, which cm forwards to a terminal that
//     ignores it, so the insert-line and delete-line operations apply full width and both halves of a
//     vertical split scroll. Measured in one kitty window: cm answered "\x1b[?69;2$y" where bare kitty
//     and zmx both answered "\x1b[?69;0$y".
//   - Mode 2048 makes nvim stop reacting to SIGWINCH and wait for in-band size reports that cm never
//     sends, so a window keeps its old height after a kitty split closes. Measured against a fake
//     terminal that answered the probe directly: told ";2" nvim emitted 0 bytes on both a shrink and a
//     grow, and told ";0" it emitted 4302 and 11206.
func TestSessionTerminalDeniesModes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{name: "left/right margins", query: "\x1b[?69$p", want: "\x1b[?69;0$y"},
		{name: "in-band size reports", query: "\x1b[?2048$p", want: "\x1b[?2048;0$y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := NewSessionTerminal(24, 80, 0)
			if err != nil {
				t.Fatalf("NewSessionTerminal() error = %v", err)
			}
			defer st.Close()

			if err := st.Write([]byte(tc.query)); err != nil {
				t.Fatalf("Write() error = %v", err)
			}

			pending := st.TakePending()
			if len(pending) != 1 {
				t.Fatalf("DECRQM %q produced %d replies, want 1: %q", tc.query, len(pending), pending)
			}
			// Ps 0 is "mode not recognized", which is what a terminal without the capability answers.
			if got := string(pending[0]); got != tc.want {
				t.Errorf("DECRQM %q reply = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}

// Enabling a denied mode does not change the answer.
//
// Worth asserting separately because the emulator really does implement both: after DECSET its honest
// report is ";1", so this is the case where the model and the reply deliberately disagree. A fix that
// passed the reply through whenever the mode was on would leave both bugs reachable by any program that
// sets the mode before asking.
func TestSessionTerminalDeniesModesWhenSet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write string
		want  string
	}{
		{name: "left/right margins", write: "\x1b[?69h\x1b[?69$p", want: "\x1b[?69;0$y"},
		{name: "in-band size reports", write: "\x1b[?2048h\x1b[?2048$p", want: "\x1b[?2048;0$y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := NewSessionTerminal(24, 80, 0)
			if err != nil {
				t.Fatalf("NewSessionTerminal() error = %v", err)
			}
			defer st.Close()

			if err := st.Write([]byte(tc.write)); err != nil {
				t.Fatalf("Write() error = %v", err)
			}

			pending := st.TakePending()
			if len(pending) != 1 {
				t.Fatalf("DECRQM produced %d replies, want 1: %q", len(pending), pending)
			}
			if got := string(pending[0]); got != tc.want {
				t.Errorf("DECRQM reply with the mode set = %q, want %q", got, tc.want)
			}
		})
	}
}

// Setting mode 2048 must not make the model emit an in-band size report of its own on resize.
//
// The other half of the 2048 bug, and the reason denying the mode is not enough on its own to reason
// about. libghostty can emit these reports, so if a resize ever produced one it would reach the pty as
// a notification cm does not otherwise send, and a program that had been told the mode is unavailable
// would receive a report it never asked for.
//
// Asserted through Resize rather than by reading libghostty's source, so a future version that starts
// emitting from this path fails here rather than silently changing what programs see.
func TestSessionTerminalResizeSendsNoSizeReport(t *testing.T) {
	st, err := NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer st.Close()

	// Set the mode, then discard the reply to it so only what the resize produces is left.
	if err := st.Write([]byte("\x1b[?2048h")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	st.TakePending()

	if err := st.Resize(40, 100); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	if pending := st.TakePending(); len(pending) != 0 {
		t.Errorf("resize with mode 2048 set produced %d writes to the pty, want 0: %q",
			len(pending), pending)
	}
}

// Other mode queries still report the model's own state.
//
// The control for the tests above: it shows they constrain the denied modes specifically rather than
// breaking DECRQM in general. Without this, replacing every DECRPM with ";0" would pass.
//
// 2026 is included because it is adjacent to 2048 in the place a bug would land: synchronized output is
// a mode cm genuinely owns, and denying it would make a TUI stop batching repaints.
func TestSessionTerminalStillReportsOtherModes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		// Autowrap, which libghostty has on by default, so the honest answer is ";1" and not ";0".
		{name: "autowrap", query: "\x1b[?7$p", want: "\x1b[?7;1$y"},
		// Synchronized output, off by default, so the honest answer is ";2".
		{name: "synchronized output", query: "\x1b[?2026$p", want: "\x1b[?2026;2$y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := NewSessionTerminal(24, 80, 0)
			if err != nil {
				t.Fatalf("NewSessionTerminal() error = %v", err)
			}
			defer st.Close()

			if err := st.Write([]byte(tc.query)); err != nil {
				t.Fatalf("Write() error = %v", err)
			}

			pending := st.TakePending()
			if len(pending) != 1 {
				t.Fatalf("DECRQM %q produced %d replies, want 1: %q", tc.query, len(pending), pending)
			}
			if got := string(pending[0]); got != tc.want {
				t.Errorf("DECRQM %q reply = %q, want %q", tc.query, got, tc.want)
			}
		})
	}
}
