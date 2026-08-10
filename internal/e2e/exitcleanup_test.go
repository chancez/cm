package e2e

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// attachInBackground follows a session until the returned function is called.
//
// Needed because the behavior under test depends on a client having attached, and the plain harness
// helpers all run a command to completion. Read-only so it cannot disturb the session it is watching, and
// stopped through the returned function rather than left running, since a process still talking to a
// torn-down sandbox panics inside the harness and reports as a failure in an unrelated test.
func (e *env) attachInBackground(t *testing.T, session string) func() {
	t.Helper()

	cmd := exec.Command(e.bin, "attach", "--read-only", session)
	cmd.Env = e.environ()
	cmd.Dir = e.state
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	cmd.Stdin = devNull
	if err := cmd.Start(); err != nil {
		devNull.Close()
		t.Fatalf("starting a background attach: %v", err)
	}

	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
			devNull.Close()
		})
	}
	t.Cleanup(stop)

	// Wait until the server sees it, so a send that follows is ordered after the attach.
	e.waitFor("a client to attach to "+session, 15*time.Second, func() bool {
		s, ok := e.session(session)
		return ok && s.Clients > 0
	})
	return stop
}

// An interactive session whose shell exits is forgotten at once.
//
// Found by using cm rather than by review: closing a terminal left an exited(0) record in `cm list` for
// the five-minute forget interval, and with one session per window they accumulated visibly. zmx draws the
// same line -- `zmx a foo` then `exit` removes the session immediately -- which is what made the lingering
// record read as a bug rather than a policy.
//
// Attached first, because that is the distinction being tested: a person sitting in a session saw its
// output and typed exit, so there is no status left to collect.
func TestInteractiveSessionIsForgottenOnExit(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("attach", "--no-attach", "interactive", "--", "/bin/sh")

	// A read-only follower rather than a full client, since an e2e test has no terminal. It still
	// registers as an attachment, which is what the manager keys the decision on: the question is whether
	// anyone was ever watching, not what kind of client they were.
	stop := e.attachInBackground(t, "interactive")

	e.mustRun("send", "interactive", "echo hello world", "--enter")
	e.waitForOutputInSession("interactive", "hello world", 10*time.Second)

	e.mustRun("send", "interactive", "exit", "--enter")
	stop()

	// Gone rather than exited: the record is not merely marked, it is removed.
	e.waitFor("the session record to be forgotten", 15*time.Second, func() bool {
		_, ok := e.session("interactive")
		return !ok
	})
}

// A `cm run` task keeps its record, since its exit status is the product.
//
// The control for the test above. Forgetting every exited session would break `cm run`, which reads the
// status from the record after the command finishes, so the two cases must be told apart rather than
// treated alike.
func TestRunTaskKeepsItsRecordAfterExit(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// Exits non-zero, so there is a status worth keeping.
	if r := e.run("run", "--session", "task", "--", "/bin/sh", "-c", "echo TASKOUT; exit 3"); r.code != 3 {
		t.Fatalf("run exited %d, want 3: %s", r.code, r.stderr)
	}

	s, ok := e.session("task")
	if !ok {
		t.Fatal("the task's record was forgotten, so its exit status is unreadable")
	}
	if s.State != "exited" || s.ExitCode != 3 {
		t.Errorf("record = state %q exit %d, want exited 3", s.State, s.ExitCode)
	}
	// And its output, which is what CaptureOutput is for.
	if out := e.mustRun("read", "task", "--lines", "5"); !strings.Contains(out, "TASKOUT") {
		t.Errorf("read = %q, want the task's output still readable", out)
	}
}

// A session nobody ever attached to keeps its record.
//
// The record is the only evidence it ran at all, so forgetting it would lose the outcome with nothing
// having observed it. Distinguished from the interactive case by attachment rather than by client count,
// since a client detaching as its shell exits is exactly the shape of typing exit.
func TestDetachedSessionKeepsItsRecordAfterExit(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("attach", "--no-attach", "never-watched", "--", "/bin/sh", "-c", "exit 0")

	// Present shortly after ending, rather than removed the moment the shell exits.
	e.waitFor("the session to be recorded as exited", 15*time.Second, func() bool {
		s, ok := e.session("never-watched")
		return ok && s.State == "exited"
	})

	// Still there a moment later: this is kept for the forget interval, not deleted at once.
	time.Sleep(2 * time.Second)
	if _, ok := e.session("never-watched"); !ok {
		t.Error("a session nobody attached to was forgotten immediately, " +
			"want its record kept as the only evidence it ran")
	}
}

// Replacing an ended session is recorded rather than silent.
//
// Attaching to a name whose shell had exited starts a fresh shell under it and deletes the old row, taking
// the exit status and pid that were the only evidence of the previous run. That was destructive and
// invisible. Logged rather than refused, because a terminal emulator restoring a saved window attaches by
// name and must get a working session either way -- what it must not do is pretend nothing was there.
func TestReplacingAnEndedSessionIsLogged(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// A task record survives its command, so there is something to replace.
	if r := e.run("run", "--session", "reused", "--", "/bin/sh", "-c", "exit 7"); r.code != 7 {
		t.Fatalf("run exited %d, want 7", r.code)
	}

	// A long-lived command, so a working replacement would be observably running.
	e.mustRun("attach", "--no-attach", "reused", "--", "/bin/sh", "-c", "sleep 120")

	// Deliberately not asserting that the replacement runs, because today it usually does not: the new
	// shim cannot claim the socket while the old one is still shutting down and holds it, so it dies with
	// "already served by a live shim" and the stale row survives. That is a separate, pre-existing bug --
	// a binary built before this change fails the same way -- and it is recorded in docs/ideas.md rather
	// than asserted here, so this test does not fail for a reason it is not about.
	//
	// What is asserted is the logging, which is what this change added.
	e.waitFor("the replacement to be logged", 15*time.Second, func() bool {
		return strings.Contains(
			e.readFileOrEmpty(e.serverLogPath()), "replacing an ended session")
	})

	// The discarded incarnation is named, including the status that was lost.
	log := e.readFileOrEmpty(e.serverLogPath())
	if !strings.Contains(log, "previous_exit_code=7") {
		t.Errorf("server log does not record the discarded exit status:\n%s", tailOf(log))
	}
}

// tailOf returns the last few lines of a log, for a failure message.
func tailOf(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	return strings.Join(lines, "\n")
}
