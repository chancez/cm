package e2e

import (
	"os/exec"
	"testing"
	"time"
)

// A real shell's own hooks put the command it is running into `cm list --json`.
//
// End to end because every hop is a separate thing that can be wrong and none of them is the feature: the
// integration has to install its hooks in an interactive shell, the shell has to emit the sequence to the
// pty, the server's pump has to parse it out of ordinary output, the session has to hold the stack, and the
// CLI has to render it. The unit tests cover each hop with a constructed value, which is exactly how a chain
// that is broken in the middle passes all of them.
//
// zsh rather than /bin/sh, because the hooks are what is under test and dash has none. Installed in the
// Linux test image for the same reason the OSC 133 tests need it.
func TestLocationComesFromTheShellsOwnHooks(t *testing.T) {
	skipIfShort(t)
	if _, err := exec.LookPath("zsh"); err != nil {
		t.Skip("zsh is not installed")
	}
	e := newEnv(t)

	// -f so the machine's own zsh configuration cannot decide what this test sees.
	sess := attachOnPty(t, e, "loc", "--", "zsh", "-f", "-i")

	// No waitReady here, and that is deliberate: it waits for a prompt spelled "$", while zsh -f prints
	// "host% ". Waiting for output this test arranged is the rule that thread arrived at, and the pty buffers
	// what is typed before the shell reads it, so nothing is lost by typing first.
	//
	// The session's shell has CM_SESSION from the shim, so the integration's own guard passes and nothing has
	// to be arranged for it.
	e.waitFor("the session to start its shell", 20*time.Second, func() bool {
		s, ok := e.session("loc")
		return ok && s.ShellPID != 0
	})

	sess.typeLine(`eval "$(` + e.bin + ` shell-init zsh)"`)

	// Printed in pieces, so what is waited for appears in the command's output rather than in the shell's
	// echo of the line. Waiting for a string the echo contains stops the read before anything has run, which
	// is the same trap the pty test in internal/shellinit records.
	sess.typeLine(`printf 'LOAD%s\n' DONE`)
	sess.waitForOutput("LOADDONE", 20*time.Second)

	if s, ok := e.session("loc"); !ok || len(s.Location) != 0 {
		t.Fatalf("session = %+v, want no location at a prompt", s)
	}

	// A command that stays running, so the frame is observable while it is open rather than in a race with
	// its own close.
	sess.typeLine("sleep 30")
	e.waitFor("the location to name the running command", 20*time.Second, func() bool {
		s, ok := e.session("loc")
		return ok && len(s.Location) == 1 && s.Location[0].Argv == "sleep 30"
	})

	// And the frame closes when the command ends, which is the half that makes the stack a location rather
	// than a growing list.
	e.mustRun("signal", "loc", "int")
	e.waitFor("the location to empty when the command returned", 20*time.Second, func() bool {
		s, ok := e.session("loc")
		return ok && len(s.Location) == 0
	})
}
