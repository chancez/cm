package e2e

import (
	"strings"
	"testing"
	"time"
)

// sendRepeatedly prints a line in a session until the test ends.
//
// Stopped through t.Cleanup rather than left to finish, because a goroutine still calling e.run after the
// test returns uses a torn-down sandbox and panics inside the harness -- which reports as a failure in
// whatever test happens to be running, not in the one that leaked it. That cost a diagnosis pass here.
func (e *env) sendRepeatedly(t *testing.T, session, line string) {
	t.Helper()
	done := make(chan struct{})
	stopped := make(chan struct{})
	t.Cleanup(func() {
		close(done)
		<-stopped
	})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-done:
				return
			default:
			}
			e.run("send", session, line, "--enter")
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
}

// A match wait returns when the text appears, on a session no state wait could serve.
//
// /bin/sh reports no OSC 133 and nothing calls `cm report`, so idle and busy are never reported and every
// other form of wait is useless here. That is the case this exists for, and it is the common one: most
// programs people put in a session report nothing.
func TestWaitMatchOnASessionThatReportsNothing(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "w", "--", "/bin/sh")

	// Printed after the wait is issued, so the wait genuinely blocks rather than finding it already there.
	go func() {
		time.Sleep(1500 * time.Millisecond)
		e.run("send", "w", "echo BUILD-OK", "--enter")
	}()

	start := time.Now()
	r := e.runWithin(30*time.Second, "wait", "w", "--match", "BUILD-OK", "--timeout", "20s")
	elapsed := time.Since(start)

	if r.code != 0 {
		t.Fatalf("wait --match exited %d, want 0: %s", r.code, r.stderr)
	}
	// Returned when the text appeared rather than at the deadline, which is what distinguishes waiting
	// from sleeping.
	if elapsed > 10*time.Second {
		t.Errorf("wait --match took %v, want it to return when the text appeared", elapsed)
	}

	// The control that makes the above meaningful, and it is not "a state wait fails here". Without OSC
	// 133 cm sees no command running, so `--until idle` is satisfied *immediately* and truthfully reports
	// a session it knows nothing about as idle. That is the documented pitfall rather than a working wait:
	// it answers before the work starts, so a caller using it reads the previous turn's output.
	//
	// Measured here rather than asserted from the docs, since the whole claim for --match rests on it.
	start = time.Now()
	if r := e.runWithin(30*time.Second, "wait", "w", "--until", "idle", "--timeout", "5s"); r.code != 0 {
		t.Errorf("wait --until idle exited %d on a session with no OSC 133, want it satisfied at once",
			r.code)
	}
	// An upper bound rather than a timeout, so scaleTimeout does not reach it. Scaled here for the same
	// reason: a race-instrumented cm measured 1.054s against this 1s bound while behaving correctly. The
	// claim is that idle is satisfied at once rather than waiting out the 5s timeout, and that stays a
	// real assertion with the slack.
	if idle := time.Since(start); idle > scaleTimeout(time.Second) {
		t.Errorf("wait --until idle took %v, want it immediate: this test rests on idle being "+
			"trivially true without OSC 133", idle)
	}
}

// A match that never appears times out and reports failure.
func TestWaitMatchTimesOut(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "w", "--", "/bin/sh")

	start := time.Now()
	r := e.runWithin(30*time.Second, "wait", "w", "--match", "NEVER-APPEARS", "--timeout", "2s")
	elapsed := time.Since(start)

	// Non-zero, matching how a state wait reports not-yet: the caller asked a question and the answer is
	// no.
	if r.code == 0 {
		t.Error("wait --match exited 0 for text that never appeared, want non-zero")
	}
	if elapsed > 15*time.Second {
		t.Errorf("wait --match took %v, want it bounded by the timeout", elapsed)
	}
}

// Escape sequences between the characters do not defeat a match.
//
// The reason rendering is the default. A program that colours its output writes "DO\x1b[0mNE", so a
// byte-wise match finds nothing while a person looking at the screen plainly sees DONE.
func TestWaitMatchIgnoresColour(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "w", "--", "/bin/sh")

	go func() {
		time.Sleep(time.Second)
		// printf rather than echo, so the escape reaches the pty rather than being written literally.
		e.run("send", "w", `printf "\033[32mDO\033[0mNE\n"`, "--enter")
	}()

	if r := e.runWithin(30*time.Second, "wait", "w", "--match", "DONE", "--timeout", "15s"); r.code != 0 {
		t.Errorf("wait --match exited %d, want it to match across a colour escape: %s", r.code, r.stderr)
	}
}

