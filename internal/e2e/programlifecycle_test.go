package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/ansi"
)

// TestProgramLifecycleAcrossAttachAndDetach drives a full-screen program's whole life against a real
// client, checking the client and the model agree at each stage.
//
// This is aimed at where the reported trouble actually is: programs starting and exiting, and the escape
// sequences they emit on the way in and out. A TUI does not just paint. It takes the alternate screen,
// enables mouse reporting, hides the cursor, pushes kitty keyboard flags, turns on synchronized output and
// bracketed paste, and then undoes all of it. Each of those is a mode cm has to carry into a restore and
// relay faithfully, and a single one handled wrongly leaves a shell with no cursor, or with mouse reporting
// on, or on the wrong screen.
//
// Checked with the differential oracle rather than against expected text: the client's own byte stream is
// replayed into a fresh emulator and compared with what the server's model renders. Nothing here says what
// the screen should look like, which is what makes it cheap to extend with another stage.
//
// The stages are the transitions, not the steady states. A steady state is what other tests cover; the
// transitions are where a mode gets set on one side and not the other.
func TestProgramLifecycleAcrossAttachAndDetach(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty")
	}

	// The mode set a real TUI uses, as one write per stage so each lands in its own pty read. Every one of
	// these has a bug behind it somewhere in docs/: 1049 the alternate screen, 1000/1002/1006 mouse,
	// 25 the cursor, >1u kitty keyboard, 2026 synchronized output, 2004 bracketed paste, 1004 focus.
	const (
		enter = `\033[?1049h\033[?1000h\033[?1002h\033[?1006h\033[?25l\033[>1u\033[?2026h\033[?2004h\033[?1004h`
		leave = `\033[?1004l\033[?2004l\033[?2026l\033[<u\033[?25h\033[?1006l\033[?1002l\033[?1000l\033[?1049l`
	)

	// Markers the test waits on. Each one means "the stage before this is fully delivered".
	const (
		shellReady  = "SHELL-READY"
		inProgram   = "IN-PROGRAM"
		backAtShell = "BACK-AT-SHELL"
	)

	script := strings.Join([]string{
		// Shell output first, so there is main-screen content that has to survive the program.
		`printf '` + shellReady + `\r\n'`,
		"sleep 0.4",
		// The program starts and paints.
		`printf '` + enter + `'`,
		"sleep 0.4",
		`printf '\033[2;1H\033[38:2:10:200:30m` + inProgram + `\033[m'`,
		"sleep 4",
		// And exits, restoring everything it changed.
		`printf '` + leave + `'`,
		`printf '` + backAtShell + `\r\n'`,
		"sleep 60",
	}, "; ")

	e := newEnvWith(t, cmHooksBinary(t), "")
	transcript := e.state + "/lifecycle.jsonl"

	c := attachOnPtyWithEnv(t, e,
		[]string{"CM_TESTHOOK_TRANSCRIPT=" + transcript},
		"lifecycle", "--", "/bin/sh", "-c", script)

	// Stage one: the shell, before any program. The client attached at creation, so this is a plain relay.
	waitForOnPty(t, c, shellReady)

	// Stage two: inside the program, on the alternate screen with every mode set.
	waitForOnPty(t, c, inProgram)
	time.Sleep(500 * time.Millisecond)
	assertClientRendersLikeModel(t, e, "lifecycle", transcript)

	// Nothing cm generated may have landed inside one of those mode sequences. They are short and arrive
	// back to back, which is exactly the shape that got split before.
	if problems := ansi.Validate(readTranscript(t, transcript)); len(problems) > 0 {
		for _, p := range problems {
			t.Errorf("%v", p)
		}
		t.Error("cm wrote into the middle of the program's mode sequences")
	}

	// Stage three: the program exits and puts everything back. This is the transition the reported
	// complaints are about, and the one where a client can be left on the wrong screen.
	waitForOnPty(t, c, backAtShell)
	time.Sleep(500 * time.Millisecond)
	assertClientRendersLikeModel(t, e, "lifecycle", transcript)

	// And the shell output from before the program is on screen again, which is what "quit vim and get your
	// shell back" means. Read from the model, since that is what any client should now be showing.
	if out := e.mustRun("read", "lifecycle"); !strings.Contains(out, shellReady) {
		t.Errorf("after the program exited, the session does not show the shell output from before it:\n%s",
			out)
	}
}

// TestReattachDuringAProgramThenItExits is the same transition, reached the way a person reaches it.
//
// Detach while a full-screen program is running, reattach, and quit the program. The reattaching client's
// restore blob describes the alternate screen, so nothing describes its main screen, and the `?1049l` on the
// way out switches the terminal onto whatever that window held. This is the everyday form of the bug: leave
// a session running vim, come back, quit vim.
//
// Distinct from the unit test in internal/server, which constructs the transition. This drives it through a
// real detach and a real reattach, where the resume position and the stored restore are also in play.
func TestReattachDuringAProgramThenItExits(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty")
	}

	const (
		shellMark   = "SHELL-BEFORE-VIM"
		programMark = "INSIDE-THE-PROGRAM"
		afterMark   = "SHELL-AFTER-VIM"
	)

	script := strings.Join([]string{
		`printf '` + shellMark + `\r\n'`,
		"sleep 0.4",
		`printf '\033[?1049h\033[?25l'`,
		`printf '\033[3;1H` + programMark + `'`,
		// Long enough for a detach and a reattach to happen inside it.
		"sleep 8",
		`printf '\033[?25h\033[?1049l'`,
		`printf '` + afterMark + `\r\n'`,
		"sleep 60",
	}, "; ")

	e := newEnvWith(t, cmHooksBinary(t), "")

	first := attachOnPty(t, e, "reattach", "--", "/bin/sh", "-c", script)
	waitForOnPty(t, first, programMark)

	// Detach with the key, which is the real path: it goes through the client's own teardown rather than a
	// dropped connection.
	first.detachKey()
	e.waitFor("the first client to detach", 20*time.Second, func() bool {
		return e.sessionDetail(t, "reattach").Clients == 0
	})

	// Reattach while the program is still running. This client has no main-screen content.
	transcript := e.state + "/reattach.jsonl"
	second := attachOnPtyWithEnv(t, e,
		[]string{"CM_TESTHOOK_TRANSCRIPT=" + transcript},
		"reattach")
	waitForOnPty(t, second, programMark)

	// The program exits. The reattached client has to end up showing the session's shell, which means being
	// repainted, since its own main screen never held the session's content.
	waitForOnPty(t, second, afterMark)
	time.Sleep(1 * time.Second)

	if got := second.output(); !strings.Contains(got, shellMark) {
		t.Errorf("after the program exited, the reattached client never received the shell output from "+
			"before it started, so its screen shows whatever its window held before the attach.\n"+
			"client saw:\n%s", got)
	}
	assertClientRendersLikeModel(t, e, "reattach", transcript)
}
