package e2e

import (
	"syscall"
	"testing"
	"time"
)

// The detach key leaves an inner session that could not tell the server it was nested.
//
// `env -u CM_SESSION` stands in for the ssh hop, and is the whole of what makes this case different: a
// client on the far side of an ssh has no CM_SESSION to name a parent with, so Open carries no
// inside_session and the outer session's server learns nothing from the RPC path. What it learns instead
// arrives in its own output stream, which is where the inner client announces itself.
//
// End to end because every hop is involved and no single one is the bug: the inner client has to emit the
// sequence through its screen, the bytes have to survive the trip up the pty, the outer session's pump has
// to parse them out of ordinary output, the server has to publish it, and the outer client's gate has to
// stop intercepting the key. A unit test of any one hop passes while the chain is broken.
//
// The symptom without it: the outer session is usually the per-window one, so ctrl-\ inside the remote
// session closed the terminal window instead of leaving the remote session.
func TestDetachKeyLeavesAnAnnouncedInnerSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	outer := attachOnPty(t, e, "outer", "--", "/bin/sh")
	outer.waitReady()

	outer.typeLine("env -u CM_SESSION " + e.bin + " attach inner -- /bin/sh")
	e.waitFor("the nested client to attach", 20*time.Second, func() bool {
		s, ok := e.session("inner")
		return ok && s.Clients == 1
	})

	// The outer session reports nothing hosted, which is the state this test is about: an announced client
	// is not a session on this server, so it is deliberately absent from the listing while still being
	// enough to move the detach key.
	if s, ok := e.session("outer"); !ok || len(s.Hosting) != 0 {
		t.Fatalf("outer session = %+v, want no hosted sessions: nothing named a parent in Open", s)
	}

	// A marker from the inner shell, which also orders the announcement against this press. The
	// announcement is written before the inner client's first painted byte, and the outer session's pump
	// consumes its pty in order, so a marker on screen means the announcement has already been applied.
	outer.typeLine("echo INNER_MARKER")
	outer.waitForOutput("INNER_MARKER", 20*time.Second)

	// One press. The inner session is the one that must go.
	outer.detachKey()
	e.waitFor("the inner client to detach", 15*time.Second, func() bool {
		s, ok := e.session("inner")
		return ok && s.Clients == 0
	})

	// The outer client still holds the terminal, which is the half that was broken.
	if s, ok := e.session("outer"); !ok || s.Clients != 1 {
		t.Fatalf("outer session = %+v, want it still holding its one client: the press detached the "+
			"wrong session", s)
	}
	if s, _ := e.session("inner"); s.State != "running" {
		t.Errorf("inner state = %q, want running: detaching must not end the session", s.State)
	}

	// The outer shell is back in charge, which proves the terminal is usable rather than that a count
	// changed, and that the key came back when the announcement was withdrawn.
	outer.typeLine("echo OUTER_AGAIN")
	outer.waitForOutput("OUTER_AGAIN", 20*time.Second)

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
			"announced client withdrew")
	}
	outer.waitExit(10 * time.Second)
}

// A nesting whose client is gone can still be escaped, by pressing the detach key until cm says otherwise.
//
// The state: the inner client was killed, so it withdrew nothing and the parent still believes a client is
// there. Every press is forwarded into nothing, and before this the window could only be freed from another
// one. Two presses now go through as before, the second is reported, and the third leaves this session.
//
// End to end because the escape spans the gate, the notice, and the detach path, and because the stranded
// state itself only exists once three real processes are involved: a unit test can set the flag, but not
// arrive at it by the route a dropped link takes.
func TestDetachKeyEscapesANestingThatIsNotAnswering(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	outer := attachOnPty(t, e, "outer", "--", "/bin/sh")
	outer.waitReady()

	outer.typeLine("env -u CM_SESSION " + e.bin + " attach inner -- /bin/sh")
	e.waitFor("the nested client to attach", 20*time.Second, func() bool {
		s, ok := e.session("inner")
		return ok && s.Clients == 1
	})
	e.waitFor("the outer session to hear the announcement", 10*time.Second, func() bool {
		s, ok := e.session("outer")
		return ok && s.AnnouncedClients == 1
	})

	// Killed rather than detached, which is the whole setup: a client that exits withdraws its
	// announcement, and a dropped ssh is the case where nothing does.
	inner, ok := e.session("inner")
	if !ok || len(inner.AttachedClients) != 1 {
		t.Fatalf("inner session = %+v, want exactly one attached client to kill", inner)
	}
	if err := syscall.Kill(inner.AttachedClients[0].PID, syscall.SIGKILL); err != nil {
		t.Fatalf("killing the nested client: %v", err)
	}
	e.waitFor("the inner session to lose its client", 15*time.Second, func() bool {
		s, ok := e.session("inner")
		return ok && s.Clients == 0
	})

	// The parent still believes it is hosting, which is the state being escaped rather than a bug in this
	// test: nothing withdrew the announcement and nothing else can.
	if s, _ := e.session("outer"); s.AnnouncedClients != 1 {
		t.Fatalf("outer announced clients = %d, want 1: the setup did not strand an announcement",
			s.AnnouncedClients)
	}

	// The first two presses are forwarded, so this window stays put.
	outer.detachKey()
	time.Sleep(scaleTimeout(300 * time.Millisecond))
	outer.detachKey()
	time.Sleep(scaleTimeout(300 * time.Millisecond))
	if s, ok := e.session("outer"); !ok || s.Clients != 1 {
		t.Fatalf("outer session = %+v after two presses, want it still attached: the escape must take "+
			"three, so auto-repeat cannot reach it", s)
	}

	// The third leaves, whatever the handover says.
	outer.detachKey()
	e.waitFor("the outer client to detach on the third press", 15*time.Second, func() bool {
		s, ok := e.session("outer")
		return ok && s.Clients == 0
	})
	if s, _ := e.session("outer"); s.State != "running" {
		t.Errorf("outer state = %q, want running: escaping is a detach, not a kill", s.State)
	}
	outer.waitExit(10 * time.Second)
}
