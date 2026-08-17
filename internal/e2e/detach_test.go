package e2e

import (
	"strings"
	"testing"
	"time"
)

// `cm detach` must disconnect a real client and leave the shell running.
//
// End to end because the failure would be in the wiring rather than in any one layer: the server has to
// wake a client that is blocked waiting for output, the client has to recognize the message as a request
// rather than as the server going away, and the process has to actually exit. A unit test of any single
// hop passes while the chain is broken.
func TestDetachCommandDisconnectsAClient(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "letgo", "--", "/bin/sh")
	c.waitReady()
	// A marker, so the test knows the shell is really running rather than only that a client connected.
	c.typeLine("echo DETACH_MARKER")
	c.waitForOutput("DETACH_MARKER", 15*time.Second)

	e.waitFor("the client to attach", 10*time.Second, func() bool {
		s, ok := e.session("letgo")
		return ok && s.Clients == 1
	})

	got := e.run("detach", "letgo")
	if got.code != 0 {
		t.Fatalf("cm detach exited %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "letgo") {
		t.Errorf("stdout = %q, want it to name the session", got.stdout)
	}

	e.waitFor("the client to be disconnected", 10*time.Second, func() bool {
		s, ok := e.session("letgo")
		return ok && s.Clients == 0
	})

	// The client process really exited, rather than merely being unregistered. A client left running
	// against a closed stream is the reconnect bug: it would come back and re-register.
	c.waitExit(10 * time.Second)

	// And the session survived, which is the difference between this and `cm kill`.
	s, ok := e.session("letgo")
	if !ok {
		t.Fatal("session is gone after a detach, want it still running")
	}
	if s.State != "running" {
		t.Errorf("state = %q, want running: a detach must not end the session", s.State)
	}

	// Reattaching works, which proves the shell is genuinely still there rather than just recorded.
	c2 := attachOnPty(t, e, "letgo")
	c2.waitForOutput("DETACH_MARKER", 15*time.Second)
}

// A bare `cm detach` inside a session detaches that session.
//
// The shape a keybinding or a shell alias uses, and it takes the CM_SESSION path rather than an
// argument, so it is worth exercising as the variable is really set rather than as a name passed in.
func TestDetachCommandDefaultsToTheCallingSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "callers", "--", "/bin/sh")
	c.waitReady()
	e.waitFor("the client to attach", 10*time.Second, func() bool {
		s, ok := e.session("callers")
		return ok && s.Clients == 1
	})

	if got := e.runInSession("callers", "detach"); got.code != 0 {
		t.Fatalf("cm detach with CM_SESSION set exited %d: %s", got.code, got.stderr)
	}

	e.waitFor("the client to be disconnected", 10*time.Second, func() bool {
		s, ok := e.session("callers")
		return ok && s.Clients == 0
	})
	c.waitExit(10 * time.Second)
}

// Detaching a session nobody is attached to succeeds and says so.
//
// What makes the command safe to call without checking first. A teardown script that had to know
// whether anyone was watching would have to race that answer.
func TestDetachCommandOnIdleSessionSucceeds(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	if got := e.run("attach", "--no-attach", "idle", "--", "/bin/sh"); got.code != 0 {
		t.Fatalf("creating the session exited %d: %s", got.code, got.stderr)
	}

	got := e.run("detach", "idle")
	if got.code != 0 {
		t.Fatalf("cm detach on an idle session exited %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "no clients attached") {
		t.Errorf("stdout = %q, want it to say nothing was attached", got.stdout)
	}

	// Still running, since detaching nothing must not disturb the session.
	if s, ok := e.session("idle"); !ok || s.State != "running" {
		t.Errorf("session = %+v, want it still running", s)
	}
}

// Detaching a name that does not exist is an error, unlike detaching an idle session.
//
// The distinction is the point: an idle session is a satisfied request, while a name nobody has ever
// created is a mistake, and exiting 0 there would let a script report success having done nothing.
func TestDetachCommandUnknownSessionFails(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// A server has to exist, or the failure is "no server" rather than "no such session".
	if got := e.run("attach", "--no-attach", "real", "--", "/bin/sh"); got.code != 0 {
		t.Fatalf("creating a session exited %d: %s", got.code, got.stderr)
	}

	got := e.run("detach", "nosuch")
	if got.code == 0 {
		t.Errorf("cm detach on an unknown session exited 0, want a failure\nstdout: %s", got.stdout)
	}
}
