package e2e

import (
	"strings"
	"testing"
	"time"
)

// `cm read --follow` prints the recent lines and then keeps streaming.
//
// The two halves differ in kind, deliberately: the tail is a rendered screen with wrapping rejoined, and what
// follows is raw output. Re-rendering on every byte would repaint rather than append, which is wrong for
// something being piped to a file.
func TestReadFollowPrintsTailThenStreams(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// Output before the follow starts, then more after, so both halves are exercised.
	e.mustRun("run", "--session", "rf", "-d", "--",
		"/bin/sh", "-c", "echo BEFORE; sleep 1.5; echo AFTER; sleep 0.3; echo LAST")
	e.waitForOutputInSession("rf", "BEFORE", 15*time.Second)

	out := e.mustRunWithin(30*time.Second, "read", "--follow", "rf")

	// The tail carries what was already printed, and the stream carries what came next.
	for _, want := range []string{"BEFORE", "AFTER", "LAST"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// Exactly once each. The screen repaint that normally opens an attachment would duplicate the tail, which
	// is what this produced before Open gained no_restore: BEFORE appeared twice, once rendered and once
	// inside the restored screen.
	if n := strings.Count(out, "BEFORE"); n != 1 {
		t.Errorf("BEFORE appears %d times, want 1: the screen repaint is duplicating the tail\n%s", n, out)
	}
}

// `cm read --follow` returns when the session ends rather than hanging.
//
// A follower has to stop on its own, or every use needs a timeout. The session ending is the signal.
func TestReadFollowReturnsWhenTheSessionEnds(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "short", "-d", "--", "/bin/sh", "-c", "echo done; sleep 0.5")

	// A generous bound that still fails rather than hanging the suite: the session lives well under a second.
	out := e.mustRunWithin(20*time.Second, "read", "--follow", "short")
	if !strings.Contains(out, "done") {
		t.Errorf("output is missing the session's output:\n%s", out)
	}
}

// `cm send --follow` streams a command's output and returns when it finishes.
//
// Needs OSC 133, since --follow implies waiting for idle and idle is derived from those markers.
func TestSendFollowStreamsUntilTheCommandFinishes(t *testing.T) {
	skipIfShort(t)
	requireShell(t, "/bin/zsh")
	e := newEnv(t)

	args := append([]string{"run", "--session", "sf", "-d"}, e.withOSC133()...)
	args = append(args, "--", "/bin/zsh", "-i")
	e.mustRun(args...)
	e.waitFor("the shell to reach its first prompt", 20*time.Second, func() bool {
		s, ok := e.session("sf")
		return ok && s.State == "running"
	})

	start := time.Now()
	out := e.mustRunWithin(30*time.Second, "send", "sf",
		"for i in 1 2 3; do echo streamed $i; sleep 0.3; done", "--enter", "--follow")
	elapsed := time.Since(start)

	for i := 1; i <= 3; i++ {
		want := "streamed " + itoa(i)
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	// It waited for the command rather than returning immediately, which is what distinguishes this from a
	// plain send. The loop sleeps 0.9s in total.
	if elapsed < 700*time.Millisecond {
		t.Errorf("returned after %s, want it to wait for the command to finish", elapsed)
	}
}

// `cm send --follow` sends the input only after the stream is live.
//
// The ordering is the design, and getting it wrong loses output: a command can start and finish faster than a
// second connection can be made, so attaching afterwards misses whatever it printed in between.
//
// Tested with a command that prints immediately, which is the case that loses output if the send goes first.
//
// A caveat worth stating: on a local socket the whole operation takes about 24ms and the attach completes in
// well under a millisecond, so removing the wait does not fail this test on its own -- the send simply cannot
// win the race by chance. It does fail with the attach deliberately delayed by 400ms, which is how the ordering
// was confirmed to be what makes this work. So this test proves the behavior, not the mechanism; the mechanism
// is argued in sendAndFollow's comment and was verified by hand.
func TestSendFollowDoesNotMissEarlyOutput(t *testing.T) {
	skipIfShort(t)
	requireShell(t, "/bin/zsh")
	e := newEnv(t)

	args := append([]string{"run", "--session", "early", "-d"}, e.withOSC133()...)
	args = append(args, "--", "/bin/zsh", "-i")
	e.mustRun(args...)
	e.waitFor("the shell to reach its first prompt", 20*time.Second, func() bool {
		s, ok := e.session("early")
		return ok && s.State == "running"
	})

	// No sleep before the output, so it lands as soon as the shell runs it.
	out := e.mustRunWithin(30*time.Second, "send", "early", "echo IMMEDIATE", "--enter", "--follow")
	if !strings.Contains(out, "IMMEDIATE") {
		t.Errorf("output printed at the very start was missed, which means the send raced the stream:\n%s",
			out)
	}
}

// `cm send --follow` warns when the session reports no OSC 133.
//
// --follow implies waiting for idle, and a session whose shell never reports is permanently idle as far as cm
// can tell, so the wait never resolves. That is existing --wait behavior, but --follow turns a documented
// pitfall into a command that prints nothing and never returns. The warning replaces an unexplained hang with
// an explanation.
func TestSendFollowWarnsWithoutShellIntegration(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// /bin/sh with no prompt hooks reports nothing.
	e.mustRun("run", "--session", "quiet", "-d", "--", "/bin/sh", "-c", "sleep 30")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("quiet")
		return ok && s.State == "running"
	})

	// A timeout, or this would wait forever -- which is the very thing being warned about.
	r := e.runWithin(20*time.Second, "send", "quiet", "echo hi", "--enter", "--follow", "--timeout", "2s")
	if !strings.Contains(r.stderr, "OSC 133") {
		t.Errorf("stderr does not warn about missing shell integration:\n%s", r.stderr)
	}
}

