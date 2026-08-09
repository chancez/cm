package e2e

import (
	"strings"
	"testing"
	"time"
)

// An attached client must survive the server restarting, and reconnect on its own.
//
// This is cm's central architectural claim and the reason for the three-layer design: the shim owns the pty,
// so a server can be replaced under a live client and the shell never notices. Everything else about
// upgrades follows from it.
//
// It needs a real client on a real terminal. The reconnect loop lives in the client, holds the terminal in
// raw mode across the outage, buffers keystrokes typed while the server is away, and resumes the output
// stream from its own position. None of that exists below the process boundary, and a test driving the
// service directly would assert none of it.
func TestClientReconnectsAcrossAServerRestart(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "upgrade", "--", "/bin/sh")
	c.waitReady()

	c.typeLine("echo BEFORE_RESTART")
	c.waitForOutput("BEFORE_RESTART", 15*time.Second)

	before, ok := e.session("upgrade")
	if !ok {
		t.Fatal("session was not created")
	}
	if before.Clients != 1 {
		t.Fatalf("clients = %d before the restart, want 1", before.Clients)
	}

	// Replace the server under the live client. `server stop` leaves sessions running by design, and the
	// next command starts a fresh one that adopts them.
	e.restartServer()

	// The client reconnects on its own, without anything being typed. Polled on the server's view of
	// clients rather than on the terminal, since a reconnect that produced no output would still be a
	// reconnect.
	e.waitFor("the client to reconnect on its own", 20*time.Second, func() bool {
		s, ok := e.session("upgrade")
		return ok && s.Clients == 1
	})

	after, _ := e.session("upgrade")
	// Same shell throughout: the pty was never recreated, which is what makes this an upgrade rather than
	// a restart.
	if after.ShellPID != before.ShellPID {
		t.Errorf("shell pid changed across the restart: %d -> %d, want the same process",
			before.ShellPID, after.ShellPID)
	}
	if after.State != "running" {
		t.Errorf("state after the restart = %q, want running", after.State)
	}

	// And the session is still usable, which is the part a user would notice. Typed after the reconnect,
	// so this tests the restored connection rather than buffered input.
	c.typeLine("echo AFTER_RESTART")
	c.waitForOutput("AFTER_RESTART", 20*time.Second)
}

// Keystrokes typed while the server is away must reach the shell, not vanish.
//
// The client holds them in a pending buffer and flushes them on reconnect. Without that a user typing
// through a brief freeze would silently lose what they typed, which is worse than the freeze: the input was
// accepted by the terminal and then discarded.
//
// Deliberately typed during the outage rather than before or after it. That window is the only thing this
// test is about, and hitting it means stopping the server and typing before the client has reconnected.
func TestInputTypedDuringAnOutageIsNotLost(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "buffered", "--", "/bin/sh")
	c.waitReady()
	c.typeLine("echo READY_MARK")
	c.waitForOutput("READY_MARK", 15*time.Second)

	// Stop the server and leave it stopped, so the client is definitely mid-outage.
	e.mustRun("server", "stop")
	e.waitServerGone()

	// Typed with no server to receive it. The client is in its reconnect loop and must hold this.
	c.typeLine("echo TYPED_WHILE_DOWN")

	// A moment with the server still down, so the input cannot have been delivered yet and the test is not
	// quietly racing a fast reconnect.
	time.Sleep(500 * time.Millisecond)

	// Bring a server back. Any command starts one, which is the same path a user's next command takes.
	e.mustRun("list")

	// The buffered line reaches the shell and runs.
	got := c.waitForOutput("TYPED_WHILE_DOWN", 25*time.Second)
	// Once, not twice: a flush that also replayed on a second reconnect would run the command again, which
	// for anything with side effects would be worse than losing it.
	if n := strings.Count(got, "TYPED_WHILE_DOWN"); n < 1 {
		t.Errorf("the line typed during the outage never arrived: %q", got)
	}
}

// Output produced while no client is attached must arrive after the reconnect.
//
// The shim keeps consuming the pty and the server resumes from the client's own position, so a command that
// finishes during the outage is not lost. This is what makes an upgrade invisible rather than merely
// survivable.
func TestOutputDuringAnOutageIsDeliveredOnReconnect(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "catchup", "--", "/bin/sh")
	c.waitReady()

	// A command that prints after a delay, started before the outage and finishing during it.
	c.typeLine("(sleep 2; echo PRINTED_WHILE_DOWN) &")

	e.mustRun("server", "stop")
	e.waitServerGone()
	// Long enough for the background command to have printed with nobody watching.
	time.Sleep(3 * time.Second)

	e.mustRun("list")

	// The output arrives once the client is back, because the shim held it and the server resumed from
	// where this client had read to.
	c.waitForOutput("PRINTED_WHILE_DOWN", 25*time.Second)
}

// Several restarts in a row must keep working, not degrade.
//
// A reconnect that leaks a subscription or advances a resume point wrongly would work once and fail later,
// which is the shape of the socket-deletion bug found earlier: the first restart was fine and the second
// was not.
func TestClientSurvivesRepeatedRestarts(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "repeat", "--", "/bin/sh")
	c.waitReady()

	for i := range 3 {
		e.restartServer()
		e.waitFor("the client to reconnect", 20*time.Second, func() bool {
			s, ok := e.session("repeat")
			return ok && s.Clients == 1
		})

		// Usable after each one, since "still listed" is not the same as "still working".
		marker := "ROUND_" + itoa(i)
		c.typeLine("echo " + marker)
		c.waitForOutput(marker, 20*time.Second)
	}
}

// A command issued while no server is running must start one and adopt the session.
//
// This is the other half of an upgrade, and the behavior a user sees after `cm server stop`: their next
// command brings a server back and the session is still there. Worth asserting explicitly, because the
// obvious-seeming alternative -- failing because no server is running -- would make an upgrade a two-step
// dance instead of invisible.
//
// An earlier version of this test asserted that failure, on the strength of a comment in wait.go claiming
// the command deliberately does not auto-start. The comment was wrong; the code was right. Both are fixed.
func TestACommandStartsAServerAndAdoptsSessions(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "adopted", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("adopted")
		return ok && s.State == "running"
	})
	before, _ := e.session("adopted")

	e.mustRun("server", "stop")
	e.waitServerGone()

	// No server, and a command that has to reach one. It starts a server, which adopts the session, and
	// answers about it.
	e.mustRun("wait", "adopted", "--until", "idle", "--timeout", "10s")

	after, ok := e.session("adopted")
	if !ok {
		t.Fatal("the session was not adopted by the new server")
	}
	if after.ShellPID != before.ShellPID {
		t.Errorf("shell pid changed: %d -> %d, want the same process to have been adopted",
			before.ShellPID, after.ShellPID)
	}
}