// --match-raw matches the emitted bytes, so a pattern rendering would have joined does not match.
//
// Asserted as the difference between the two modes rather than only that raw works, since "raw matched
// something" alone would not show that the modifier changes anything.
func TestWaitMatchRawSeesTheBytes(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "w", "--", "/bin/sh")

	// Repeated, so both waits below have something to see, and stopped with the test.
	e.sendRepeatedly(t, "w", `printf "\033[32mDO\033[0mNE\n"`)

	// Raw sees the escape sequence itself.
	if r := e.runWithin(30*time.Second,
		"wait", "w", "--match", "[32m", "--match-raw", "--timeout", "10s"); r.code != 0 {
		t.Errorf("wait --match-raw exited %d, want it to find an escape sequence: %s", r.code, r.stderr)
	}
	// And does not see text that only exists once the sequences are removed.
	if r := e.runWithin(30*time.Second,
		"wait", "w", "--match", "DONE", "--match-raw", "--timeout", "3s"); r.code == 0 {
		t.Error("wait --match-raw matched DONE across an escape sequence, want it to see the bytes")
	}
}

// Only output arriving after the call counts.
//
// The same rule that keeps a wait for idle from being satisfied by the idle a session started in. Without
// it, a wait would resolve on the previous turn's output and hand the caller a stale answer.
func TestWaitMatchIgnoresEarlierOutput(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "w", "--", "/bin/sh")

	e.mustRun("send", "w", "echo ALREADY-PRINTED", "--enter")
	// Confirmed present before waiting, so the test is about the wait's rule rather than about timing.
	e.waitForOutputInSession("w", "ALREADY-PRINTED", 10*time.Second)

	r := e.runWithin(30*time.Second, "wait", "w", "--match", "ALREADY-PRINTED", "--timeout", "2s")
	if r.code == 0 {
		t.Error("wait --match was satisfied by output printed before the call, want only new output to count")
	}
}

// A match spanning several chunks is found.
//
// A pty read is bounded by the kernel buffer, so a pattern printed as separate writes arrives split. The
// matcher keeps a tail for this; without it the wait burns its whole timeout on output that already
// contained what was asked for.
func TestWaitMatchAcrossChunks(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "w", "--", "/bin/sh")

	go func() {
		time.Sleep(time.Second)
		// Separate printf calls with no newline, so the pattern is split across writes rather than
		// arriving whole. A sleep between them makes separate pty reads far likelier.
		e.run("send", "w", `printf "SPLIT-"; sleep 0.3; printf "PATTERN\n"`, "--enter")
	}()

	if r := e.runWithin(30*time.Second,
		"wait", "w", "--match", "SPLIT-PATTERN", "--timeout", "15s"); r.code != 0 {
		t.Errorf("wait --match exited %d, want it to match across chunks: %s", r.code, r.stderr)
	}
}

// --match composes with --tag, waiting on a group.
func TestWaitMatchWithTag(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	for _, n := range []string{"ga", "gb"} {
		e.mustRun("attach", "--no-attach", n, "--tag", "grp=m", "--", "/bin/sh")
	}

	go func() {
		time.Sleep(time.Second)
		for _, n := range []string{"ga", "gb"} {
			e.run("send", n, "echo GROUP-DONE", "--enter")
		}
	}()

	if r := e.runWithin(30*time.Second,
		"wait", "--tag", "grp=m", "--match", "GROUP-DONE", "--timeout", "15s"); r.code != 0 {
		t.Errorf("wait --tag --match exited %d, want 0: %s", r.code, r.stderr)
	}
}

// Combinations with no single sensible meaning are refused.
func TestWaitMatchRejectsConflictingFlags(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "w", "--", "/bin/sh")

	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			// "idle and also matching" and "idle or matching" are both plausible.
			name: "match with until",
			args: []string{"wait", "w", "--match", "X", "--until", "idle"},
			want: "--until",
		},
		{
			// Nothing is being matched, so the modifier cannot mean anything.
			name: "match-raw alone",
			args: []string{"wait", "w", "--match-raw"},
			want: "--match",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := e.run(tc.args...)
			if r.code == 0 {
				t.Errorf("%v exited 0, want it refused", tc.args)
			}
			if !strings.Contains(r.stderr, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", r.stderr, tc.want)
			}
		})
	}
}

// A match against an ended session is refused rather than waiting out the timeout.
//
// It will produce no further output, so the wait could never be satisfied. Saying so points at the command
// that can answer from what was saved.
func TestWaitMatchOnAnEndedSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("run", "--session", "done", "--", "/bin/sh", "-c", "echo finished")

	r := e.runWithin(30*time.Second, "wait", "done", "--match", "finished", "--timeout", "10s")
	if r.code == 0 {
		t.Error("wait --match on an ended session exited 0, want it refused")
	}
	if !strings.Contains(r.stderr, "ended") {
		t.Errorf("stderr = %q, want it to say the session has ended", r.stderr)
	}
}

