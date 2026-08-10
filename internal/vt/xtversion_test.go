package vt

import (
	"strings"
	"testing"

	"github.com/chancez/cm/internal/paths"
)

// XTVERSION must name cm rather than the emulator it embeds.
//
// A program asking CSI > q wants to know what terminal it is talking to, so it can decide which
// features to use. Unwired, libghostty answers "libghostty", which is true about the emulator and
// misleading about the terminal: nothing a program can act on, and it hides the fact that a
// multiplexer holds the pty.
//
// Only reaches a program when nothing is attached, since an attached terminal answers this itself.
// That is the case where cm really is the terminal.
func TestXtversionReportsCM(t *testing.T) {
	SetXtversion("cm 1.2.3")

	var replies [][]byte
	term, err := New(24, 80, Callbacks{
		WritePty:        func(d []byte) { replies = append(replies, append([]byte(nil), d...)) },
		ReportXtversion: true,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.Write([]byte("\x1b[>q")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if len(replies) != 1 {
		t.Fatalf("XTVERSION produced %d replies, want 1: %q", len(replies), replies)
	}
	// DCS > | <text> ST is the XTVERSION reply form.
	if got, want := string(replies[0]), "\x1bP>|cm 1.2.3\x1b\\"; got != want {
		t.Errorf("XTVERSION reply = %q, want %q", got, want)
	}
}

// With reporting off, the reply falls back to libghostty's own default rather than becoming empty.
//
// Degrading to the previous behavior matters more than the value: an empty version string is
// something a program may not expect, while "libghostty" is at least a name.
//
// Independent of what SetXtversion recorded, and deliberately so. ReportXtversion false means the
// callback is never installed, so libghostty answers on its own and the shared buffer is not
// consulted. That is what keeps this test honest when another test in the package has already set a
// value, which it has, since the buffer is process-wide.
func TestXtversionUnsetFallsBackToDefault(t *testing.T) {
	var replies [][]byte
	term, err := New(24, 80, Callbacks{
		WritePty: func(d []byte) { replies = append(replies, append([]byte(nil), d...)) },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.Write([]byte("\x1b[>q")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if len(replies) != 1 {
		t.Fatalf("XTVERSION produced %d replies, want 1: %q", len(replies), replies)
	}
	if got := string(replies[0]); !strings.Contains(got, "libghostty") {
		t.Errorf("XTVERSION reply with no version set = %q, want it to mention libghostty", got)
	}
}

// A session's terminal model reports cm and its real version, which is what a program in a detached
// session sees. Asserted through the constructor the server actually uses, so a version that never
// reached the emulator would fail here.
func TestSessionTerminalReportsCMVersion(t *testing.T) {
	st, err := NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer st.Close()

	if err := st.Write([]byte("\x1b[>q")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	pending := st.TakePending()
	if len(pending) != 1 {
		t.Fatalf("XTVERSION produced %d replies, want 1: %q", len(pending), pending)
	}
	want := "\x1bP>|" + paths.Name + " " + paths.Version() + "\x1b\\"
	if got := string(pending[0]); got != want {
		t.Errorf("session terminal XTVERSION reply = %q, want %q", got, want)
	}
}
