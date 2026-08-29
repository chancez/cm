package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestSendReportsAFailedWriteToThePty exercises a path that has no natural cause.
//
// This is what fault injection is for here. A write to the pty cannot be made to fail from outside:
// os.File.Write loops over write(2) until the tty accepts everything, so there is no input, no size, and
// no timing that produces a partial or failed write. The behavior when one happens is therefore untested
// by construction, and it is not hypothetical behavior, since a wedged shell or a closed pty produces it.
//
// What must happen is that the caller hears about it. `cm send` that silently reports success while the
// keystrokes went nowhere is the worst version of this: a script goes on to wait for output that will
// never come, and nothing anywhere says why.
func TestSendReportsAFailedWriteToThePty(t *testing.T) {
	skipIfShort(t)

	// The instrumented build, since a released one has no fault machinery at all. The variable reaches the
	// server, which is the process that writes to the shim.
	e := newEnvWith(t, cmHooksBinary(t), "",
		"CM_TESTHOOK_FAULTS=before-shim-write:error")

	e.mustRun("attach", "--no-attach", "faulty", "--", "/bin/sh")

	r := e.run("send", "faulty", "echo hello")
	t.Logf("exit=%d stderr=%q", r.code, r.stderr)
	if r.code == 0 {
		t.Errorf("cm send exited 0 while the write to the pty failed, so a caller is told keystrokes "+
			"were delivered when they were not.\nstdout: %s\nstderr: %s", r.stdout, r.stderr)
	}
	// Failed for the injected reason rather than some other one, which is what stops this passing because
	// the session was missing or the server was down.
	if !strings.Contains(r.stderr, "injected fault") {
		t.Errorf("cm send stderr = %q, want it to name the injected fault: without that this test would "+
			"pass for any failure at all", r.stderr)
	}

	// The control, on the same binary with no spec. Without it a `cm send` broken for an unrelated reason
	// would satisfy everything above, and the test would report the fault mechanism working when nothing
	// had been injected.
	clean := newEnvWith(t, cmHooksBinary(t), "")
	clean.mustRun("attach", "--no-attach", "clean", "--", "/bin/sh")
	clean.mustRun("send", "clean", "printf CONTROL\\n", "--enter")
	clean.waitForOutputInSession("clean", "CONTROL", 20*time.Second)
}

// TestReleasedBinaryIgnoresFaultInjection is the guard on the whole mechanism.
//
// The reason faults are behind a build tag rather than only an environment variable is that a released cm
// honoring one would let a stale `export` in a shell profile make a real session fail, or stall, in a way
// whose cause is nowhere near its symptom. That is a worse bug than any this package helps find.
//
// Asserted behaviorally, matching TestReleasedBinaryIgnoresTheVersionOverride: the same spec that makes an
// instrumented cm fail above must do nothing at all here.
func TestReleasedBinaryIgnoresFaultInjection(t *testing.T) {
	skipIfShort(t)

	// The ordinary binary, with a spec that would break it if it were honored.
	e := newEnvWith(t, cmBinary(t), "",
		"CM_TESTHOOK_FAULTS=before-shim-write:error")

	e.mustRun("attach", "--no-attach", "unfaulty", "--", "/bin/sh")

	// Succeeds, because the fault does not exist in this build.
	e.mustRun("send", "unfaulty", "printf FAULTFREE\\n", "--enter")
	e.waitForOutputInSession("unfaulty", "FAULTFREE", 20*time.Second)
}
