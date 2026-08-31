package e2e

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// ptyClient is a `cm attach` process with a real controlling terminal, plus a reader draining it.
//
// A pty rather than pipes, because the client behaves differently without one: it checks for a terminal
// to decide whether EOF on stdin means "the window closed" or "input is simply finished", and it only
// puts the terminal into raw mode when there is one. Testing attach over pipes exercises a path no
// interactive user takes.
type ptyClient struct {
	t    *testing.T
	ptmx *os.File
	cmd  *exec.Cmd

	mu   sync.Mutex
	seen strings.Builder
}

// attachOnPty starts `cm attach` on a pty and begins draining its output.
//
// The output is read continuously on its own goroutine rather than on demand. A pty does not support
// read deadlines ("file type does not support deadline"), so a read-when-asked helper blocks forever
// once the expected text has arrived, and the client then stalls behind a full pty buffer. That looked
// exactly like the detach key being ignored.
func attachOnPty(t *testing.T, e *env, args ...string) *ptyClient {
	t.Helper()

	cmd := exec.Command(e.bin, append([]string{"attach"}, args...)...)
	cmd.Env = e.environ()
	cmd.Dir = e.state

	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty.Start() error = %v", err)
	}
	// A definite size, so a restored screen has a known geometry rather than the pty's default.
	// Pixel dimensions as well as cells, matching attachOnPtyWithEnv and matching a real terminal, which
	// reports both. Without them the model has no cell size, so it calls every image placement off-screen and
	// no image restore is reachable at all: a graphics test then passes on a path it never exercised.
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80, X: 800, Y: 480}); err != nil {
		t.Fatalf("Setsize() error = %v", err)
	}

	c := &ptyClient{t: t, ptmx: ptmx, cmd: cmd}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				c.mu.Lock()
				c.seen.Write(buf[:n])
				c.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		ptmx.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return c
}

// waitReady blocks until the client has painted something, so input sent afterwards is not lost.
//
// Keystrokes written before the client has finished attaching go into the pty buffer and are read by
// the client before it has a session to forward them to, so they vanish. That looked like the shell
// ignoring input, and it is a real ordering requirement rather than a test artifact: anything driving
// cm programmatically has to wait for it to be ready.
//
// Waits for the shell's prompt rather than for anything cm emits, because a prompt is the real readiness
// signal: it means the shell is accepting commands, which is the thing being waited for.
//
// Waiting on cm's own output would be waiting on the wrong layer. The escape sequences a client writes on
// attach come from the emulator serializing a screen, so they say a screen was painted rather than that the
// shell is listening -- and back when a no-cgo build existed they were absent entirely, so this hung with only
// "# " on the pty.
func (c *ptyClient) waitReady() {
	c.t.Helper()
	// Both common prompt endings, since the shell here is whatever /bin/sh is: "$ " for a user and "# "
	// for root, which is what a container runs as.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got := c.output()
		if strings.Contains(got, "$ ") || strings.Contains(got, "# ") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting for a shell prompt on the pty, got %q", c.output())
}

// output returns everything the client has written to its terminal so far.
func (c *ptyClient) output() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen.String()
}

// typeLine sends a line as if typed.
func (c *ptyClient) typeLine(text string) {
	c.t.Helper()
	if _, err := c.ptmx.Write([]byte(text + "\n")); err != nil {
		c.t.Fatalf("writing to the pty: %v", err)
	}
}

// write sends raw bytes, for keys that have no convenience method.
//
// Bytes rather than a string, since these are control characters and spelling them as escapes in a string
// literal invites the mangling that makes a terminal test silently pass.
func (c *ptyClient) write(b []byte) {
	c.t.Helper()
	if _, err := c.ptmx.Write(b); err != nil {
		c.t.Fatalf("writing to the pty: %v", err)
	}
}

// detachKey sends ctrl-\, the default detach key, as the literal byte a terminal would send.
func (c *ptyClient) detachKey() {
	c.t.Helper()
	if _, err := c.ptmx.Write([]byte{0x1c}); err != nil {
		c.t.Fatalf("sending the detach key: %v", err)
	}
}

// waitExit blocks until the client process has exited.
//
// Distinct from waiting for the session to report zero clients, and both are needed: the server
// deregisters a client before the process is gone, so a client that was told to detach and then
// reconnected would pass the count check while still running. That is exactly the failure mode of the
// client treating a server-initiated close as an outage, so the process itself has to be observed.
//
// Waited on a goroutine because Wait blocks, and the cleanup registered by attachOnPty also calls it;
// a second Wait on an already-reaped process returns an error rather than hanging, which is why the
// result is ignored here.
func (c *ptyClient) waitExit(timeout time.Duration) {
	c.t.Helper()

	done := make(chan struct{})
	go func() {
		_, _ = c.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		c.t.Fatalf("the client process did not exit within %s after being asked to detach, "+
			"so it is holding the terminal or has reconnected", timeout)
	}
}

// kill terminates the client without letting it detach, which is what closing a window does.
func (c *ptyClient) kill() {
	c.t.Helper()
	if err := c.cmd.Process.Kill(); err != nil {
		c.t.Fatalf("killing the client: %v", err)
	}
	_, _ = c.cmd.Process.Wait()
}

