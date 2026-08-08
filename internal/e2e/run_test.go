package e2e

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// A command's exit status must be this command's exit status.
//
// `cm run -- false` has to fail the way `false` does, or it cannot be used in a script. This failed
// intermittently for a different reason than it looks: the shell finished before the client finished
// attaching, and the resulting "session has ended" surfaced as exit 1 whatever the command returned.
// So a passing run and a failing run were indistinguishable from each other and from a cm failure.
func TestRunPropagatesExitStatus(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	for _, want := range []int{0, 1, 3, 42, 127} {
		r := e.run("run", "--", "/bin/sh", "-c", exitScript(want))
		if r.code != want {
			t.Errorf("cm run -- exit %d exited %d, want %d\nstdout: %s\nstderr: %s",
				want, r.code, want, r.stdout, r.stderr)
		}
	}
}

// The status must be right every time, not usually.
//
// The bug: the command finished before the client finished attaching, so the server reported "session
// has ended" and the real status was lost. It hit roughly one run in twenty-five on macOS and one in
// ten on Linux in ordinary use.
//
// How often the race is lost depends on how fast the command is, and the difference is large enough to
// decide whether a test catches anything at all. Measured against a deliberately broken build,
// `/usr/bin/true` lost 36 of 40 while `sh -c 'echo RAN; exit 7'` lost only 2 of 40, because the echo
// gives the attach time to complete. Hence the fastest command available and no shell.
//
// Both entry points are covered, since they leave the stream differently: the waiting form reads the
// status back afterwards, while --detach sends a Detach immediately and never waits.
//
// Only the detached form reliably reproduces it now, and the reason is worth recording: `cm run`
// captures output, so the shim writes a log file, and that extra work is enough that the shell usually
// no longer beats the attach. The waiting subtest is kept because it asserts the behavior users depend
// on, but the detached one is what fails when the fix is removed.
func TestRunExitStatusIsNotRacy(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	const runs = 20
	t.Run("waiting", func(t *testing.T) {
		bad, firstErr := 0, ""
		for range runs {
			r := e.run("run", "--", falseBinary())
			if r.code != 1 {
				bad++
				if firstErr == "" {
					firstErr = strings.TrimSpace(r.stderr)
				}
			}
		}
		if bad != 0 {
			t.Errorf("%d of %d runs reported the wrong exit status; first failure: %s",
				bad, runs, firstErr)
		}
	})

	t.Run("detached", func(t *testing.T) {
		bad, firstErr := 0, ""
		for i := range runs {
			// A name per run, since --detach prints the name rather than waiting and reusing one
			// would collide with the previous session's record.
			r := e.run("run", "-d", "--session", "d"+itoa(i), "--", trueBinary())
			if r.code != 0 {
				bad++
				if firstErr == "" {
					firstErr = strings.TrimSpace(r.stderr)
				}
			}
		}
		if bad != 0 {
			t.Errorf("%d of %d detached runs failed, want them to start cleanly; first failure: %s",
				bad, runs, firstErr)
		}
	})
}

// falseBinary returns a path to a program that exits 1 and prints nothing.
//
// An absolute path rather than a shell builtin, since the point is to exit as fast as possible: a
// shell that has to start first hides the race this test exists to catch.
func falseBinary() string { return firstExisting("/usr/bin/false", "/bin/false") }

// trueBinary returns a path to a program that exits 0 and prints nothing.
func trueBinary() string { return firstExisting("/usr/bin/true", "/bin/true") }

// firstExisting returns the first path that exists, falling back to a shell.
//
// The fallback is slower, so the window is harder to hit, but running a weaker version of the test
// beats not running it on a platform that puts these somewhere unexpected.
func firstExisting(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/bin/sh"
}