// `cm send --match` does not match the shell's echo of the command it just sent.
//
// Measured before it was fixed: writing to a pty makes the shell echo the line back, and the echo contains
// the command, so a pattern naming anything in the command resolved against the echo. `send 'sh -c "sleep
// 2; echo UNIQUEWORD"' --match UNIQUEWORD` returned in 11ms while the real output arrived 2s later. The same
// class of wrong answer as a wait for idle satisfied by the idle a session was already in, and it hands the
// caller a result before the work has happened.
//
// The timing is the assertion. A pattern present in both the echo and the output cannot be distinguished any
// other way: matching the echo and matching the output look identical apart from when they happen.
func TestSendMatchSkipsTheEchoedCommand(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "w", "--", "/bin/sh")

	start := time.Now()
	r := e.runWithin(40*time.Second,
		"send", "w", `sh -c "sleep 2; echo UNIQUEWORD"`, "--enter",
		"--match", "UNIQUEWORD", "--timeout", "25s")
	elapsed := time.Since(start)

	if r.code != 0 {
		t.Fatalf("send --match exited %d, want 0: %s", r.code, r.stderr)
	}
	// The output arrives two seconds in, so anything much faster matched the echo instead.
	if elapsed < 1500*time.Millisecond {
		t.Errorf("send --match returned in %v, want it to wait for the output at ~2s: "+
			"it matched the echoed command line", elapsed)
	}
	// And it did not simply wait out the clock.
	if elapsed > 15*time.Second {
		t.Errorf("send --match took %v, want it to return when the output appeared", elapsed)
	}
}

// A command that prints and finishes immediately is still caught.
//
// The window a composed `send` then `wait --match` cannot close from outside: the output is already past by
// the time a second call subscribes. Send arms the subscription before writing, so this is the case that
// justifies the flag living on the send request rather than only on Wait.
func TestSendMatchCatchesAnInstantCommand(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "w", "--", "/bin/sh")

	r := e.runWithin(30*time.Second,
		"send", "w", "echo INSTANT-DONE", "--enter", "--match", "INSTANT-DONE", "--timeout", "15s")
	if r.code != 0 {
		t.Errorf("send --match exited %d on an instant command, want it caught: %s", r.code, r.stderr)
	}
}

// `cm run --session --match` bounds a reused session whose shell reports nothing.
//
// The gap this closes, which the cm skill documented as needing a timeout: reusing a session waits for the
// shell to be idle, which needs OSC 133, so `/bin/sh` could only be bounded by a clock. Measured before and
// after -- the state form took its whole 3s timeout, the match form returned in 14ms.
func TestRunMatchOnAReusedSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "build", "--", "/bin/sh")

	start := time.Now()
	r := e.runWithin(40*time.Second,
		"run", "--session", "build", "--match", "COMPILED", "--timeout", "20s",
		"--", `sh -c "sleep 1; echo COMPILED"`)
	elapsed := time.Since(start)

	if r.code != 0 {
		t.Fatalf("run --match exited %d, want 0: %s", r.code, r.stderr)
	}
	// Returned when the text appeared rather than at the deadline.
	if elapsed > 12*time.Second {
		t.Errorf("run --match took %v, want it to return when the text appeared", elapsed)
	}
	// The command's output is printed, which is what run is for.
	if !strings.Contains(r.stdout, "COMPILED") {
		t.Errorf("stdout = %q, want the command's output", r.stdout)
	}
}

// A match that never appears on a reused session times out and says so.
func TestRunMatchTimesOut(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "build", "--", "/bin/sh")

	r := e.runWithin(30*time.Second,
		"run", "--session", "build", "--match", "NEVER-APPEARS", "--timeout", "2s",
		"--", "echo something-else")
	if r.code == 0 {
		t.Error("run --match exited 0 for text that never appeared, want non-zero")
	}
	// The output is still printed, since it is usually how a caller works out why the match failed.
	if !strings.Contains(r.stdout, "something-else") {
		t.Errorf("stdout = %q, want the output printed even on a timeout", r.stdout)
	}
}

// Combinations with no single meaning are refused on send and run too.
func TestSendAndRunMatchRejectConflicts(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "w", "--", "/bin/sh")

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "send match with wait", args: []string{"send", "w", "x", "--match", "A", "--wait", "idle"}},
		// --follow stops when its wait resolves, and a match resolving mid-command would cut the stream
		// off partway through output the caller was watching.
		{name: "send match with follow", args: []string{"send", "w", "x", "--match", "A", "--follow"}},
		{name: "send match-raw alone", args: []string{"send", "w", "x", "--match-raw"}},
		{name: "run match-raw alone", args: []string{"run", "--match-raw", "--", "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r := e.run(tc.args...); r.code == 0 {
				t.Errorf("%v exited 0, want it refused", tc.args)
			}
		})
	}
}
