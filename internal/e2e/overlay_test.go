package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// The overlay, driven by real keypresses through a real pty against a real server.
//
// Worth an e2e test rather than leaving it to the unit tests because the value of the feature is a chain:
// a keystroke the client intercepts, a child process it spawns, an RPC that child makes, and a session
// the server renames. Every hop is covered in isolation, and nothing but this checks that pressing two
// keys names the session you are looking at.
func TestOverlayBindsTheSessionItIsIn(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "work", "--", "/bin/sh")
	c.waitReady()

	// The prefix key, as the byte a terminal sends. The bar naming the session is what proves the client
	// intercepted it rather than passing it to the shell.
	c.write([]byte{0x1d})
	c.waitForOutput("cm work", 15*time.Second)

	// A command line, then the name. Typed into the overlay, so none of it reaches the shell.
	c.write([]byte(":bind renamed\r"))
	// Checked against the session's set of names rather than by looking one up: a bind adds a name, and
	// `cm list` still reports "work" as the label, so a lookup by the new name finds nothing even when the
	// bind worked. That is what this test asserted first, and it failed for that reason rather than any
	// other.
	e.waitFor("the session to gain the name the overlay bound", 20*time.Second, func() bool {
		s, ok := e.session("work")
		return ok && slices.Contains(s.Names, "renamed")
	})

	// And the shell never saw any of it, which is the other half: an overlay that forwarded what it
	// consumed would leave ":bind renamed" on the command line.
	if got := c.output(); strings.Contains(got, "bind renamed\r\n") {
		t.Errorf("the shell echoed the overlay's command line, so the keystrokes were forwarded: %q", got)
	}
}

// Pressing the detach key from inside the overlay forwards it to the program instead of detaching.
//
// This is the regression test for a race found only in a real terminal: closing the overlay repaints,
// which closes the connection, and the forwarded byte sent on that connection was sometimes lost. It
// killed a foreground `sleep` on some attempts and not others. The bytes are carried into the reconnect
// now.
//
// Signals are turned off in the session so the byte can be *observed* rather than inferred from something
// dying: `cat -v` prints it as ^\, which is evidence the pty received exactly that byte.
func TestOverlayForwardsTheDetachKeyToTheProgram(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "work", "--", "/bin/sh")
	c.waitReady()

	c.typeLine("stty -isig; cat -v")
	// A control byte the overlay is not involved in, which proves cat is running and reading before the
	// case under test sends anything.
	c.write([]byte("A"))
	c.waitForOutput("A", 15*time.Second)

	// prefix then q: forward the detach key to the program.
	c.write([]byte{0x1d})
	c.waitForOutput("cm work", 15*time.Second)
	c.write([]byte("q"))
	c.waitForOutput("^\\", 20*time.Second)

	// And the client did not detach, which is what makes the key the program's rather than cm's.
	if s, ok := e.session("work"); !ok || s.Clients != 1 {
		t.Errorf("session = %+v, want it still holding its one client: forwarding the key must not detach",
			s)
	}
}

// A style the config file cannot express is refused when the client starts, naming the setting.
//
// The alternative is what the first version of this did: draw an unstyled overlay and leave the user
// wondering which of three settings they got wrong. There is a config file involved, so the message has to
// say which key.
func TestAttachRejectsAnUnreadableOverlayStyle(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.writeConfig("[overlay]\nbody = \"chartreuse on chartreuse\"\n")
	r := e.run("attach", "badstyle", "--", "/bin/sh")
	if r.code == 0 {
		t.Errorf("exit code = 0 for a style with no such colour, want non-zero\nstdout: %s", r.stdout)
	}
	if !strings.Contains(r.stderr, "overlay.body") {
		t.Errorf("stderr = %q, want it to name overlay.body as the setting at fault", r.stderr)
	}
}
