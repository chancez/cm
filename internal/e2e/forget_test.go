package e2e

import (
	"strings"
	"testing"
	"time"
)

// `cm forget` clears the record of a session that has ended, and refuses one that has not.
//
// End to end rather than at the seam because the split between the two verbs is the point: the refusal has
// to reach the caller as a non-zero exit, and a live session has to come through it untouched. A `cm run -d`
// that finished is exactly the case, since its record is kept on purpose so its status can be collected.
func TestForgetClearsAnEndedSessionAndRefusesALiveOne(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "done", "-d", "--", "/bin/sh", "-c", "exit 3")
	e.mustRun("run", "--session", "busy", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the finished session to be recorded as exited", 15*time.Second, func() bool {
		s, ok := e.session("done")
		return ok && s.State == "exited"
	})

	out := e.mustRun("forget", "done")
	if !strings.Contains(out, "forgot done") {
		t.Errorf("stdout = %q, want it to report the session forgotten", out)
	}
	if _, ok := e.session("done"); ok {
		t.Error("the forgotten session is still listed")
	}

	r := e.run("forget", "busy")
	if r.code == 0 {
		t.Errorf("forget of a running session exited 0, want non-zero: %s", r.stdout)
	}
	if !strings.Contains(r.stderr, "still running") {
		t.Errorf("stderr = %q, want it to say the session is still running", r.stderr)
	}
	// Refused rather than half-done: the session is still there and still running.
	s, ok := e.session("busy")
	if !ok || s.State != "running" {
		t.Errorf("session after the refusal = %+v, ok = %v, want it running and untouched", s, ok)
	}
}

// --ended clears every ended record and leaves running sessions alone, which is the bulk case: a list
// filling with rows for windows that were closed is what this exists for.
func TestForgetEndedLeavesRunningSessions(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	for _, name := range []string{"d1", "d2"} {
		e.mustRun("run", "--session", name, "-d", "--", "/bin/sh", "-c", "exit 0")
	}
	e.mustRun("run", "--session", "alive", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("both finished sessions to be recorded as exited", 15*time.Second, func() bool {
		d1, ok1 := e.session("d1")
		d2, ok2 := e.session("d2")
		return ok1 && ok2 && d1.State == "exited" && d2.State == "exited"
	})

	e.mustRun("forget", "--ended")

	var names []string
	for _, s := range e.list() {
		names = append(names, s.Name)
	}
	if len(names) != 1 || names[0] != "alive" {
		t.Errorf("sessions after forget --ended = %v, want only [alive]", names)
	}
}
