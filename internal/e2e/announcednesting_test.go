package e2e

import (
	"strings"
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

// Several announced levels at once, each press leaving exactly one.
//
// Confirmed by hand first, over a real ssh with several nested cm sessions, and then this test found what
// the hand test had not: at four levels the third press detached the *outer* window while a live client was
// still nested. The escape on the detach key counted every forwarded press, and a press that legitimately
// left one level of several did not look like a change to the outer client, because the aggregate stayed
// nested. Three working presses therefore reached the escape. The fix is the count in the Hosting event; the
// symptom is what this asserts.
//
// Four levels rather than two, because two never reaches the escape and would have passed throughout.
//
// `env -u CM_SESSION` at each level is what makes every inner client announce rather than telling the
// server, standing in for an ssh chain without needing hosts.
func TestDetachKeyLeavesOneAnnouncedLevelPerPress(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	outer := attachOnPty(t, e, "outer", "--", "/bin/sh")
	outer.waitReady()

	levels := []string{"l1", "l2", "l3"}
	for _, name := range levels {
		outer.typeLine("env -u CM_SESSION " + e.bin + " attach " + name + " -- /bin/sh")
		e.waitFor("the "+name+" client to attach", 20*time.Second, func() bool {
			s, ok := e.session(name)
			return ok && s.Clients == 1
		})
	}

	// Every level's announcement reaches the outermost session, since each one writes through its parent's
	// pty all the way out.
	e.waitFor("the outer session to hear all three announcements", 10*time.Second, func() bool {
		s, ok := e.session("outer")
		return ok && s.AnnouncedClients == 3
	})

	// A marker from the innermost shell, which also orders the announcements against the first press.
	outer.typeLine("echo INNER_READY")
	outer.waitForOutput("INNER_READY", 20*time.Second)

	// One press per level, innermost first.
	for i := len(levels) - 1; i >= 0; i-- {
		name := levels[i]
		outer.detachKey()
		e.waitFor("the "+name+" client to detach", 15*time.Second, func() bool {
			s, ok := e.session(name)
			return ok && s.Clients == 0
		})
	}

	// The outer client is still here, which is the assertion this test exists for: before the count was
	// published, the third press detached it with a live client still nested. Waited rather than sampled
	// because a client that painted a notice reconnects to repaint and reads zero for an instant; what must
	// not happen is it staying zero.
	e.waitFor("the outer client to still be attached", 10*time.Second, func() bool {
		s, ok := e.session("outer")
		return ok && s.Clients == 1
	})

	// And it still owns the terminal, which a count cannot say.
	outer.typeLine("echo OUTER_ALIVE")
	outer.waitForOutput("OUTER_ALIVE", 20*time.Second)
	for _, name := range levels {
		if s, _ := e.session(name); s.State != "running" {
			t.Errorf("%s state = %q, want running: detaching must not end a session", name, s.State)
		}
	}

	// And the next press leaves the outermost, by then holding no handover at all.
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
		t.Fatal("the outer client never detached once every announced client had gone")
	}
	outer.waitExit(10 * time.Second)
}

// The outer session's location names the session a nested client went to.
//
// End to end because the label crosses every hop: the inner client puts its own session in the sequence, the
// bytes travel up the pty, the outer session's pump parses them, the server places the client at the frame it
// was announced inside, and the CLI renders it. Each hop has a unit test with a constructed value, which is
// how a chain broken in the middle passes all of them.
//
// `env -u CM_SESSION` is the ssh hop, as in the tests above: it is what makes the client announce rather than
// name a parent in Open. An `@id` reference deliberately, because that is what `cm tui` attaches with, and the
// point of this is that the location shows the name anyway.
func TestLocationNamesTheSessionAnAnnouncedClientAttachedTo(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	outer := attachOnPty(t, e, "outer", "--", "/bin/sh")
	outer.waitReady()

	// A session to attach to, and its ID, so the inner client asks by ID and the name has to come from the
	// server's answer rather than from the command line.
	e.mustRun("attach", "--no-attach", "inner", "--", "/bin/sh")
	id := strings.TrimSpace(e.mustRun("info", "inner", "--field", "id"))
	if id == "" {
		t.Fatal("cm info --field id printed nothing")
	}

	outer.typeLine("env -u CM_SESSION " + e.bin + " attach " + id)
	e.waitFor("the nested client to attach", 20*time.Second, func() bool {
		s, ok := e.session("inner")
		return ok && s.Clients == 1
	})

	e.waitFor("the outer location to name the session the client went to", 20*time.Second, func() bool {
		s, ok := e.session("outer")
		if !ok {
			return false
		}
		for _, f := range s.Location {
			if f.Session == "inner" {
				return true
			}
		}
		return false
	})

	// And the rendered line a person reads, which is the reason any of this exists. The shell that ran the
	// attach has no cm integration loaded here, so there is no frame for the client to sit inside and the
	// location is the client alone.
	if got, want := e.mustRun("info", "outer", "--field", "location"), "inner\n"; got != want {
		t.Errorf("cm info --field location = %q, want %q", got, want)
	}
}
