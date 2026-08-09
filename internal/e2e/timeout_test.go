package e2e

import (
	"strings"
	"testing"
	"time"
)

// timeoutBudget bounds how long a test waits for a command that should have bounded itself.
//
// Generously larger than the timeouts passed below, so a slow machine does not fail the test, while still
// far short of hanging: the failure being checked for is "waits forever", and anything that takes ten
// times its own deadline has failed whatever the exact number.
const timeoutBudget = 20 * time.Second

// `cm read --follow --timeout` returns rather than streaming forever.
//
// The gap this closes. Following a session whose program never exits had no bound at all, so an agent that
// followed one waited indefinitely, and the cm skill had to tell callers to always pass a timeout because a
// missing bound was the default failure.
func TestReadFollowTimeoutReturns(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	// A shell with no OSC 133 and nothing ending it, which is the case that used to hang.
	e.mustRun("attach", "--no-attach", "f", "--", "/bin/sh")
	e.mustRun("send", "f", "echo before-follow", "--enter")
	e.waitForOutputInSession("f", "before-follow", 10*time.Second)

	start := time.Now()
	r := e.runWithin(timeoutBudget, "read", "f", "--follow", "--timeout", "2s")
	elapsed := time.Since(start)

	// Exit 0, which is the decision that makes this flag mean one thing across commands. The output was
	// delivered and stopping is what was asked for, so failing would make a caller discard what it got.
	if r.code != 0 {
		t.Errorf("read --follow --timeout exited %d, want 0: a bounded follow succeeded: %s",
			r.code, r.stderr)
	}
	// Bounded near its deadline rather than at the harness's.
	if elapsed > 10*time.Second {
		t.Errorf("read --follow --timeout 2s took %v, want it bounded by the timeout", elapsed)
	}
	// And the output it had already printed is still there, which is the point of not failing.
	if !strings.Contains(r.stdout, "before-follow") {
		t.Errorf("stdout = %q, want the output printed before the timeout", r.stdout)
	}
}

// The timeout and the wait disagree about what a deadline means, deliberately.
//
// `cm wait` was asked a question and could not answer it, so it fails. A follower was asked to print output
// until told to stop, and a deadline is being told to stop. Asserted together because the difference is a
// decision rather than an accident, and a later change that unified them would be wrong.
//
// Worth knowing where the follower's exit status actually comes from, since it is not the obvious place: the
// client treats a cancelled or expired context as a deliberate detach and returns no error at all, so the
// zero status is that behavior rather than a deadline being special-cased. Verified by disabling the
// deadline arm in followSession, which changed nothing. So this test guards the contract, and the arm exists
// to keep it true if that path ever changes.
func TestTimeoutMeaningDiffersBetweenWaitAndFollow(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "f", "--", "/bin/sh")

	// A wait that cannot be satisfied: nothing will end this session.
	if r := e.runWithin(timeoutBudget, "wait", "f", "--until", "exited", "--timeout", "1s"); r.code == 0 {
		t.Error("wait --timeout exited 0, want non-zero: it could not answer what it was asked")
	}

	// The same session, followed with the same kind of deadline, succeeds.
	r := e.runWithin(timeoutBudget, "read", "f", "--follow", "--timeout", "1s")
	if r.code != 0 {
		t.Errorf("read --follow --timeout exited %d, want 0", r.code)
	}
	// And says nothing about a timeout, since from the caller's point of view nothing went wrong. A
	// diagnostic here would send someone looking for a problem that does not exist.
	if strings.Contains(r.stderr, "timed out") {
		t.Errorf("stderr = %q, want no timeout complaint from a bounded follow", r.stderr)
	}
}

// --timeout without --follow is refused rather than silently ignored.
//
// Every other form of read returns as soon as the server answers, so a timeout there would bound something
// that cannot hang. A caller passing it believes it is protecting against a hang, and accepting it quietly
// would confirm a belief that is wrong.
func TestReadTimeoutRequiresFollow(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "f", "--", "/bin/sh")

	r := e.run("read", "f", "--timeout", "2s")
	if r.code == 0 {
		t.Error("read --timeout without --follow exited 0, want it refused")
	}
	if !strings.Contains(r.stderr, "--follow") {
		t.Errorf("stderr = %q, want it to name --follow", r.stderr)
	}
}

// A timeout of zero means no bound, on every command that takes one.
//
// The trap this guards against is a shared helper turning an unset flag into an already-expired deadline:
// context.WithTimeout(ctx, 0) does exactly that, so passing the flag straight through would make every
// command fail instantly rather than waiting.
func TestZeroTimeoutMeansNoBound(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("run", "--session", "quick", "-d", "--", "/bin/sh", "-c", "exit 0")

	// An explicit zero, on a command whose work finishes promptly. It must wait for the work rather than
	// give up at once.
	r := e.runWithin(timeoutBudget, "wait", "quick", "--until", "exited", "--timeout", "0")
	if r.code != 0 {
		t.Errorf("wait --timeout 0 exited %d, want it to wait rather than expire: %s", r.code, r.stderr)
	}
}

// `cm send --follow --timeout` is bounded too, which it already was.
//
// Pinned rather than added: the timeout travels to the server-side wait, which returns and ends the stream.
// Worth a test because the mechanism is different from read's -- one is a deadline on the client's stream,
// the other a bound the server enforces -- and a change to either could silently unbound this.
func TestSendFollowTimeoutReturns(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	// No OSC 133, so --follow's implied wait for idle can never be satisfied. This is the documented
	// pitfall the warning mentions, and the timeout is what makes it survivable.
	e.mustRun("attach", "--no-attach", "f", "--", "/bin/sh")

	start := time.Now()
	r := e.runWithin(timeoutBudget, "send", "f", "sleep 60", "--enter", "--follow", "--timeout", "2s")
	elapsed := time.Since(start)

	if elapsed > 10*time.Second {
		t.Errorf("send --follow --timeout 2s took %v, want it bounded", elapsed)
	}
	// The warning about the missing integration is the useful part here, since it names the cause.
	if !strings.Contains(r.stderr, "OSC 133") {
		t.Errorf("stderr = %q, want the warning about no shell integration", r.stderr)
	}
}

// `cm run --timeout` still fails on a deadline, since it was asked for a result.
func TestRunTimeoutFails(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	r := e.runWithin(timeoutBudget, "run", "--timeout", "2s", "--", "/bin/sh", "-c", "sleep 60")
	if r.code == 0 {
		t.Error("run --timeout exited 0, want non-zero: the command did not finish")
	}
	if !strings.Contains(r.stderr, "timed out") {
		t.Errorf("stderr = %q, want it to say it timed out", r.stderr)
	}
}

// Every command that accepts --timeout describes it identically.
//
// The drift this prevents is what the feature started from: three commands had the flag with three
// different help strings and the one that could block forever did not have it at all.
func TestTimeoutHelpIsConsistent(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	const want = "give up after this long (0 waits indefinitely)"
	for _, cmd := range []string{"wait", "send", "run", "read"} {
		out := e.mustRun(cmd, "--help")
		if !strings.Contains(out, "--timeout") {
			t.Errorf("%s --help does not offer --timeout", cmd)
			continue
		}
		if !strings.Contains(out, want) {
			t.Errorf("%s --help describes --timeout differently, want %q:\n%s", cmd, want, out)
		}
	}
}
