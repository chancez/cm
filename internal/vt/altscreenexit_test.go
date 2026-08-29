package vt

import (
	"strings"
	"testing"
)

// TestRestoreOnTheAlternateScreenSurvivesTheProgramExiting asks what a client sees when a full-screen
// program quits *after* that client attached.
//
// The situation is ordinary: a session is running vim, a client attaches, and then vim quits. Attaching
// replays a serialized screen, and for a session on the alternate screen that blob describes the alternate
// screen, because that is where the model is. Nothing populates the client's main screen. So when the
// program sends `?1049l` on the way out, the terminal pops to a main screen that this client's window
// happens to hold, which is whatever was there before cm was attached.
//
// docs/restore.md records the mirror image of this being fixed: a main-screen blob said nothing about which
// screen it belonged to, so a repaint could land on a client's alternate screen and leave a program's
// display behind after it quit. This is the other direction, and the doc does not claim it.
//
// Asserted against the same model rather than against a written-down expectation: terminal A is the
// session, terminal B is the attaching client. Both are given `?1049l` at the end, so the only difference
// between them is what the restore carried, which is exactly what is under test.
func TestRestoreOnTheAlternateScreenSurvivesTheProgramExiting(t *testing.T) {
	const rows, cols = 10, 40

	// The session: shell output, then a program taking the alternate screen and painting it.
	session, err := NewSessionTerminal(rows, cols, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	const shellLine = "SHELL-BEFORE-THE-PROGRAM"
	const tuiLine = "TUI-ON-THE-ALT-SCREEN"
	if err := session.Write([]byte(shellLine + "\r\n" + "\x1b[?1049h" + "\x1b[H" + tuiLine)); err != nil {
		t.Fatal(err)
	}

	// A client attaches now, while the program is still running.
	blob, err := session.Restore()
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	client, err := NewSessionTerminal(rows, cols, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// The window this client is in already had something on it, which is the whole point: a terminal a
	// person is using is never blank, and this is the content that must not survive.
	const stale = "STALE-CONTENT-FROM-THIS-WINDOW"
	if err := client.Write([]byte(stale + "\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := client.Write(blob); err != nil {
		t.Fatalf("replaying the restore blob: %v", err)
	}

	// Sanity: the client is showing the program, or nothing below is about the right thing.
	shown, err := client.Tail(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shown), tuiLine) {
		t.Fatalf("the attached client is not showing the program: %q", shown)
	}

	// The program quits. Both the session and the client receive it, because cm relays the session's bytes.
	const exit = "\x1b[?1049l"
	if err := session.Write([]byte(exit)); err != nil {
		t.Fatal(err)
	}
	if err := client.Write([]byte(exit)); err != nil {
		t.Fatal(err)
	}

	wantTail, err := session.Tail(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	gotTail, err := client.Tail(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	want, got := string(wantTail), string(gotTail)

	// The client must not be left showing content from its own window. That is the visible symptom: quit
	// vim and the screen fills with something from before the attach.
	if strings.Contains(got, stale) {
		t.Errorf("after the program quit, the client shows content from its own window before it "+
			"attached.\nclient:\n%s\nsession:\n%s", got, want)
	}
	// What it is *not* showing is the session's own main screen, and that cannot be fixed here.
	//
	// The blob can only describe the active screen: libghostty serializes that one, and
	// GhosttyTerminalScreen is a read of which screen is active rather than a selector, so there is no way
	// to put the session's main screen into an alternate-screen blob. Clearing is the whole of what this
	// layer can do.
	//
	// The rest is the server's, and it is fixed there rather than left undone:
	// TestClientAttachedDuringAFullScreenProgramIsRepaintedWhenItQuits covers it. When the model
	// leaves the alternate screen, clients that attached during the program are flagged for a repaint,
	// which reattaches them and hands them a blob describing the main screen.
	//
	// Asserted as current behaviour rather than left as a comment, so a future change that does carry the
	// main screen in the blob fails here and has to update this on purpose.
	if strings.Contains(got, shellLine) {
		t.Errorf("the blob now carries the session's main screen, which this layer had no way to do. "+
			"That is an improvement: update this test, and check whether the server's repaint on leaving "+
			"the alternate screen is still needed.\nclient:\n%s\nsession:\n%s", got, want)
	}
}
