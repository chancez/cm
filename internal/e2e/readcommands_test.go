package e2e

import (
	"strings"
	"testing"
)

// startOSC133Session creates a session whose shell reports OSC 133 and runs cmds in it.
//
// A real zsh with the harness's own integration rather than a script printing markers, because the
// thing under test is where cm records boundaries in a real stream: a shell interleaves its prompt, the
// line it echoes, and the markers in an order no fixture would reproduce by accident.
func (e *env) startOSC133Session(name string, cmds ...string) {
	e.t.Helper()

	args := append([]string{"attach", "--no-attach", name}, e.withOSC133()...)
	args = append(args, "--", "/bin/zsh")
	e.mustRun(args...)
	e.waitForPrompt(name)

	for _, c := range cmds {
		if r := e.run("send", name, c, "--enter", "--wait", "idle", "--timeout", "10s"); r.code != 0 {
			e.t.Fatalf("send %q failed: %s", c, r.stderr)
		}
	}
}

// --since-commands N returns exactly the last N command blocks, each opened by its prompt.
//
// The prompt is the point rather than incidental: it is what separates one block from the next, so a
// caller reading three commands can tell where each began. Outputs alone, concatenated, cannot be.
func TestReadSinceCommands(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.startOSC133Session("work",
		"echo alpha-out", "echo beta-out", "echo gamma-out")

	tests := []struct {
		n int
		// want is the set of command outputs that must appear.
		want []string
		// absent is what must not, which is how the bound is verified rather than assumed.
		absent []string
	}{
		{n: 1, want: []string{"gamma-out"}, absent: []string{"alpha-out", "beta-out"}},
		{n: 2, want: []string{"beta-out", "gamma-out"}, absent: []string{"alpha-out"}},
		{n: 3, want: []string{"alpha-out", "beta-out", "gamma-out"}},
	}

	for _, tc := range tests {
		out := e.mustRun("read", "work", "--since-commands", itoa(tc.n))
		for _, w := range tc.want {
			if !strings.Contains(out, w) {
				t.Errorf("--since-commands %d missing %q:\n%s", tc.n, w, out)
			}
		}
		for _, a := range tc.absent {
			if strings.Contains(out, a) {
				t.Errorf("--since-commands %d included %q, want it bounded:\n%s", tc.n, a, out)
			}
		}
		// The echoed command line is present, which is what delimits the blocks.
		if !strings.Contains(out, "echo gamma-out") {
			t.Errorf("--since-commands %d has no echoed command line:\n%s", tc.n, out)
		}
	}
}

// --last-output excludes the prompt and the echoed command line.
//
// The difference from --since-commands 1 is the whole reason both exist: this is what a parser reads, so
// the shell's own text must not be in it.
func TestReadLastOutput(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.startOSC133Session("work", "echo only-this")

	out := e.mustRun("read", "work", "--last-output")
	if !strings.Contains(out, "only-this") {
		t.Errorf("--last-output missing the command's output:\n%s", out)
	}
	// The echoed command line must not appear, which is what distinguishes this from the transcript.
	if strings.Contains(out, "echo only-this") {
		t.Errorf("--last-output included the echoed command line:\n%s", out)
	}

	// And the transcript form does include it, so the two are genuinely different.
	transcript := e.mustRun("read", "work", "--since-commands", "1")
	if !strings.Contains(transcript, "echo only-this") {
		t.Errorf("--since-commands 1 has no echoed command line:\n%s", transcript)
	}
}

// A session whose shell reports no OSC 133 has no boundaries, and must say so.
//
// The failure this prevents is the misleading one: returning empty output looks like a command that
// printed nothing, which sends someone looking at their program instead of their shell configuration.
func TestReadSinceCommandsWithoutShellIntegration(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	// /bin/sh with no integration, which is what a default install gets.
	e.mustRun("run", "--session", "plain", "-d", "--", "/bin/sh", "-c", "echo out; sleep 120")

	r := e.run("read", "plain", "--since-commands", "1")
	if r.code == 0 {
		t.Errorf("--since-commands on a session with no OSC 133 exited 0, want an error: %s", r.stdout)
	}
	// Points at the cause rather than only the symptom, and names the tool that diagnoses it.
	for _, want := range []string{"OSC 133", "doctor"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr = %q, want it to mention %q", r.stderr, want)
		}
	}
	// Reading by lines still works, so the session is not broken, only unbracketed.
	if out := e.mustRun("read", "plain", "--lines", "5"); !strings.Contains(out, "out") {
		t.Errorf("reading by lines failed for the same session:\n%s", out)
	}
}

