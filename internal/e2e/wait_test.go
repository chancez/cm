package e2e

import (
	"strings"
	"testing"
	"time"
)

// `cm wait --until idle` must block for as long as the command takes.
//
// Driven against a real zsh because the timing is the point: the shell takes a few hundred milliseconds
// between receiving input and reporting a command, and every version of this feature that got the wait
// wrong still passed a test that only checked the final state.
func TestWaitBlocksUntilTheCommandFinishes(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	requireShell(t, "/bin/zsh")
	// The shell reports OSC 133 because the test says so, not because the machine happens to be
	// configured for it.
	osc := e.withOSC133()

	e.mustRun(append([]string{"run", "--session", "waiter", "-d"}, append(osc, "--", "/bin/zsh", "-i")...)...)
	e.waitFor("the shell to reach a prompt", 15*time.Second, func() bool {
		s, ok := e.session("waiter")
		return ok && s.State == "running" && !s.Busy
	})

	e.mustRun("send", "waiter", "sleep 3", "--enter")
	e.waitFor("the command to start", 15*time.Second, func() bool {
		s, _ := e.session("waiter")
		return s.Busy
	})

	start := time.Now()
	e.mustRun("wait", "waiter", "--until", "idle", "--timeout", "30s")
	elapsed := time.Since(start)

	// At least a second of the sleep must remain when the wait starts, so a wait that returned early
	// would show up as a short elapsed time rather than as a wrong final state.
	if elapsed < time.Second {
		t.Errorf("wait returned after %s, want it to block for the rest of a 3s command", elapsed)
	}
	if s, _ := e.session("waiter"); s.Busy {
		t.Errorf("session is still busy after the wait returned: %+v", s)
	}
}

// A wait that gives up exits non-zero, so it composes with && and ||.
func TestWaitTimeoutExitsNonZero(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	requireShell(t, "/bin/zsh")
	// The shell reports OSC 133 because the test says so, not because the machine happens to be
	// configured for it.
	osc := e.withOSC133()

	e.mustRun(append([]string{"run", "--session", "busy", "-d"}, append(osc, "--", "/bin/zsh", "-i")...)...)
	e.waitFor("the shell to reach a prompt", 15*time.Second, func() bool {
		s, ok := e.session("busy")
		return ok && !s.Busy
	})
	e.mustRun("send", "busy", "sleep 30", "--enter")
	e.waitFor("the command to start", 15*time.Second, func() bool {
		s, _ := e.session("busy")
		return s.Busy
	})

	r := e.run("wait", "busy", "--until", "idle", "--timeout", "500ms")
	if r.code == 0 {
		t.Errorf("wait exited 0 on timeout, want non-zero so it composes with &&")
	}
	// The message says what it is doing instead, which is the useful part of a timeout.
	if !strings.Contains(r.stderr, "sleep 30") {
		t.Errorf("stderr = %q, want it to name the command that is still running", r.stderr)
	}
}

// send --wait must wait for the command it sent rather than the prompt it started at.
//
// The whole reason the wait is part of the send request. Two separate calls cannot be ordered from
// outside: a command that finishes first leaves a following wait blocked on a transition that already
// happened, and a shell that is already idle satisfies a naive wait before its own command starts.
func TestSendWaitWaitsForTheCommandItSent(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	requireShell(t, "/bin/zsh")
	// The shell reports OSC 133 because the test says so, not because the machine happens to be
	// configured for it.
	osc := e.withOSC133()

	e.mustRun(append([]string{"run", "--session", "atomic", "-d"}, append(osc, "--", "/bin/zsh", "-i")...)...)
	e.waitFor("the shell to reach a prompt", 15*time.Second, func() bool {
		s, ok := e.session("atomic")
		return ok && !s.Busy
	})

	start := time.Now()
	e.mustRun("send", "atomic", "sleep 3", "--enter", "--wait", "idle", "--timeout", "30s")
	elapsed := time.Since(start)

	// The session was idle when the send was issued, so an implementation that checks on arrival returns
	// in milliseconds. Anything under two seconds means it did not wait for the command.
	if elapsed < 2*time.Second {
		t.Errorf("send --wait idle returned after %s, want it to wait out a 3s command: "+
			"the session was already idle when the input was sent", elapsed)
	}
}

// A fast command must still resolve, which is the other half of the same race.
//
// Repeated, because a single pass can win by luck: the failure mode is a wait that misses the transition
// and then blocks until its timeout.
func TestSendWaitResolvesForFastCommands(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	requireShell(t, "/bin/zsh")
	// The shell reports OSC 133 because the test says so, not because the machine happens to be
	// configured for it.
	osc := e.withOSC133()

	e.mustRun(append([]string{"run", "--session", "fast", "-d"}, append(osc, "--", "/bin/zsh", "-i")...)...)
	e.waitFor("the shell to reach a prompt", 15*time.Second, func() bool {
		s, ok := e.session("fast")
		return ok && !s.Busy
	})

	const runs = 10
	for i := range runs {
		// A short timeout on purpose: if the wait misses the transition it will hit this rather than
		// hanging the suite, and the failure names the iteration.
		r := e.run("send", "fast", "true", "--enter", "--wait", "idle", "--timeout", "10s")
		if r.code != 0 {
			t.Fatalf("iteration %d: send --wait idle exited %d on an instant command: %s",
				i, r.code, r.stderr)
		}
	}
}

// `cm read` returns a bounded tail with soft-wrapped lines rejoined.
//
// Both halves matter to a caller parsing output: the bound keeps a megabyte log usable, and rejoining
// means a path the terminal broke to fit its width is one line rather than two.
func TestReadReturnsABoundedUnwrappedTail(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	requireShell(t, "/bin/zsh")
	// The shell reports OSC 133 because the test says so, not because the machine happens to be
	// configured for it.
	osc := e.withOSC133()

	e.mustRun(append([]string{"run", "--session", "reader", "-d"}, append(osc, "--", "/bin/zsh", "-i")...)...)
	e.waitFor("the shell to reach a prompt", 15*time.Second, func() bool {
		s, ok := e.session("reader")
		return ok && !s.Busy
	})

	// Numbered lines, so what came back is identifiable rather than merely the right length.
	e.mustRun("send", "reader", "for i in $(seq 1 40); do echo line-$i; done",
		"--enter", "--wait", "idle", "--timeout", "30s")

	got := e.mustRun("read", "reader", "--lines", "5")
	if !strings.Contains(got, "line-40") {
		t.Errorf("read --lines 5 = %q, want the end of the output", got)
	}
	if strings.Contains(got, "line-1\n") {
		t.Errorf("read --lines 5 = %q, want only the tail, not the whole output", got)
	}

	// A line longer than the terminal is wide, which the terminal soft-wraps.
	e.mustRun("send", "reader", `python3 -c "print('X'*300)"`,
		"--enter", "--wait", "idle", "--timeout", "30s")

	unwrapped := e.mustRun("read", "reader", "--lines", "5")
	if !strings.Contains(unwrapped, strings.Repeat("X", 300)) {
		t.Errorf("read did not rejoin a soft-wrapped line; got %q", unwrapped)
	}

	// And --keep-wrap shows it as the terminal laid it out, which is the point of having the flag.
	wrapped := e.mustRun("read", "reader", "--lines", "8", "--keep-wrap")
	if strings.Contains(wrapped, strings.Repeat("X", 300)) {
		t.Errorf("read --keep-wrap rejoined the line anyway; got %q", wrapped)
	}
}