// waitForOutput blocks until the client's terminal shows want, and returns everything seen.
func (c *ptyClient) waitForOutput(want string, timeout time.Duration) string {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := c.output(); strings.Contains(got, want) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting for %q on the pty, got %q", want, c.output())
	return ""
}

// Attaching, detaching, and reattaching must repaint the screen.
//
// This is the whole point of the program, and it only works through a real terminal: the restore blob
// is escape sequences written to a tty, so a test over pipes would assert that bytes were sent without
// checking that they reconstruct anything.
//
// The detach key is ctrl-\, sent as a literal 0x1c byte.
func TestAttachDetachReattachRepaintsScreen(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "paint", "--", "/bin/sh")
	c.waitReady()
	// A distinctive marker rather than a prompt, since the prompt depends on the shell's configuration.
	c.typeLine("echo SCREEN_MARKER")
	c.waitForOutput("SCREEN_MARKER", 15*time.Second)

	// Detach, leaving the session running.
	c.detachKey()
	e.waitFor("the client to detach", 10*time.Second, func() bool {
		s, ok := e.session("paint")
		return ok && s.Clients == 0
	})
	if s, _ := e.session("paint"); s.State != "running" {
		t.Errorf("state after detach = %q, want the session still running", s.State)
	}

	// Reattach on a fresh terminal. The marker has to come back, which it can only do if the server
	// serialized the screen and the client painted it.
	c2 := attachOnPty(t, e, "paint")
	got := c2.waitForOutput("SCREEN_MARKER", 15*time.Second)

	// No literal escape sequences rendered as text. This is the artifact class that a restore gets
	// wrong: a client resuming mid-sequence loses the ESC and the parameters show up as characters.
	if strings.Contains(got, "[24;1H") || strings.Contains(got, "0;10;1c") {
		t.Errorf("restored screen contains escape parameters as literal text: %q", got)
	}
}

// A session must outlive the terminal its client was running in.
//
// Killing the client without a detach is what closing a terminal window does. The session survives it,
// which is what makes reopening a terminal restore the work in it. There was once an --own flag that
// made this case end the session instead; nothing does now, so a lost client is never fatal.
func TestSessionSurvivesClientBeingKilled(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "orphan", "--", "/bin/sh")
	c.waitReady()
	c.typeLine("echo STILL_HERE")
	c.waitForOutput("STILL_HERE", 15*time.Second)

	// SIGKILL, so no cleanup runs: the client gets no chance to detach politely, which is the point.
	c.kill()

	e.waitFor("the client to be gone", 10*time.Second, func() bool {
		s, ok := e.session("orphan")
		return ok && s.Clients == 0
	})
	if s, ok := e.session("orphan"); !ok || s.State != "running" {
		t.Errorf("session after the client was killed = %+v (found=%v), want it still running", s, ok)
	}
}

// --detach-key overrides the configured key for one attachment.
//
// Asked for after attaching to cm from inside a zmx window: zmx claims ctrl-\ and has no way to change it, so
// the outer client saw the key first and the inner cm never received it. Nesting one multiplexer in another is
// exactly the situation during a migration, and the fix has to be per-attachment rather than a global setting.
//
// A real pty and real keystroke bytes, since the whole mechanism is about which client sees a byte first.
func TestAttachDetachKeyFlagOverridesTheDefault(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// ctrl-o rather than the default ctrl-\.
	c := attachOnPty(t, e, "customkey", "--detach-key", "ctrl-o", "--", "/bin/sh")
	c.waitReady()
	c.typeLine("echo READY_MARK")
	c.waitForOutput("READY_MARK", 15*time.Second)

	// The default key must now reach the shell rather than detaching. 0x1c is ctrl-\.
	c.write([]byte{0x1c})
	time.Sleep(500 * time.Millisecond)
	s, ok := e.session("customkey")
	if !ok {
		t.Fatal("session vanished")
	}
	if s.Clients != 1 {
		t.Errorf("clients = %d after sending the default detach key, want 1: the flag should have "+
			"disabled it", s.Clients)
	}

	// And the configured key detaches. 0x0f is ctrl-o.
	c.write([]byte{0x0f})
	e.waitFor("the client to detach on the custom key", 15*time.Second, func() bool {
		s, ok := e.session("customkey")
		return ok && s.Clients == 0
	})

	// The session survives, which is what detaching means.
	after, ok := e.session("customkey")
	if !ok {
		t.Fatal("the session was destroyed rather than detached from")
	}
	if after.State != "running" {
		t.Errorf("state = %q after detaching, want running", after.State)
	}
}

// An invalid --detach-key is rejected rather than silently ignored.
//
// Silently falling back to the default would be worse than an error: the user asked for a key precisely because
// the default does not reach them, so ignoring it means the client cannot be detached at all.
func TestAttachRejectsAnInvalidDetachKey(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	r := e.run("attach", "badkey", "--detach-key", "not-a-key", "--", "/bin/sh")
	if r.code == 0 {
		t.Errorf("exit code = 0 for an invalid detach key, want non-zero\nstdout: %s", r.stdout)
	}
	if !strings.Contains(r.stderr, "detach key") {
		t.Errorf("stderr = %q, want it to name the detach key as the problem", r.stderr)
	}
}