// Asking for more commands than were seen reports how many there are.
func TestReadSinceCommandsBeyondWhatIsKnown(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.startOSC133Session("work", "echo one")

	r := e.run("read", "work", "--since-commands", "50")
	if r.code == 0 {
		t.Error("--since-commands 50 with one command exited 0, want an error")
	}
	// The count, so a caller learns what it can ask for rather than only that it asked too much.
	if !strings.Contains(r.stderr, "command") {
		t.Errorf("stderr = %q, want it to say how many commands are known", r.stderr)
	}
}

// The flags that bound the same read cannot be combined.
func TestReadRejectsConflictingBounds(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.startOSC133Session("work", "echo one")

	tests := []struct {
		name string
		args []string
	}{
		{
			// Two unrelated bounds on one read, with no sensible precedence.
			name: "lines with since-commands",
			args: []string{"read", "work", "--since-commands", "1", "--lines", "5"},
		},
		{
			name: "lines with last-output",
			args: []string{"read", "work", "--last-output", "--lines", "5"},
		},
		{
			// Both anchor at a command boundary, but different ones.
			name: "both anchors",
			args: []string{"read", "work", "--since-commands", "1", "--last-output"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if r := e.run(tc.args...); r.code == 0 {
				t.Errorf("%v exited 0, want it refused", tc.args)
			}
		})
	}
}

// A session that has ended has no boundaries, since they live in memory with it.
//
// Reported rather than silently falling back to a line count: answering a different question than the
// one asked is how a caller comes to trust output that does not mean what it thinks.
func TestReadSinceCommandsOnAnEndedSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	// Runs to completion, so the session is gone by the time this reads it.
	e.mustRun("run", "--session", "done", "--", "/bin/sh", "-c", "echo finished")

	r := e.run("read", "done", "--since-commands", "1")
	if r.code == 0 {
		t.Errorf("--since-commands on an ended session exited 0, want an error: %s", r.stdout)
	}
	if !strings.Contains(r.stderr, "ended") {
		t.Errorf("stderr = %q, want it to say the session has ended", r.stderr)
	}
	// Reading its saved output by lines still works, which is what `cm run` relies on.
	if out := e.mustRun("read", "done", "--lines", "5"); !strings.Contains(out, "finished") {
		t.Errorf("reading an ended session by lines failed:\n%s", out)
	}
}

// Boundaries stay correct after a server restart, which is where a position could drift.
//
// A restarted server adopts the session and resumes its log partway in, so a tracker starting from zero
// would place every later boundary short by however far the session had already got. The commands run
// before the restart are gone from the tracker, but the ones after it must still be exact.
func TestReadSinceCommandsAfterAServerRestart(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.startOSC133Session("work", "echo before-restart")

	e.restartServer()
	e.waitForPrompt("work")

	// Two commands after the restart, not one, and that is what makes this test discriminating.
	//
	// With only one, a tracker that recorded positions from zero instead of from the resume offset
	// still passes: every position is far below the log's oldest byte, the read clamps to the start of
	// what is retained, and after a restart the retained log happens to begin near the one command
	// there is. Measured while checking this test could fail -- it could not. A second command means
	// the correct answer is a position *inside* the log rather than at its start, so clamping is
	// visibly wrong: it returns both commands where only the last was asked for.
	for _, c := range []string{"echo post-one", "echo post-two"} {
		if r := e.run("send", "work", c,
			"--enter", "--wait", "idle", "--timeout", "10s"); r.code != 0 {
			t.Fatalf("send %q after restart failed: %s", c, r.stderr)
		}
	}

	out := e.mustRun("read", "work", "--since-commands", "1")
	if !strings.Contains(out, "post-two") {
		t.Errorf("--since-commands 1 after a restart missing the last command's output:\n%s", out)
	}
	// The echoed command line too, which is what proves the position landed on the prompt rather than
	// somewhere arbitrary in the stream.
	if !strings.Contains(out, "echo post-two") {
		t.Errorf("--since-commands 1 after a restart has no echoed command line:\n%s", out)
	}
	// The bound is the assertion: a position taken from the wrong origin reads from the start of the
	// retained log instead, which includes everything before it.
	if strings.Contains(out, "post-one") {
		t.Errorf("--since-commands 1 after a restart included the previous command:\n%s", out)
	}
	if strings.Contains(out, "before-restart") {
		t.Errorf("--since-commands 1 reached output from before the restart:\n%s", out)
	}
}
