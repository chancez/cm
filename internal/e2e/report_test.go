package e2e

import (
	"strings"
	"testing"
	"time"
)

// A program in a session can report what it is doing, and a script can wait for it.
//
// This is the whole point of the mechanism, and nothing about it is specific to any program: cm never
// learns what is running. A coding agent's hook, a build script, and a test runner all make the same call,
// which is what keeps this from needing per-program knowledge that goes stale.
func TestReportMakesBlockedWaitable(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// A plain shell, with no OSC 133 and no agent: whatever reports is irrelevant to cm.
	e.mustRun("run", "--session", "worker", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("worker")
		return ok && s.State == "running"
	})

	// Nothing has reported, so blocked cannot be reached: a shell marks a command as running whether it
	// is computing or waiting, so this state can only come from a report.
	if r := e.run("wait", "worker", "--until", "blocked", "--timeout", "300ms"); r.code == 0 {
		t.Error("wait --until blocked succeeded before anything reported it")
	}

	e.mustRun("report", "worker", "--state", "blocked",
		"--detail", "needs approval", "--source", "some-tool")

	s, ok := e.session("worker")
	if !ok {
		t.Fatal("session disappeared")
	}
	if s.ReportedState != "blocked" || s.ReportedDetail != "needs approval" {
		t.Errorf("session = %+v, want it reported blocked with a detail", s)
	}
	// A report replaces the derived state, so busy reflects it: a caller reading one field gets the better
	// answer rather than having to know which to prefer.
	if !s.Busy {
		t.Errorf("session = %+v, want busy to reflect the report", s)
	}

	// And the wait now succeeds.
	e.mustRun("wait", "worker", "--until", "blocked", "--timeout", "10s")

	// Clearing falls back to what cm can see for itself.
	e.mustRun("report", "worker", "--state", "clear")
	s, _ = e.session("worker")
	if s.ReportedState != "" {
		t.Errorf("session = %+v, want the report cleared", s)
	}
}

// A wait must be released by a report that arrives while it is waiting.
//
// The shape a script coordinating with an agent uses: let it work, then block until it needs something.
func TestReportReleasesAWaitingCaller(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "agent", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("agent")
		return ok && s.State == "running"
	})
	e.mustRun("report", "agent", "--state", "busy", "--detail", "reviewing")

	// The report arrives while the wait is already blocked.
	go func() {
		time.Sleep(700 * time.Millisecond)
		e.run("report", "agent", "--state", "blocked", "--detail", "which branch?")
	}()

	start := time.Now()
	e.mustRun("wait", "agent", "--until", "blocked", "--timeout", "20s")
	if elapsed := time.Since(start); elapsed < 400*time.Millisecond {
		t.Errorf("wait returned after %s, want it to have blocked until the report arrived", elapsed)
	}
}

// A hook inside a session needs no argument, since cm already exports CM_SESSION into its shell.
//
// This is the one place cm reads that variable, and it does not weaken the rule it resembles. That rule is
// about `attach`: zmx treats the variable as a request to *switch* the parent terminal's session, so
// attaching from inside one hijacks the window it ran from. Using it as the default target of a report
// moves nothing.
func TestReportDefaultsToTheSurroundingSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "inside", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("inside")
		return ok && s.State == "running"
	})

	// No session argument, exactly as a hook would invoke it.
	if r := e.runInSession("inside", "report", "--state", "blocked", "--detail", "from a hook"); r.code != 0 {
		t.Fatalf("report with no session exited %d: %s", r.code, r.stderr)
	}

	s, _ := e.session("inside")
	if s.ReportedState != "blocked" || s.ReportedDetail != "from a hook" {
		t.Errorf("session = %+v, want the report applied to the surrounding session", s)
	}

	// An explicit name still wins, so a program can report about a session other than its own.
	e.mustRun("run", "--session", "other", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the other session to be running", 15*time.Second, func() bool {
		s, ok := e.session("other")
		return ok && s.State == "running"
	})
	if r := e.runInSession("inside", "report", "other", "--state", "busy"); r.code != 0 {
		t.Fatalf("report with an explicit session exited %d: %s", r.code, r.stderr)
	}
	if s, _ := e.session("other"); s.ReportedState != "busy" {
		t.Errorf("other session = %+v, want the explicit name to win over CM_SESSION", s)
	}
	// And the surrounding one is untouched by that call.
	if s, _ := e.session("inside"); s.ReportedState != "blocked" {
		t.Errorf("inside session = %+v, want it unchanged", s)
	}

	// With neither, a clear error rather than a guess.
	r := e.run("report", "--state", "busy")
	if r.code == 0 {
		t.Error("report with no session and no CM_SESSION exited 0, want non-zero")
	}
	if !strings.Contains(r.stderr, "CM_SESSION") {
		t.Errorf("stderr = %q, want it to name the variable it looked for", r.stderr)
	}
}

// Reported state is not persisted, since it describes a running program.
//
// A stored value would come back after a restart claiming a program needs input when it finished long ago,
// and anything waiting on it would be released for no reason.
func TestReportedStateIsNotPersisted(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "transient", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("transient")
		return ok && s.State == "running"
	})
	e.mustRun("report", "transient", "--state", "blocked", "--detail", "waiting")

	e.restartServer()

	s, ok := e.session("transient")
	if !ok {
		t.Fatal("the session did not survive the restart")
	}
	if s.ReportedState != "" {
		t.Errorf("session = %+v after a restart, want no reported state: it describes a running "+
			"program, so restoring it would claim something is blocked that is not", s)
	}
}

// Reporting about a session that does not exist fails, rather than being silently dropped.
func TestReportOnMissingSessionFails(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	r := e.run("report", "nosuchsession", "--state", "busy")
	if r.code == 0 {
		t.Error("report on a missing session exited 0, want non-zero")
	}
	if !strings.Contains(r.stderr, "nosuchsession") {
		t.Errorf("stderr = %q, want it to name the session", r.stderr)
	}
}