// A command's output must be readable after it finishes.
//
// The session is gone by then, so this exercises reading history from a session that has ended rather
// than from a live terminal model.
func TestRunCapturesOutput(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	requireTerminal(t, e)

	name := strings.TrimSpace(e.mustRun("run", "--session", "captured", "-d", "--",
		"/bin/sh", "-c", "echo CAPTURED_OUTPUT"))
	if name != "captured" {
		t.Errorf("run -d printed %q, want the session name", name)
	}
	if got := e.waitForHistory("captured", "CAPTURED_OUTPUT"); !strings.Contains(got, "CAPTURED_OUTPUT") {
		t.Errorf("history = %q, want the command's output", got)
	}
}

// exitScript returns a shell command that exits with the given status.
func exitScript(code int) string {
	// echo first, so the session has output as well as a status: a command that printed nothing would
	// not distinguish "status reported correctly" from "session never ran".
	return "echo RAN; exit " + itoa(code)
}

// itoa avoids importing strconv for one call in a test helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// `cm run` prints the command's output, with escape sequences stripped.
//
// It used to print nothing at all and leave the caller to run `cm read` afterwards, which surprised the first
// person to use it: a command that runs and shows nothing looks like it did nothing. Rendered rather than raw,
// matching cm read, so a redirected build log is text rather than a file full of colour codes.
func TestRunPrintsStrippedOutput(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := e.mustRun("run", "--", "/bin/sh", "-c",
		`printf 'plain\n'; printf '\033[31mred\033[0m\n'`)

	if !strings.Contains(out, "plain") || !strings.Contains(out, "red") {
		t.Errorf("output is missing the command's text:\n%q", out)
	}
	// The escape byte itself, which is what makes this rendered rather than raw. A redirected log containing
	// these is the thing being avoided.
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("output contains escape sequences, want them stripped:\n%q", out)
	}
}

// A failing command prints its output before its status.
//
// The ordering is the point. Returning the status first means the output explaining the failure is never
// printed, which is exactly backwards for the case where output matters most.
func TestRunPrintsOutputWhenTheCommandFails(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	r := e.run("run", "--", "/bin/sh", "-c", "echo the reason it failed; exit 3")

	if r.code != 3 {
		t.Errorf("exit code = %d, want 3", r.code)
	}
	if !strings.Contains(r.stdout, "the reason it failed") {
		t.Errorf("a failing command printed no output:\nstdout: %q\nstderr: %q", r.stdout, r.stderr)
	}
}

// --quiet prints nothing and still reports the status.
//
// For a caller that only wants the exit code, and for anything where the output would be noise.
func TestRunQuietPrintsNothing(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := e.mustRun("run", "--quiet", "--", "/bin/sh", "-c", "echo hidden")
	if strings.Contains(out, "hidden") {
		t.Errorf("--quiet printed the command's output:\n%q", out)
	}

	// And the status still propagates, which is the whole reason to use --quiet rather than redirecting.
	if r := e.run("run", "--quiet", "--", "/bin/sh", "-c", "exit 4"); r.code != 4 {
		t.Errorf("exit code = %d with --quiet, want 4", r.code)
	}
}

// --detach prints the session name rather than output, since there is none yet.
//
// The two modes are different by nature: detaching returns before the command has run, so there is nothing to
// print but the name to look it up by.
func TestRunDetachedStillPrintsTheSessionName(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := strings.TrimSpace(e.mustRun("run", "-d", "--session", "named", "--",
		"/bin/sh", "-c", "echo later"))
	if out != "named" {
		t.Errorf("detached run printed %q, want the session name", out)
	}
}

// JSON mode does not mix output into the document.
//
// A caller that asked for JSON is parsing it, and free-form command output in the middle would break that.
func TestRunJSONDoesNotIncludeOutput(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := e.mustRun("run", "--json", "--", "/bin/sh", "-c", "echo SHOULD_NOT_APPEAR")
	if strings.Contains(out, "SHOULD_NOT_APPEAR") {
		t.Errorf("JSON output includes the command's output:\n%s", out)
	}
	// And it is still valid JSON, which is what the mode promises.
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Errorf("output is not valid JSON: %v\n%s", err, out)
	}
}
