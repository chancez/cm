package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The last command's exit status reaches cm info, distinct from the session's own.
//
// cm parsed this from OSC 133;D all along and exposed it nowhere, so a failing command left no trace:
// `false` in a session looked exactly like `true`. The parsing was never the problem, which is why the
// regression test that matters is here rather than in internal/osc -- the gap was the plumbing to a caller.
//
// The two statuses must stay separate. exit_code is the session's: whether the shell itself has gone.
// last_command_exit_code is whether the last thing it ran succeeded. Conflating them would report a failed
// build as a dead session.
func TestCommandExitStatusReachesInfo(t *testing.T) {
	skipIfShort(t)
	requireShell(t, "/bin/zsh")
	e := newEnv(t)

	args := append([]string{"run", "--session", "st", "-d"}, e.withOSC133()...)
	args = append(args, "--", "/bin/zsh", "-i")
	e.mustRun(args...)
	e.waitFor("the shell to reach its first prompt", 20*time.Second, func() bool {
		s, ok := e.session("st")
		return ok && s.State == "running"
	})

	for _, tc := range []struct {
		command string
		want    int
	}{
		// A subshell rather than a bare `exit`, so the session survives and both statuses are observable
		// at once. `exit 7` would end the shell, leaving the *session* carrying 7 and no command status.
		{command: `sh -c "exit 0"`, want: 0},
		{command: `sh -c "exit 1"`, want: 1},
		{command: `sh -c "exit 42"`, want: 42},
	} {
		e.mustRunWithin(30*time.Second, "send", "st", tc.command,
			"--enter", "--wait", "idle", "--timeout", "20s")

		got := e.sessionDetail(t, "st")
		if !got.CommandFinished {
			t.Errorf("after %q: command_finished = false, want true", tc.command)
		}
		if got.LastCommandExitCode != tc.want {
			t.Errorf("after %q: last_command_exit_code = %d, want %d",
				tc.command, got.LastCommandExitCode, tc.want)
		}
		// The session is untouched by a failing command, which is the split being asserted.
		if got.State != "running" {
			t.Errorf("after %q: session state = %q, want running", tc.command, got.State)
		}
		if got.ExitCode != 0 {
			t.Errorf("after %q: session exit_code = %d, want 0: a failed command is not a dead session",
				tc.command, got.ExitCode)
		}
	}
}

// A shell that has run nothing reports no command status.
//
// Zero is a real exit status, so command_finished is what makes the value readable. Without it a fresh
// session would look like it had just run something successfully.
func TestNoCommandStatusBeforeAnyCommand(t *testing.T) {
	skipIfShort(t)
	requireShell(t, "/bin/zsh")
	e := newEnv(t)

	args := append([]string{"run", "--session", "fresh", "-d"}, e.withOSC133()...)
	args = append(args, "--", "/bin/zsh", "-i")
	e.mustRun(args...)
	e.waitFor("the shell to reach its first prompt", 20*time.Second, func() bool {
		s, ok := e.session("fresh")
		return ok && s.State == "running"
	})

	got := e.sessionDetail(t, "fresh")
	if got.CommandFinished {
		t.Errorf("command_finished = true for a shell that has run nothing (status %d)",
			got.LastCommandExitCode)
	}
}

// `cm send --wait` carries the status in the reply that says the wait was satisfied.
//
// One call rather than two. A second `cm info` would race the next command starting and could report the
// wrong one, which is the same class of race the combined send-and-wait exists to avoid.
func TestWaitReplyCarriesCommandStatus(t *testing.T) {
	skipIfShort(t)
	requireShell(t, "/bin/zsh")
	e := newEnv(t)

	args := append([]string{"run", "--session", "wr", "-d"}, e.withOSC133()...)
	args = append(args, "--", "/bin/zsh", "-i")
	e.mustRun(args...)
	e.waitFor("the shell to reach its first prompt", 20*time.Second, func() bool {
		s, ok := e.session("wr")
		return ok && s.State == "running"
	})

	out := e.mustRunWithin(30*time.Second, "send", "wr", `sh -c "exit 9"`,
		"--enter", "--wait", "idle", "--timeout", "20s", "--json")

	var got struct {
		Satisfied           bool `json:"satisfied"`
		CommandFinished     bool `json:"command_finished"`
		LastCommandExitCode int  `json:"last_command_exit_code"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parsing wait output %q: %v", out, err)
	}
	if !got.Satisfied {
		t.Error("satisfied = false, want true")
	}
	if !got.CommandFinished {
		t.Error("command_finished = false in the wait reply")
	}
	if got.LastCommandExitCode != 9 {
		t.Errorf("last_command_exit_code = %d in the wait reply, want 9", got.LastCommandExitCode)
	}
}

// A shell with no OSC 133 reports no command status rather than a wrong one.
//
// /bin/sh sends no markers, so there is nothing to derive from. Reporting a plausible zero would be worse
// than reporting nothing: a caller would act on a success that was never observed.
func TestNoCommandStatusWithoutShellIntegration(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "bare", "-d", "--", "/bin/sh", "-c", "sleep 30")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("bare")
		return ok && s.State == "running"
	})

	// Something that would set a status if anything were reporting.
	e.mustRun("send", "bare", "false", "--enter")
	time.Sleep(time.Second)

	got := e.sessionDetail(t, "bare")
	if got.CommandFinished {
		t.Errorf("command_finished = true for a shell with no OSC 133 (status %d)",
			got.LastCommandExitCode)
	}
}

// The field is documented in the info listing, so it is discoverable.
func TestInfoListsTheCommandStatusField(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "doc", "-d", "--", "/bin/sh", "-c", "sleep 30")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("doc")
		return ok && s.State == "running"
	})

	out := e.mustRun("info", "doc")
	if !strings.Contains(out, "last_command_exit_code") {
		t.Errorf("`cm info` does not list last_command_exit_code:\n%s", out)
	}
}
