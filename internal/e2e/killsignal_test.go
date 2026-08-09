package e2e

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startStubbornJob runs a job that ignores SIGHUP and returns its pid.
//
// The trap runs in a child shell rather than the session's own, because trapping HUP in the session's
// shell protects that shell and not the sleep it forks. Without the child, SIGHUP reaches the sleep
// anyway and the test cannot show that escalating matters.
//
// The sleep duration is unique per session and is what finds the process, so a leftover from another
// case cannot be mistaken for this one's. Not hypothetical: a stale sleep from an earlier case made a
// working escalation look like a leak while checking this by hand.
func (e *env) startStubbornJob(session string, seconds int) int {
	e.t.Helper()
	marker := fmt.Sprintf("sleep %d", seconds)
	e.mustRun("attach", "--no-attach", session, "--", "/bin/sh")
	e.mustRun("send", session, fmt.Sprintf("sh -c \"trap '' HUP; %s\"", marker), "--enter")
	e.waitForOutputInSession(session, marker, 10*time.Second)

	// Read from the process table rather than from cm. The question these tests ask is whether a
	// process outlived its session, so cm's own view of it is exactly what cannot be trusted.
	var pid int
	e.waitFor("the job to appear in the process table", 10*time.Second, func() bool {
		pid = findProcess(marker)
		return pid > 0
	})
	e.t.Cleanup(func() {
		// Every process matching the marker, not just the pid found above.
		//
		// The job is `sh -c "trap ...; sleep N"`, so it is two processes and pgrep returns whichever it
		// lists first. Killing one leaves the other, and TestKillWithoutASignalUsesSIGHUP leaks its job
		// deliberately -- so a surviving wrapper was still matching `sleep 941` during later tests and
		// made them fail against a working implementation.
		//
		// A leaked job also holds a pty for the rest of the run, and macOS caps those at 511 system-wide
		// with exhaustion surfacing as "device not configured" somewhere unrelated.
		for _, p := range findProcesses(marker) {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
	})
	return pid
}

// findProcess returns one pid whose command line contains marker, or 0.
func findProcess(marker string) int {
	pids := findProcesses(marker)
	if len(pids) == 0 {
		return 0
	}
	return pids[0]
}

// findProcesses returns every pid whose command line contains marker.
//
// Several, because the job under test is a shell wrapping a sleep, so one marker names two processes.
// Cleanup has to reach both: leaving the wrapper behind means a later test's pgrep still matches this
// marker, which is how a working implementation came to look broken.
func findProcesses(marker string) []int {
	out, err := exec.Command("pgrep", "-f", marker).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(field); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// jobAlive reports whether a pid still exists.
//
// Wraps the harness's processAlive, which returns an error, so the tests below read as questions rather
// than as error checks.
func jobAlive(pid int) bool {
	return pid > 0 && processAlive(pid) == nil
}

// waitForProcessToGo blocks until a pid is gone, reporting whether it went.
func waitForProcessToGo(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !jobAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// sessionGone reports whether cm has forgotten a session.
//
// Deliberately not used to decide whether a kill worked. The record is deleted whether or not the signal
// reached the job: measured with the shim ignoring the requested signal, where `cm kill --signal kill`
// removed the record and left the job running. A test asserting on the record therefore passes against a
// broken implementation, which is why the ones below assert on the process.
func (e *env) sessionGone(name string) bool {
	e.t.Helper()
	_, ok := e.session(name)
	return !ok
}

// Plain `cm kill` sends SIGHUP, which a job can ignore.
//
// Pinned as current behavior rather than as desirable. SIGHUP is a request, so a job that ignores it
// survives while `cm kill` reports the session killed, and the surviving process holds a pty. This test
// exists so the behavior is deliberate, and so the escalation below has something to be measured
// against: without it, a passing --signal test would not show that the default was insufficient.
func TestKillWithoutASignalUsesSIGHUP(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	pid := e.startStubbornJob("stubborn", 941)

	e.mustRun("kill", "stubborn")

	// The record goes either way, since the shim is asked to shut down and does.
	e.waitFor("the session record to go", 10*time.Second, func() bool {
		return e.sessionGone("stubborn")
	})

	// What did not happen is the point: nothing stronger than SIGHUP followed, so the job is still here.
	if !jobAlive(pid) {
		t.Error("the HUP-ignoring job ended, so this test cannot show that --signal is needed")
	}
}

// --signal names a stronger signal, which reaps what SIGHUP cannot.
//
// Paired with the test above: same job, same command, one flag different, and only this one stops it.
func TestKillWithSignalReapsAStubbornJob(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	for i, tc := range []struct {
		name string
		args []string
	}{
		{name: "signal-kill", args: []string{"--signal", "kill"}},
		{name: "signal-number", args: []string{"--signal", "9"}},
		// --force means be maximally forceful, of which SIGKILL is half. That half was undocumented
		// until now, which is why it is pinned here.
		{name: "force", args: []string{"--force"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := "s-" + tc.name
			pid := e.startStubbornJob(session, 950+i)

			e.mustRun(append([]string{"kill", session}, tc.args...)...)

			// The process, not the record. The record disappears either way.
			if !waitForProcessToGo(pid, 15*time.Second) {
				t.Errorf("the job survived kill %v, want it reaped", tc.args)
			}
		})
	}
}

// A signal the job does not ignore works too, so --signal is not only a synonym for SIGKILL.
//
// Worth its own case: a caller wants the gentlest signal that works, and a program needing SIGTERM to
// flush state should not have to be SIGKILLed.
func TestKillWithSignalTerm(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	// Ignores HUP only, so TERM is what reaches it.
	pid := e.startStubbornJob("term-me", 961)

	e.mustRun("kill", "term-me", "--signal", "term")

	if !waitForProcessToGo(pid, 15*time.Second) {
		t.Error("the job survived kill --signal term, want it reaped")
	}
}

// A signal spelling is accepted the same ways `cm signal` accepts it.
func TestKillSignalSpellings(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	for i, spec := range []string{"term", "TERM", "SIGTERM", "15"} {
		session := fmt.Sprintf("spell%d", i)
		e.mustRun("run", "--session", session, "-d", "--", "/bin/sh", "-c", "sleep 300")
		if r := e.run("kill", session, "--signal", spec); r.code != 0 {
			t.Errorf("kill --signal %q exited %d, want it accepted: %s", spec, r.code, r.stderr)
		}
	}
}

// A bad signal is refused before anything is killed.
//
// Client-side, so a typo costs nothing: the session has to survive a rejected call, or a mistyped signal
// would be more destructive than a correct one.
func TestKillRejectsABadSignal(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("run", "--session", "keeper", "-d", "--", "/bin/sh", "-c", "sleep 300")

	for _, spec := range []string{"nosuchsignal", "0", "-1"} {
		r := e.run("kill", "keeper", "--signal", spec)
		if r.code == 0 {
			t.Errorf("kill --signal %q exited 0, want it refused", spec)
		}
	}

	// Untouched by any refused call.
	s, ok := e.session("keeper")
	if !ok || s.State != "running" {
		t.Errorf("session state = %q after refused kills, want it still running", s.State)
	}
	// And the error names what is accepted, since the fix is a spelling.
	if r := e.run("kill", "keeper", "--signal", "nosuchsignal"); !strings.Contains(r.stderr, "term") {
		t.Errorf("stderr = %q, want it to list the accepted names", r.stderr)
	}
}

// --signal composes with the ways kill selects sessions.
func TestKillSignalWithSelectors(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	for _, name := range []string{"g1", "g2"} {
		e.mustRun("run", "--session", name, "--tag", "grp=k", "-d",
			"--", "/bin/sh", "-c", "sleep 300")
	}
	e.mustRun("run", "--session", "untagged", "-d", "--", "/bin/sh", "-c", "sleep 300")

	e.mustRun("kill", "--tag", "grp=k", "--signal", "kill")

	e.waitFor("the tagged sessions to go", 15*time.Second, func() bool {
		return e.sessionGone("g1") && e.sessionGone("g2")
	})
	// The selector still bounds what is killed, which a signal must not widen.
	if s, ok := e.session("untagged"); !ok || s.State != "running" {
		t.Errorf("untagged session state = %q, want it still running", s.State)
	}
}