// The warning is not printed for a session that does report.
//
// A warning on every run is one nobody reads, and this command is meant for watching ordinary builds.
func TestSendFollowQuietWithShellIntegration(t *testing.T) {
	skipIfShort(t)
	requireShell(t, "/bin/zsh")
	e := newEnv(t)

	args := append([]string{"run", "--session", "loud", "-d"}, e.withOSC133()...)
	args = append(args, "--", "/bin/zsh", "-i")
	e.mustRun(args...)
	e.waitFor("the shell to reach its first prompt", 20*time.Second, func() bool {
		s, ok := e.session("loud")
		return ok && s.State == "running"
	})

	// One command first, so the session has reported at least once by the time the followed send runs.
	//
	// Polled on the server's own view rather than on busy or the command name: both are cleared when a command
	// finishes, so a fast one leaves nothing for a client to see. That is exactly the mistake this test caught
	// in the warning itself, which is why the flag is computed server-side from a monotonic count.
	e.waitFor("the shell to have reported a command", 20*time.Second, func() bool {
		return !strings.Contains(
			e.runWithin(20*time.Second, "send", "loud", "true", "--enter", "--follow").stderr,
			"OSC 133")
	})

	r := e.runWithin(30*time.Second, "send", "loud", "echo ok", "--enter", "--follow")
	if strings.Contains(r.stderr, "OSC 133") {
		t.Errorf("warned about shell integration for a session that reports:\n%s", r.stderr)
	}
}

// Followed output has escape sequences stripped by default, and keeps them with --raw.
//
// The default matters more than it sounds: --follow mostly replaces a send followed by a read where the caller
// had to guess how much to read, and a colour code in a redirected build log is noise. cm read already strips,
// so the streaming form matching it is consistency rather than a new opinion.
func TestFollowStripsEscapesByDefault(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "esc", "-d", "--",
		"/bin/sh", "-c", `printf '\033[32mgreen\033[0m\n'; sleep 0.4`)

	out := e.mustRunWithin(20*time.Second, "read", "--follow", "esc")
	if !strings.Contains(out, "green") {
		t.Errorf("output is missing the text:\n%q", out)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("escape sequences survived the default strip:\n%q", out)
	}
}

// --raw keeps the escape sequences in the streamed half.
//
// For the cases where the sequences are the interesting part, such as checking what a program actually emitted.
//
// The escapes have to be printed after the follow starts, which took a correction: an earlier version printed
// them first, so they only ever arrived through the rendered tail, which is stripped either way. The test then
// failed while --raw was working, and the flag looked broken when the test was.
func TestFollowRawKeepsEscapes(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// A delay before the escapes, so they reach the stream rather than the tail.
	e.mustRun("run", "--session", "rawesc", "-d", "--",
		"/bin/sh", "-c", `sleep 1; printf '\033[32mgreen\033[0m\n'; sleep 0.3`)

	out := e.mustRunWithin(20*time.Second, "read", "--follow", "--raw", "rawesc")
	if !strings.ContainsRune(out, 0x1b) {
		t.Errorf("--raw stripped the escape sequences from the stream:\n%q", out)
	}
}

// The default strips escapes from the streamed half too, not only from the tail.
//
// The complement of the case above, and the one that would hide a stripper installed on the wrong writer: the
// tail is rendered by the server and so is clean regardless, which means a test that only checks the tail
// passes with no filter at all.
func TestFollowStripsEscapesFromTheStream(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "strmesc", "-d", "--",
		"/bin/sh", "-c", `sleep 1; printf '\033[32mgreen\033[0m\n'; sleep 0.3`)

	out := e.mustRunWithin(20*time.Second, "read", "--follow", "strmesc")
	if !strings.Contains(out, "green") {
		t.Errorf("output is missing the streamed text:\n%q", out)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("escape sequences survived in the streamed half:\n%q", out)
	}
}
