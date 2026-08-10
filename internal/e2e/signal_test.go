package e2e

import (
	"strings"
	"testing"
	"time"
)

// jobEnded reports whether a job started with startInterruptibleJob has finished.
//
// Detected by a marker the shell prints after the job, not by waiting for idle. That distinction cost a
// debugging pass: these sessions run /bin/sh, which reports no OSC 133, so `--wait idle` can never
// succeed and a helper built on it reports "still running" forever. Both signal tests failed against a
// working implementation until this was replaced -- the harness was lying, not the feature.
//
// Read through cm rather than with pgrep, so the test observes what the session printed rather than the
// host's process table, where it could match a process belonging to another test.
//
// A session that has ended counts as not having printed the marker rather than failing the read. That
// happens when a signal reaches the shell instead of the job, which is a wrong answer these tests want
// to report in their own words rather than as an unrelated error from `cm read`.
func (e *env) jobEnded(session string) bool {
	e.t.Helper()
	r := e.run("read", session, "--lines", "20")
	if r.code != 0 {
		return false
	}
	return strings.Contains(r.stdout, jobEndedMarker)
}

// jobEndedMarker is printed once the job the shell was running has finished, however it finished.
const jobEndedMarker = "JOBENDED"

// startInterruptibleJob runs a long job that ignores the given signal, followed by a marker.
//
// Two details are load-bearing, and both cost a debugging pass.
//
// The trap runs in a *child* shell rather than the session's own. `trap ” INT; sleep 300` protects the
// shell, not the sleep it forks, so ctrl-c killed the job anyway and the test could not show that
// signalling was necessary. Wrapping the job in `sh -c` puts the trap in the process that actually
// receives the signal.
//
// The marker is assembled at runtime, as JOB${x}ENDED with x unset, so the literal string never appears
// in the command line the shell echoes. Spelling it directly made every check match the echo of the
// command that had not run yet, which reads as the job having finished the moment it started.
func (e *env) startInterruptibleJob(session, ignore string) {
	e.t.Helper()
	e.mustRun("attach", "--no-attach", session, "--", "/bin/sh")
	e.mustRun("send", session,
		`sh -c "trap '' `+ignore+`; sleep 300"; echo JOB${x}ENDED`, "--enter")
	e.waitForOutputInSession(session, "sleep 300", 10*time.Second)
}

// waitForJobToEnd blocks until the marker appears, reporting whether it did.
func (e *env) waitForJobToEnd(session string, timeout time.Duration) bool {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if e.jobEnded(session) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// A control key sent with --key reaches the pty and interrupts what is running.
//
// The behavior a caller expects from "send ctrl-c", and what previously required producing the byte by
// hand because every spelling was typed as text instead.
func TestSendKeyInterrupts(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "k", "--", "/bin/sh")
	// A shell that reports nothing, so this cannot lean on OSC 133: the assertion is the marker text
	// the interrupted shell prints, which is observable either way.
	e.mustRun("send", "k", "sleep 300", "--enter")
	e.waitForOutputInSession("k", "sleep 300", 10*time.Second)

	e.mustRun("send", "k", "--key", "ctrl-c")

	// ^C is what the pty echoes when the line discipline turns the byte into an interrupt, so its
	// presence is evidence the key was interpreted rather than typed.
	e.waitForOutputInSession("k", "^C", 10*time.Second)

	// And the shell is usable again, which a typed "ctrl-c" would not have left it.
	e.mustRun("send", "k", "echo recovered", "--enter")
	e.waitForOutputInSession("k", "recovered", 10*time.Second)
}

// An unknown key name is refused rather than sent as text.
//
// The silent failure this whole flag exists to remove: typing "ctrlc" onto a command line while a build
// keeps running reads as cm having ignored the request.
func TestSendKeyRejectsUnknownNames(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "k", "--", "/bin/sh")

	for _, key := range []string{"ctrlc", "nosuchkey", "ctrl-abc"} {
		r := e.run("send", "k", "--key", key)
		if r.code == 0 {
			t.Errorf("send --key %q exited 0, want it refused", key)
		}
		if !strings.Contains(r.stderr, "unknown key") && !strings.Contains(r.stderr, "ctrl-") {
			t.Errorf("send --key %q stderr = %q, want it to explain the spelling", key, r.stderr)
		}
	}

	// Nothing was typed by any refused call, which is the part that matters: a rejected key must not
	// reach the session as characters.
	out := e.mustRun("read", "k", "--lines", "20")
	for _, key := range []string{"ctrlc", "nosuchkey"} {
		if strings.Contains(out, key) {
			t.Errorf("a refused key name reached the session as text:\n%s", out)
		}
	}
}

// Text and keys combine, with keys sent after the text.
func TestSendKeyAfterText(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "k", "--", "/bin/sh")

	// --key enter rather than --enter, so the ordering rule is what is under test.
	e.mustRun("send", "k", "echo from-key", "--key", "enter")
	e.waitForOutputInSession("k", "from-key", 10*time.Second)
}

// With neither text nor a key there is nothing to send, and that is an error.
func TestSendRequiresSomethingToSend(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "k", "--", "/bin/sh")

	if r := e.run("send", "k"); r.code == 0 {
		t.Error("send with no text and no --key exited 0, want it refused")
	}
}

