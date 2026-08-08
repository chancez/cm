package e2e

import (
	"strings"
	"testing"
	"time"
)

// Finished sessions must not accumulate in `cm list` on a default install.
//
// This bug needed no config file to reproduce and every unit test had one. Expiry was gated on
// persistence being enabled, so with no config the manager was handed no policy at all and nothing was
// ever cleaned up: twenty `cm run` invocations left twenty dead records burying the sessions actually
// in use.
//
// Deliberately runs with no config, since that is the path that was broken and the one a new user gets.
func TestFinishedSessionsAreForgottenOnADefaultInstall(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// A session that should stay, so the assertion is "the noise went" rather than "everything went".
	e.mustRun("run", "--session", "keeper", "-d", "--", "/bin/sh", "-c", "sleep 120")

	const noise = 8
	for range noise {
		e.run("run", "--", "/bin/sh", "-c", "exit 0")
	}

	// All present at first: the records are what `cm run` reads an exit status from, so they cannot be
	// deleted immediately.
	if n := len(e.list()); n < noise {
		t.Fatalf("list has %d sessions right after the runs, want at least %d", n, noise)
	}

	// Age them past the forget interval and let the sweep run. Restarting triggers it on startup,
	// which is also the path a user hits after a reboot.
	e.ageAllRecords(10 * time.Minute)
	e.restartServer()

	sessions := e.list()
	if len(sessions) != 1 {
		t.Errorf("list has %d sessions after expiry, want only the live one: %+v", len(sessions), sessions)
	}
	if _, ok := e.session("keeper"); !ok {
		t.Error("the live session was expired, want it kept")
	}
}

// A session the user asked to persist must outlive one that merely captured its output.
//
// The two are stored identically, both with a log on disk, and differ only in intent. Sharing a
// lifetime breaks one way or the other: short commands clutter the list for a week, or a session the
// user asked to keep vanishes in minutes.
func TestPersistedSessionsOutliveCapturedOnes(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	// Persistence on so --persist is honored, but with a pattern that matches nothing.
	//
	// The pattern matters. `sessions = ["*"]` would mark every session as deliberately persisted, which
	// is correct behavior and would make this test assert nothing: the difference under test is between
	// a session the user asked to keep and one that merely captured its output, so config-based
	// matching has to stay out of it.
	e.writeConfig(`
[persist]
enabled = true
sessions = ["never-matches-*"]
expire_after = "168h"
forget_unpersisted_after = "1m"
`)
	requireTerminal(t, e)

	e.mustRun("run", "--session", "asked", "--persist", "--", "/bin/sh", "-c", "echo KEPT")
	e.mustRun("run", "--session", "casual", "--", "/bin/sh", "-c", "echo TRANSIENT")

	// Both readable while fresh: capturing output is the point of the change that made this work.
	for _, name := range []string{"asked", "casual"} {
		if r := e.run("history", name); r.code != 0 {
			t.Errorf("history %s exited %d, want the output readable: %s", name, r.code, r.stderr)
		}
	}

	// Older than the forget interval, far younger than the persisted lifetime.
	e.ageAllRecords(10 * time.Minute)
	e.restartServer()

	if _, ok := e.session("casual"); ok {
		t.Error("a session that only captured output survived, want it forgotten")
	}
	if _, ok := e.session("asked"); !ok {
		t.Error("a session the user asked to persist was forgotten, want it kept")
	}
	// And its content is still there, which is what persisting was for.
	if got := e.run("history", "asked").stdout; !strings.Contains(got, "KEPT") {
		t.Errorf("history of the persisted session = %q, want its output", got)
	}
}

// `cm run` output must be readable after the command exits.
//
// Its help says the output is "captured as session scrollback readable with 'cm history'", and it was
// not: without persistence the log was never written, so the output vanished the instant the command
// finished, which for a short command is immediately.
func TestRunOutputReadableAfterExit(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	requireTerminal(t, e)

	// Waits for the command, so by the time this returns the session has already ended. That is the
	// case that was broken.
	e.mustRun("run", "--session", "done", "--", "/bin/sh", "-c", "echo AFTER_EXIT")

	if s, ok := e.session("done"); !ok || s.State != "exited" {
		t.Fatalf("session = %+v (found=%v), want it recorded as exited", s, ok)
	}

	r := e.run("history", "done")
	if r.code != 0 {
		t.Fatalf("history after exit exited %d, want the output readable: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stdout, "AFTER_EXIT") {
		t.Errorf("history = %q, want the command's output", r.stdout)
	}
}

// A session running a command must report itself busy, and go back to idle when it finishes.
//
// This is what lets a terminal emulator ask "really close this?" only when something would be lost.
// The emulator cannot work it out itself: cm owns the pty, so all it ever sees running is `cm attach`.
//
// Driven with a real interactive zsh rather than a fake, because the whole feature depends on what a
// shell actually emits. zmx needed shell hooks maintaining a label for this; cm reads OSC 133 out of
// the output stream, so it works with no shell configuration at all.
func TestBusyTracksTheRunningCommand(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	requireShell(t, "/bin/zsh")

	// -i, because the markers come from the shell's interactive prompt hooks.
	e.mustRun("run", "--session", "busy", "-d", "--", "/bin/zsh", "-i")

	// Idle once the prompt is up. Polled rather than assumed: the shell has to start first.
	e.waitFor("the session to settle at a prompt", 15*time.Second, func() bool {
		s, ok := e.session("busy")
		return ok && s.State == "running" && !s.Busy
	})

	e.mustRun("send", "busy", "sleep 30\n")
	e.waitFor("the session to report itself busy", 15*time.Second, func() bool {
		s, _ := e.session("busy")
		return s.Busy
	})

	// And what it is running, which is the part that needs the cmdline extension.
	if s, _ := e.session("busy"); s.Command != "sleep 30" {
		t.Errorf("command = %q, want %q", s.Command, "sleep 30")
	}

	// Interrupting is the case where the shell may print a new prompt without reporting the command's
	// end. A session stuck as busy forever would make a close confirmation useless, since it would
	// always fire.
	e.mustRun("send", "busy", "\x03")
	e.waitFor("the session to go idle after an interrupt", 15*time.Second, func() bool {
		s, _ := e.session("busy")
		return !s.Busy && s.Command == ""
	})
}

// Busy state must not be persisted: it describes a process, not a record.
//
// A stored value would come back after a server restart claiming a command is running when it finished
// long ago, and a close confirmation built on it would fire for every window forever.
func TestBusyIsNotPersistedAcrossARestart(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	requireShell(t, "/bin/zsh")

	e.mustRun("run", "--session", "notstored", "-d", "--", "/bin/zsh", "-i")
	e.waitFor("the session to settle at a prompt", 15*time.Second, func() bool {
		s, ok := e.session("notstored")
		return ok && !s.Busy
	})
	e.mustRun("send", "notstored", "sleep 60\n")
	e.waitFor("the session to report itself busy", 15*time.Second, func() bool {
		s, _ := e.session("notstored")
		return s.Busy
	})

	e.restartServer()

	// The command is still genuinely running, and the new server has not seen its start marker, so it
	// reports idle. That is the correct answer for a value derived from a live stream: claiming to know
	// would mean having stored it, which is the thing being avoided.
	s, ok := e.session("notstored")
	if !ok {
		t.Fatal("the session did not survive the restart")
	}
	if s.Busy {
		t.Errorf("busy = true after a restart, want false: state describing a process must not be "+
			"restored from a record (%+v)", s)
	}
}
