package e2e

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// `cm run --session NAME` on an existing session sends the command to it rather than dropping it.
//
// This was a real bug and a bad one: the server returned the existing session and silently discarded the
// command, so the call exited 0 having run nothing and printed the *previous* command's output. It looked like
// it worked.
//
// Reuse is also the point of naming a session. The first call creates it, later calls reuse the shell already
// there, which keeps a directory or an activated environment between runs and costs one pty rather than one per
// command.
func TestRunReusesAnExistingSession(t *testing.T) {
	skipIfShort(t)
	requireShell(t, "/bin/zsh")
	e := newEnv(t)

	// A shell session that reports OSC 133, since reuse waits for idle.
	args := append([]string{"run", "--session", "reused", "-d"}, e.withOSC133()...)
	args = append(args, "--", "/bin/zsh", "-i")
	e.mustRun(args...)
	// The prompt, not just the session being "running": zsh has to finish starting its line editor before
	// it will read what is sent, and input that arrives first is mangled rather than lost.
	e.waitForPrompt("reused")

	out := e.mustRunWithin(30*time.Second, "run", "--session", "reused", "--", "echo REUSED_RAN")
	if !strings.Contains(out, "REUSED_RAN") {
		t.Errorf("the command was not run in the existing session:\n%q", out)
	}

	// One session, not two: reuse must not create a second one alongside.
	if s, ok := e.session("reused"); !ok {
		t.Error("the session disappeared")
	} else if s.State != "running" {
		t.Errorf("state = %q after reuse, want running", s.State)
	}
}

// State persists across reuse, which is why reusing a session is worth doing.
//
// A directory changed in one call is still current in the next, because it is the same shell. That is the
// difference from creating a session per command, and the thing a caller naming a session is usually after.
func TestRunReuseKeepsShellState(t *testing.T) {
	skipIfShort(t)
	requireShell(t, "/bin/zsh")
	e := newEnv(t)

	args := append([]string{"run", "--session", "stateful", "-d"}, e.withOSC133()...)
	args = append(args, "--", "/bin/zsh", "-i")
	e.mustRun(args...)
	// The prompt, not just the session being "running": zsh has to finish starting its line editor before
	// it will read what is sent, and input that arrives first is mangled rather than lost.
	e.waitForPrompt("stateful")

	// A variable set in one call, read in the next. A directory would work too; a variable is unambiguous,
	// since nothing else could set it.
	e.mustRunWithin(30*time.Second, "run", "--session", "stateful", "--", "MARKER=carried_over")
	out := e.mustRunWithin(30*time.Second, "run", "--session", "stateful", "--", "echo $MARKER")

	if !strings.Contains(out, "carried_over") {
		t.Errorf("shell state did not persist across reuse:\n%q", out)
	}
}

// A fresh name still creates the session and runs the command as its shell.
//
// The other half, so the reuse path cannot have broken the ordinary case. Creating passes the arguments as an
// argv; only reuse sends them to a shell.
func TestRunStillCreatesWhenTheNameIsNew(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := e.mustRun("run", "--session", "brandnew", "--", "/bin/sh", "-c", "echo CREATED_RAN")
	if !strings.Contains(out, "CREATED_RAN") {
		t.Errorf("a new session did not run its command:\n%q", out)
	}
}

// `cm attach --no-attach` creates a session, prints its name, and does not attach.
//
// For pre-creating a session something else will attach to. Distinct from `cm run -d`, which needs a command and
// captures its output for a few minutes; this makes an ordinary shell session.
func TestAttachNoAttachCreatesWithoutAttaching(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := strings.TrimSpace(e.mustRunWithin(20*time.Second, "attach", "--no-attach", "prepared"))
	if out != "prepared" {
		t.Errorf("printed %q, want the session name", out)
	}

	s, ok := e.session("prepared")
	if !ok {
		t.Fatal("the session was not created")
	}
	if s.State != "running" {
		t.Errorf("state = %q, want running", s.State)
	}
	// Nothing attached, which is the whole point: the command returned rather than staying connected.
	if s.Clients != 0 {
		t.Errorf("clients = %d, want 0: --no-attach must not stay attached", s.Clients)
	}
}

// With no name, --no-attach prints the name the server allocated.
//
// The caller needs it to attach later, and this is the only place it is reported.
func TestAttachNoAttachPrintsAnAllocatedName(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := strings.TrimSpace(e.mustRunWithin(20*time.Second, "attach", "--no-attach"))
	if out == "" {
		t.Fatal("--no-attach with no name printed nothing")
	}
	if _, ok := e.session(out); !ok {
		t.Errorf("the printed name %q does not name a session", out)
	}
}

// A session created with --no-attach can be attached to afterwards.
//
// The reason to pre-create one, so it is asserted rather than assumed: a session that cannot then be attached to
// would make the flag useless.
func TestAttachNoAttachThenAttach(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRunWithin(20*time.Second, "attach", "--no-attach", "later")

	c := attachOnPty(t, e, "later")
	c.waitReady()
	e.waitFor("the client to attach to the pre-created session", 15*time.Second, func() bool {
		s, ok := e.session("later")
		return ok && s.Clients == 1
	})
}

// A reused session's command output must arrive in full, not be truncated when the wait returns.
//
// The bug this pins: `cm run --session NAME` on an existing session printed only the shell's echo of the
// input -- "eecho $MARKER" and nothing more -- about a third of the time on a session's first reuse.
//
// It looked like mangled input, and the doubled first character made a shell timing problem the obvious
// suspect. It was not. The session's own output was always correct; what was lost was the tail of the
// *stream*. `cm send --follow` cancels its follower as soon as the send-and-wait returns, and the wait
// returning means the command finished, not that its output has crossed the socket. Whatever had not been
// read yet was discarded.
//
// The fix drains to the position the wait reports before cancelling, so this asserts on the whole line
// rather than the exit status: the status was always right, which is why nothing caught it.
//
// Repeated because the truncation is a race. A single pass passed against the broken code roughly two times
// in three.
func TestRunReuseDoesNotTruncateOutput(t *testing.T) {
	skipIfShort(t)
	requireShell(t, "/bin/zsh")
	e := newEnv(t)

	// A fresh session per iteration, because the truncation happens on a session's *first* reuse. Reusing
	// one that has already run something is reliably fine, which is why a loop against a single warm session
	// passes against the broken code and asserts nothing.
	osc := e.withOSC133()
	for i := range 5 {
		name := "notrunc" + strconv.Itoa(i)
		args := append([]string{"run", "--session", name, "-d"}, osc...)
		args = append(args, "--", "/bin/zsh", "-i")
		e.mustRun(args...)
		e.waitForPrompt(name)

		want := "TRUNC_TAG_" + strconv.Itoa(i)
		out := e.mustRunWithin(30*time.Second, "run", "--session", name, "--", "echo "+want)
		if !strings.Contains(out, want) {
			t.Fatalf("run %d printed no output for `echo %s`, so the stream was cut before it arrived:\n%q",
				i, want, out)
		}
	}
}
