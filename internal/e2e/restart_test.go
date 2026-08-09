package e2e

import (
	"strings"
	"testing"
	"time"
)

// A session must survive the server exiting, and come back with its scrollback.
//
// This is the bug no unit test could see. The terminal model lives in the server, so a new server
// starts with a blank one, and adoption resumed from the end of what the previous server had consumed.
// The shell kept running and new output flowed, so everything looked fine except that the screen was
// empty: `cm history` returned nothing and a reattaching client got a blank terminal.
//
// Nothing below the process boundary can catch this. The manager-level tests construct one Manager and
// call Reconcile on it; the defect is in what a *second server process* inherits from the store.
func TestSessionSurvivesServerRestartWithScrollback(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "keeper", "-d", "--",
		"/bin/sh", "-c", "echo BEFORE_RESTART; sleep 120")
	e.waitForHistory("keeper", "BEFORE_RESTART")

	before, ok := e.session("keeper")
	if !ok {
		t.Fatal("session was not created")
	}

	e.restartServer()

	// Same shell, still running: the pty is held by the shim, not the server.
	after, ok := e.session("keeper")
	if !ok {
		t.Fatal("the session did not survive the server restart")
	}
	if after.State != "running" {
		t.Errorf("state after restart = %q, want running", after.State)
	}
	if after.ShellPID != before.ShellPID {
		t.Errorf("shell pid changed across the restart: %d -> %d, want the same process",
			before.ShellPID, after.ShellPID)
	}

	// And the screen came back. This is the assertion the bug failed.
	got := e.run("history", "keeper").stdout
	if !strings.Contains(got, "BEFORE_RESTART") {
		t.Errorf("history after restart = %q, want it to still contain the pre-restart output", got)
	}
	// Exactly once, since replaying past the resume point duplicates output.
	if n := strings.Count(got, "BEFORE_RESTART"); n != 1 {
		t.Errorf("the pre-restart line appears %d times, want exactly 1: %q", n, got)
	}
}

// The session must still be usable after adoption, not merely visible.
//
// Separated from the scrollback assertion because they fail independently: a session can list as
// running while its output no longer flows, and a screen can be restored while input goes nowhere.
func TestAdoptedSessionStillAcceptsInput(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "live", "-d", "--", "/bin/sh", "-c", "echo READY; cat")
	e.waitForHistory("live", "READY")

	e.restartServer()

	e.mustRun("send", "live", "AFTER_ADOPTION\n")
	got := e.waitForHistory("live", "AFTER_ADOPTION")

	// Both halves are present: the replayed history and the new output.
	if !strings.Contains(got, "READY") {
		t.Errorf("history after adoption = %q, want the pre-restart output", got)
	}

	// The new input appears twice, and that is correct rather than a duplication bug: a pty echoes what
	// is written to it, and `cat` then prints the same line again. Verified without any restart
	// involved, so it is terminal behavior and not something adoption caused. An earlier version of
	// this test asserted "exactly once" and failed for that reason.
	//
	// The scrollback assertions that do check for duplication use a command that only echoes, where one
	// occurrence is the correct answer.
	if n := strings.Count(got, "AFTER_ADOPTION"); n != 2 {
		t.Errorf("the new input appears %d times, want 2 (the pty echo and cat's output): %q", n, got)
	}
}

// Repeated restarts must not compound. Each one replays and hands over again, so an off-by-one at the
// boundary would accumulate rather than stay a single glitch.
func TestScrollbackSurvivesRepeatedRestarts(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "many", "-d", "--",
		"/bin/sh", "-c", "echo FIRST_LINE; sleep 120")
	e.waitForHistory("many", "FIRST_LINE")

	for i := range 3 {
		e.restartServer()
		got := e.run("history", "many").stdout
		if n := strings.Count(got, "FIRST_LINE"); n != 1 {
			t.Fatalf("after restart %d the line appears %d times, want exactly 1: %q", i+1, n, got)
		}
	}
}

// Stopping the server must leave sessions running, and report cleanly when none is running.
//
// `cm server stop` exists because this is the upgrade path rather than an exceptional operation, and
// before it the only way to stop a server was to find and kill its pid.
func TestServerStopLeavesSessionsRunning(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// Stopping when nothing is running is the state the caller asked for, not an error.
	//
	// newEnv starts a server, so this stops that one, and the wait matters: without it the next command
	// starts a fresh server while the old one is still shutting down, and they race over the socket.
	e.mustRun("server", "stop")
	e.waitServerGone()
	if r := e.run("server", "stop"); r.code != 0 {
		t.Errorf("server stop with no server exited %d, want 0: %s", r.code, r.stderr)
	}

	e.mustRun("run", "--session", "stopme", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the session to be running", 10*time.Second, func() bool {
		s, ok := e.session("stopme")
		return ok && s.State == "running"
	})

	e.restartServer()

	if s, ok := e.session("stopme"); !ok || s.State != "running" {
		t.Errorf("session after server stop = %+v (found=%v), want it still running", s, ok)
	}
}
