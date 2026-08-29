package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for windows that had no way to be reached before internal/fault.
//
// Each one holds a client at a named point and then does something to the session underneath it. The
// states they produce are all reachable in normal use, by a slow machine or an unlucky interleaving, and
// none of them was covered: a client that has been told a session exists and has received none of it is
// the most fragile moment in an attachment, and until now a test could only get there by luck.

// bgCmd is a cm invocation running in the background, so a test can act while it is blocked.
//
// The harness runs commands to completion, which is right for almost everything and useless here: the
// point is to have a client stopped inside cm while the test does something else.
type bgCmd struct {
	cmd    *exec.Cmd
	out    *bytes.Buffer
	errBuf *bytes.Buffer
	done   chan error
}

func (e *env) startInBackground(t *testing.T, args ...string) *bgCmd {
	t.Helper()

	cmd := exec.Command(e.bin, args...)
	cmd.Env = e.environ()
	cmd.Dir = e.state
	b := &bgCmd{
		cmd:    cmd,
		out:    &bytes.Buffer{},
		errBuf: &bytes.Buffer{},
		done:   make(chan error, 1),
	}
	cmd.Stdout = b.out
	cmd.Stderr = b.errBuf
	// /dev/null rather than inherited stdin, matching runWithin: a client with a tty behaves differently.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { devNull.Close() })
	cmd.Stdin = devNull

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting cm %v: %v", args, err)
	}
	go func() { b.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	return b
}

// wait blocks until the command exits, failing the test if it does not.
func (b *bgCmd) wait(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case err := <-b.done:
		return err
	case <-time.After(scaleTimeout(within)):
		t.Fatalf("cm %v did not exit within %s: it is stuck.\nstdout: %s\nstderr: %s",
			b.cmd.Args[1:], within, b.out.String(), b.errBuf.String())
		return nil
	}
}

// release lets a paused fault point continue.
func release(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("releasing the pause: %v", err)
	}
	f.Close()
}

// TestSessionEndsWhileAClientIsInTheAttachGap holds a follower between Opened and its first byte, then
// ends the session under it.
//
// A client in that gap has been told the session exists and has nothing else. If the shell exits there,
// the exit is not on the stream yet and the log closes before the client subscribes, so the two paths that
// report an exit are both behind it. Whether it hears anything at all is the question, and until the fault
// point existed the window was a few instructions wide.
func TestSessionEndsWhileAClientIsInTheAttachGap(t *testing.T) {
	skipIfShort(t)

	gate := filepath.Join(t.TempDir(), "release")
	e := newEnvWith(t, cmHooksBinary(t), "",
		"CM_TESTHOOK_FAULTS=after-attach-opened:pause="+gate)

	e.mustRun("attach", "--no-attach", "gap", "--", "/bin/sh", "-c", "printf 'ALIVE\\n'; sleep 30")
	e.waitForOutputInSession("gap", "ALIVE", 20*time.Second)

	follower := e.startInBackground(t, "read", "--follow", "gap")

	// The server has counted the client, so the attach handler is at or past the pause. Polled rather than
	// slept, since a sleep long enough to be safe is also long enough to be slow.
	e.waitFor("the follower to be counted", 20*time.Second, func() bool {
		return e.sessionDetail(t, "gap").Clients >= 1
	})

	// Ended while it waits. This is the state the test exists for: the exit happens after Opened and before
	// the client has subscribed to anything.
	e.mustRun("kill", "gap")
	release(t, gate)

	// It has to return, and having returned it has to have shown the session's output. A follower that
	// exits instantly with nothing satisfies "did not hang" while being just as useless to whoever ran it,
	// so both are asserted: the exit status is not, since a killed session legitimately has no clean one.
	err := follower.wait(t, 20*time.Second)
	t.Logf("follower exited with %v", err)
	if got := follower.out.String(); !strings.Contains(got, "ALIVE") {
		t.Errorf("follower stdout = %q, want the session's output: a follower released into a dead "+
			"session still has to report what the session produced", got)
	}
}

// TestServerRestartsWhileAClientIsInTheAttachGap holds a client in the same gap and restarts the server.
//
// The client has a resume position from Opened and has received nothing at it. A restart makes the new
// server adopt the session and the client resume from that position, which is the sequence-number-space
// path where this repo's most expensive bugs live. Resuming from a position that was correct for the old
// server and means something else to the new one is exactly the shape of the adoption bug.
func TestServerRestartsWhileAClientIsInTheAttachGap(t *testing.T) {
	skipIfShort(t)

	gate := filepath.Join(t.TempDir(), "release")
	e := newEnvWith(t, cmHooksBinary(t), "",
		"CM_TESTHOOK_FAULTS=after-attach-opened:pause="+gate)

	// Two halves with a prompt marker between them. The marker matters because the rewrite that lengthens
	// one is what makes the two numbering spaces diverge; without it the positions agree and the bug this
	// targets cannot appear. The second half is printed late, so it lands after the restart, which is the
	// output a resumed client has to receive and the thing a wrong resume position loses.
	const after = "AFTER-RESTART"
	e.mustRun("attach", "--no-attach", "gaprestart", "--", "/bin/sh", "-c",
		`printf 'FIRST\n'; printf '\033]133;A\007prompt\r\n'; sleep 8; printf '`+after+`\n'; sleep 30`)
	e.waitForOutputInSession("gaprestart", "FIRST", 20*time.Second)

	follower := e.startInBackground(t, "read", "--follow", "gaprestart", "--timeout", "20s")
	e.waitFor("the follower to be counted", 20*time.Second, func() bool {
		return e.sessionDetail(t, "gaprestart").Clients >= 1
	})

	e.restartServer()
	e.list() // any command starts the replacement, which adopts the session
	release(t, gate)

	// The session survived, which is the whole promise of the shim owning the pty.
	e.waitFor("the session to be adopted and running", 25*time.Second, func() bool {
		return e.sessionDetail(t, "gaprestart").State == "running"
	})

	// The load-bearing assertion: the follower, released into a restarted server, receives output produced
	// after the restart. A resume position that was correct for the old server and means something else to
	// the new one loses exactly this, and every other check here passes while it does.
	e.waitFor("the follower to receive output produced after the restart", 30*time.Second, func() bool {
		return strings.Contains(follower.out.String(), after)
	})

	_ = follower.wait(t, 30*time.Second)
}

// TestSlowShimStartupStillAttaches delays a shim before it binds its socket.
//
// The server waits ten seconds for a shim to become ready. That path was measured at 10.38s per attempt
// against 0.36s when a session reference was validated as a name and the shim exited before binding, and a
// session named `work` worked throughout, which is what hid it. The slow-but-fine case is the one nothing
// covered: a loaded machine produces it, and what a user should see is a session, slightly late, rather
// than an error.
func TestSlowShimStartupStillAttaches(t *testing.T) {
	skipIfShort(t)

	// Well inside the server's ten-second budget, so the correct outcome is success rather than a timeout.
	e := newEnvWith(t, cmHooksBinary(t), "",
		"CM_TESTHOOK_FAULTS=before-shim-ready:delay=2s")

	// Longer than the delay and shorter than a hang.
	out := e.mustRunWithin(30*time.Second, "attach", "--no-attach", "slow", "--",
		"/bin/sh", "-c", "printf 'SLOWSTART\\n'; sleep 30")
	if out == "" {
		t.Error("attach printed nothing for a session that started slowly")
	}

	e.waitForOutputInSession("slow", "SLOWSTART", 25*time.Second)
	if got := e.sessionDetail(t, "slow"); got.State != "running" {
		t.Errorf("session state = %q after a slow shim start, want running", got.State)
	}
}
