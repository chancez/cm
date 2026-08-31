package e2e

import (
	"testing"
	"time"
)

// The detach key leaves the innermost session, and only after that the outer one.
//
// End to end because every hop is involved and no single one of them is the bug: the inner client has to
// declare which session it is inside, the server has to tell the outer session's client, that client has
// to stop intercepting the key, the byte has to survive the trip through the pty, and the inner client's
// own gate has to recognize it. A unit test of any one hop passes while the chain is broken.
//
// The symptom without it: the outer session is usually the per-window one, so ctrl-\ inside a nested
// attach detached that instead and the terminal window closed.
func TestDetachKeyLeavesTheInnermostSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	outer := attachOnPty(t, e, "outer", "--", "/bin/sh")
	outer.waitReady()

	// A nested attach, started by the shell inside the outer session. Nothing is arranged for it: the
	// shim exports CM_SESSION, which is what makes the inner client name the session it is inside.
	outer.typeLine(e.bin + " attach inner -- /bin/sh")
	e.waitFor("the nested client to attach", 20*time.Second, func() bool {
		s, ok := e.session("inner")
		return ok && s.Clients == 1
	})
	e.waitFor("the outer session to report that it is hosting", 10*time.Second, func() bool {
		s, ok := e.session("outer")
		return ok && len(s.Hosting) == 1
	})

	// A marker from the inner shell, which is also how the test knows the inner client has painted and
	// is reading input rather than still starting up.
	outer.typeLine("echo INNER_MARKER")
	outer.waitForOutput("INNER_MARKER", 20*time.Second)

	// One press. The inner session is the one that must go.
	outer.detachKey()
	e.waitFor("the inner client to detach", 15*time.Second, func() bool {
		s, ok := e.session("inner")
		return ok && s.Clients == 0
	})

	// The outer client is still attached and still holds the terminal, which is the half that was broken.
	if s, ok := e.session("outer"); !ok || s.Clients != 1 {
		t.Fatalf("outer session = %+v, want it still holding its one client: the press detached the "+
			"wrong session", s)
	}
	if s, _ := e.session("inner"); s.State != "running" {
		t.Errorf("inner state = %q, want running: detaching must not end the session", s.State)
	}

	// The outer shell is back in charge, which proves the terminal is usable rather than merely that a
	// count changed.
	outer.typeLine("echo OUTER_AGAIN")
	outer.waitForOutput("OUTER_AGAIN", 20*time.Second)

	// And a second press leaves the outer session, since the key came back when the nesting ended.
	//
	// Pressed in a loop rather than once, because the handover back is asynchronous: the key can be
	// pressed in the window before this client has processed the notification, and a stray 0x1c reaching
	// /bin/sh does nothing. A client that never took the key back never detaches, so this still fails.
	pressed := false
	deadline := time.Now().Add(scaleTimeout(15 * time.Second))
	for time.Now().Before(deadline) {
		outer.detachKey()
		if s, ok := e.session("outer"); ok && s.Clients == 0 {
			pressed = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !pressed {
		t.Fatal("the outer client never detached, so it did not take the detach key back when the " +
			"nested client left")
	}
	outer.waitExit(10 * time.Second)
}
