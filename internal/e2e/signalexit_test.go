package e2e

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRunReportsASignalDeathLikeAShell is a differential against /bin/sh.
//
// `cm run` documents itself as usable in a script the way a local command is, so the shell is the only
// correct reference and the comparison is run rather than hardcoded: this asserts cm agrees with whatever
// this machine's sh does, instead of asserting a number someone believed.
//
// What it caught: Go's ExitCode returns -1 for a process "terminated by a signal", the same value it
// returns for one that has not exited, and the server reads a negative code as "no status available, the
// session is lost". So every signal death was recorded as a lost session with exit code 0. `cm ls` reported
// success for a session that had been killed, and `cm run` gave exit 1 with "session ended unexpectedly"
// for TERM, KILL and INT alike, where sh gives 143, 137 and 130.
func TestRunReportsASignalDeathLikeAShell(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	for _, sig := range []string{"TERM", "KILL", "INT", "HUP"} {
		t.Run(sig, func(t *testing.T) {
			script := "kill -" + sig + " $$"

			// The reference, measured now rather than written down. A hardcoded 143 would be a claim about
			// this platform that the test could not defend.
			want := shellStatus(t, script)
			if want == 0 {
				t.Fatalf("/bin/sh exited 0 for %q, so it did not die from the signal and there is "+
					"nothing to compare against", script)
			}

			r := e.runWithin(30*time.Second, "run", "--", "/bin/sh", "-c", script)
			if r.code != want {
				t.Errorf("cm run exited %d for a shell killed by %s, want %d as /bin/sh does.\n"+
					"stderr: %s", r.code, sig, want, r.stderr)
			}
			// And not reported as cm having lost the session, which is a different problem and the one
			// this was being confused with.
			if strings.Contains(r.stderr, "ended unexpectedly") {
				t.Errorf("cm run reported %q for a shell killed by %s: the command was killed, which cm "+
					"tracked, so this is the command's outcome rather than cm losing the session",
					strings.TrimSpace(r.stderr), sig)
			}
		})
	}
}

// TestListReportsASignalDeathAsKilled is the other half, and the more misleading one.
//
// A session killed by a signal was recorded as `state=dead exit_code=0`. Zero means success, so `cm ls`
// and `cm info --json` said a killed session had finished cleanly, which is worse than saying nothing:
// anything reading the JSON to decide whether work succeeded got the wrong answer.
func TestListReportsASignalDeathAsKilled(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	want := shellStatus(t, "kill -TERM $$")

	e.mustRun("attach", "--no-attach", "killed", "--", "/bin/sh", "-c", "kill -TERM $$")
	e.waitFor("the session to finish", 20*time.Second, func() bool {
		return e.sessionDetail(t, "killed").State != "running"
	})

	got := e.sessionDetail(t, "killed")
	if got.State != "exited" {
		t.Errorf("state = %q for a session whose shell was killed, want exited: cm tracked this session "+
			"throughout, so it is not one cm lost", got.State)
	}
	if got.ExitCode != want {
		t.Errorf("exit_code = %d for a session killed by SIGTERM, want %d as /bin/sh reports. Zero here "+
			"means success, so anything reading this to decide whether work succeeded got the wrong answer",
			got.ExitCode, want)
	}

	// The control: a session that really did exit cleanly still reports 0, so the check above is about the
	// signal rather than about any nonzero value being produced.
	e.mustRun("attach", "--no-attach", "clean", "--", "/bin/sh", "-c", "exit 0")
	e.waitFor("the clean session to finish", 20*time.Second, func() bool {
		return e.sessionDetail(t, "clean").State != "running"
	})
	if clean := e.sessionDetail(t, "clean"); clean.State != "exited" || clean.ExitCode != 0 {
		t.Errorf("a clean exit reports state=%q exit_code=%d, want exited and 0",
			clean.State, clean.ExitCode)
	}
}

// shellStatus asks /bin/sh what status it reports for a script.
//
// The reference for every comparison here, and obtained by asking the shell rather than by inspecting a
// wait status. 128+signal is a convention rather than a standard, so a test that hardcoded 143 could not
// notice a platform where it differs, and reading it through Go's ExitCode would reproduce the exact bug
// under test: that call returns -1 for a signal death, which is what made cm lose the information.
//
// The inner shell runs the script and the outer one prints its status, which is precisely the question a
// script asking "did this command succeed" is asking.
func shellStatus(t *testing.T, script string) int {
	t.Helper()

	out, err := exec.Command("/bin/sh", "-c", "/bin/sh -c "+shellQuote(script)+"; echo $?").Output()
	if err != nil {
		t.Fatalf("asking /bin/sh for the status of %q: %v", script, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parsing the status /bin/sh reported for %q from %q: %v", script, out, err)
	}
	return n
}

// shellQuote wraps a script in single quotes for passing to sh -c.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