// cm signal stops a job that a keypress cannot.
//
// The case that justifies having both: SIGINT is trapped, so the control character reaches the program
// and is discarded, while a signal is delivered regardless. Verified in both directions here, since
// "signal worked" alone would not show that the key was genuinely insufficient.
func TestSignalStopsWhatAKeyCannot(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.startInterruptibleJob("k", "INT")

	// The key cannot get through, so the job must still be running afterwards.
	e.mustRun("send", "k", "--key", "ctrl-c")
	time.Sleep(time.Second)
	if e.jobEnded("k") {
		t.Fatal("the job ended after --key ctrl-c, but SIGINT was trapped: " +
			"this test cannot show that signalling is necessary")
	}

	// The signal does.
	e.mustRun("signal", "k", "term")
	if !e.waitForJobToEnd("k", 15*time.Second) {
		t.Error("the job survived cm signal term, want it stopped")
	}

	// The session itself is untouched: the signal went to the job's group, not to the shell.
	if s, ok := e.session("k"); !ok || s.State != "running" {
		t.Errorf("session state = %q after signalling its job, want it still running", s.State)
	}
}

// The signal reaches the pty's foreground process group, not the shell's own group.
//
// A shell with job control puts each job in a new group and gives that group the terminal, so the
// shell's group holds only the shell. Signalling it reported success and left the job running, which is
// the worst possible shape: the caller is told the job was stopped.
//
// Distinguished from the test above by using --process-only as a control: that deliberately signals the
// shell alone, so if it stopped the job too, the group targeting would not be what made the difference.
func TestSignalTargetsTheForegroundGroup(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.startInterruptibleJob("k", "TERM")

	// The control: signalling the shell alone must not reach the job.
	e.mustRun("signal", "k", "--process-only", "term")
	time.Sleep(time.Second)
	if e.jobEnded("k") {
		t.Fatal("--process-only ended the job, so this test cannot show group targeting matters")
	}

	// The group form reaches the job, because it asks the pty which group is in the foreground.
	e.mustRun("signal", "k", "kill")
	if !e.waitForJobToEnd("k", 15*time.Second) {
		t.Error("the job survived a group signal, want the foreground group targeted")
	}
}

func TestSignalAcceptsNamesAndNumbers(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "k", "--", "/bin/sh")

	// Every spelling a caller coming from `kill` would try.
	for _, spec := range []string{"int", "INT", "SIGINT", "sigint", "2"} {
		if r := e.run("signal", "k", spec); r.code != 0 {
			t.Errorf("signal %q exited %d, want it accepted: %s", spec, r.code, r.stderr)
		}
	}
	// And what is not a signal.
	for _, spec := range []string{"nosuchsignal", "0", "-1"} {
		if r := e.run("signal", "k", spec); r.code == 0 {
			t.Errorf("signal %q exited 0, want it refused", spec)
		}
	}
}

// Signalling a name nothing knows about is reported rather than counted as success.
//
// This used to signal a session that had just finished and assert the "has ended" message. That raced:
// the message depends on the session having left the server's registry, which happens on a background
// goroutine after the command finishes, so on a slower CI runner the signal arrived while the entry was
// still there and the command succeeded. It passed locally 8 runs out of 8, which is why it shipped.
//
// Waiting for `state == "exited"` does not close the window either, since a live session reports itself
// exited before its record is written back. The ended-versus-deregistered distinction is now covered
// deterministically in internal/server/signalended_test.go, where the registry state can be constructed
// instead of waited for.
//
// What is left here is the end-to-end half that does not race: the CLI reaches the server, the server
// refuses, and the exit status and message come back.
func TestSignalOnAnUnknownSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	// A session exists, so the server is running and the failure below is about the name rather than
	// about there being nothing to talk to.
	e.mustRun("run", "--session", "present", "-d", "--", "/bin/sh", "-c", "sleep 30")

	r := e.run("signal", "no-such-session", "term")
	if r.code == 0 {
		t.Error("signalling an unknown session exited 0, want an error")
	}
	if !strings.Contains(r.stderr, "not found") {
		t.Errorf("stderr = %q, want it to say the session was not found", r.stderr)
	}
}

// A tag selector signals every session in a group.
func TestSignalByTag(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	for _, name := range []string{"g1", "g2"} {
		e.mustRun("run", "--session", name, "--tag", "grp=x", "-d",
			"--", "/bin/sh", "-c", "sleep 300")
	}
	e.mustRun("run", "--session", "other", "-d", "--", "/bin/sh", "-c", "sleep 300")

	e.mustRun("signal", "--tag", "grp=x", "kill")

	e.waitFor("the tagged sessions to end", 15*time.Second, func() bool {
		for _, name := range []string{"g1", "g2"} {
			s, ok := e.session(name)
			if ok && s.State == "running" {
				return false
			}
		}
		return true
	})

	// The untagged session is untouched, which is what makes a selector safe to use here.
	if s, ok := e.session("other"); !ok || s.State != "running" {
		t.Errorf("untagged session state = %q, want it still running", s.State)
	}
}
