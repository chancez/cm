package e2e

import (
	"strings"
	"testing"
	"time"
)

// startRunning creates a session running a long-lived command and returns its ID.
func startRunning(t *testing.T, e *env, name string) string {
	t.Helper()
	e.mustRun("attach", "--no-attach", name, "--", "/bin/sh", "-c", "echo FIRST_RUN; exec sleep 30")
	e.waitForHistory(name, "FIRST_RUN")
	return e.sessionID(name)
}

// A command for a session that is already running fails rather than being dropped.
//
// It used to attach and exit 0 without running the command, which reads as success to whoever passed it:
// `cm attach work -- make` on a running work never ran make. The session is left exactly as it was.
func TestAttachWithACommandRefusesARunningSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	id := startRunning(t, e, "busy")

	r := e.run("attach", "--no-attach", "busy", "--", "/bin/sh", "-c", "echo SECOND_RUN")
	if r.code == 0 {
		t.Fatalf("attach with a command on a running session exited 0, want a failure\nstdout: %q", r.stdout)
	}
	if !strings.Contains(r.stderr, "busy is already running") || !strings.Contains(r.stderr, "--if-running=attach") {
		t.Errorf("stderr = %q, want it to name the running session and the way to attach anyway", r.stderr)
	}

	if s, ok := e.session("busy"); !ok || s.State != "running" || e.sessionID("busy") != id {
		t.Errorf("session after the refusal = %+v, want %s still running, untouched", s, id)
	}
}

// An interactive attach refused the same way exits promptly with the message, rather than retrying.
//
// The interactive path is the attach loop, which waits out a failed Open as an outage when the session was
// opened before. This one never was, so it must be returned as an error: the unknown-ID bug was this loop
// retrying a refusal forever with nothing on screen.
func TestInteractiveAttachWithACommandRefusesARunningSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	startRunning(t, e, "busy")

	c := attachOnPty(t, e, "busy", "--", "/bin/sh", "-c", "echo SECOND_RUN")
	c.waitExit(15 * time.Second)

	if got := c.output(); !strings.Contains(got, "busy is already running") {
		t.Errorf("client output = %q, want it to say the session is already running", got)
	}
}

// --if-running=attach, or CM_IF_RUNNING=attach, attaches as before and does not run the command.
//
// The environment variable is the same binding every flag has, which is how a caller such as an agent
// manager can set the policy once rather than on each attach.
func TestAttachIfRunningAttachDropsTheCommand(t *testing.T) {
	skipIfShort(t)
	tests := []struct {
		name string
		env  string
		args []string
	}{
		{name: "flag", args: []string{"--if-running=attach"}},
		{name: "environment", env: "CM_IF_RUNNING=attach"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			id := startRunning(t, e, "busy")

			if tt.env != "" {
				e.extraEnv = append(e.extraEnv, tt.env)
			}
			args := append([]string{"attach", "--no-attach", "busy"}, tt.args...)
			args = append(args, "--", "/bin/sh", "-c", "echo SECOND_RUN")
			r := e.run(args...)
			if r.code != 0 {
				t.Fatalf("attach exited %d, want 0\nstderr: %q", r.code, r.stderr)
			}

			if got := e.mustRun("history", "busy"); strings.Contains(got, "SECOND_RUN") {
				t.Errorf("history = %q, want the command not run in a session that was already running", got)
			}
			if e.sessionID("busy") != id {
				t.Errorf("session ID = %s, want the running session %s", e.sessionID("busy"), id)
			}
		})
	}
}

// Without a command there is nothing to drop, so --if-running has no effect and the attach succeeds.
//
// This is what a terminal emulator restoring a window by name does, and it must keep working whatever the
// policy says.
func TestAttachIfRunningIsANoOpWithoutACommand(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	id := startRunning(t, e, "busy")

	r := e.run("attach", "--no-attach", "--if-running=error", "busy")
	if r.code != 0 {
		t.Fatalf("attach without a command exited %d, want 0\nstderr: %q", r.code, r.stderr)
	}
	if e.sessionID("busy") != id {
		t.Errorf("session ID = %s, want the running session %s", e.sessionID("busy"), id)
	}
}

// A value that is not a policy fails before anything is sent, naming what is accepted.
func TestAttachIfRunningRejectsAnUnknownValue(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	r := e.run("attach", "--no-attach", "--if-running=replace", "fresh", "--", "/bin/true")
	if r.code == 0 {
		t.Fatal("attach with --if-running=replace exited 0, want it rejected")
	}
	if !strings.Contains(r.stderr, "attach, error") {
		t.Errorf("stderr = %q, want it to list the accepted values", r.stderr)
	}
	if _, ok := e.session("fresh"); ok {
		t.Error("a session was created despite the rejected flag")
	}
}
