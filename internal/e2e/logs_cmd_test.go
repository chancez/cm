package e2e

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// enableLogging writes a config that turns on debug logging.
//
// Client logging follows the same log_level as everything else rather than having its own switch, so a test
// that wants client lines has to raise the level.
func (e *env) enableLogging() {
	e.t.Helper()
	e.writeConfig("log_level = \"debug\"\n")
}

// The three kinds of diagnostic log each have a subcommand, and each finds its own file.
//
// Worth asserting together: they differ only in which path they name, so a wiring mistake would point two of
// them at the same file and the output would look plausible.
func TestLogsSubcommands(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.enableLogging()

	e.mustRun("run", "--session", "logged", "-d", "--", "/bin/sh", "-c", "sleep 30")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("logged")
		return ok && s.State == "running"
	})

	// A follower, so a client writes something. Stopped immediately; the log is what matters.
	go func() { e.runWithin(3*time.Second, "read", "--follow", "logged") }()
	e.waitFor("a client to log", 15*time.Second, func() bool {
		return strings.Contains(e.run("logs", "client").stdout, "pid=")
	})

	server := e.mustRun("logs", "server")
	if !strings.Contains(server, "level=") {
		t.Errorf("`logs server` printed no log lines:\n%s", server)
	}

	client := e.mustRun("logs", "client")
	// The fields that identify which client wrote a line, which is how one shared file stays useful.
	if !strings.Contains(client, "pid=") {
		t.Errorf("`logs client` output has no pid field:\n%s", client)
	}
	if !strings.Contains(client, "boot=") {
		t.Errorf("`logs client` output has no boot field:\n%s", client)
	}

	shim := e.mustRun("logs", "shim", "logged")
	if !strings.Contains(shim, "shim started") {
		t.Errorf("`logs shim` did not print the shim's log:\n%s", shim)
	}

	// Three different files, not one read three times.
	if server == client || server == shim || client == shim {
		t.Error("two subcommands printed identical output, so they are reading the same file")
	}
}

// Bare `cm logs` prints help rather than guessing.
//
// It used to print the server's log. With three kinds it would be arbitrary, and a command that silently picks
// one of three is worse than one that asks.
func TestLogsBarePrintsHelp(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := e.mustRun("logs")
	for _, want := range []string{"logs server", "logs client", "logs shim"} {
		if !strings.Contains(out, want) {
			t.Errorf("bare `cm logs` does not mention %q:\n%s", want, out)
		}
	}
}

// Diagnostic logs live in per-type subdirectories, and session output does not.
//
// The layout is what the scanner and the subcommands both depend on, so it is asserted directly: session output
// sits in logs/ and holds whatever the shell printed, which must not be mistaken for a diagnostic log.
func TestLogLayout(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.enableLogging()

	e.mustRun("run", "--session", "lay", "-d", "--", "/bin/sh", "-c", "echo output; sleep 30")
	// Both kinds of log are named after the session's ID rather than its name, since a name is a
	// binding and can be pointed at another session while the file it named goes on being appended to.
	id := e.sessionID("lay")
	e.waitFor("the shim to log", 15*time.Second, func() bool {
		return e.readFileOrEmpty(filepath.Join(e.state, "logs", "shim", id+".log")) != ""
	})

	// Diagnostics, each in its own directory.
	for _, rel := range []string{
		filepath.Join("logs", "server", "server.log"),
		filepath.Join("logs", "shim", id+".log"),
	} {
		if e.readFileOrEmpty(filepath.Join(e.state, rel)) == "" {
			t.Errorf("%s is missing or empty", rel)
		}
	}

	// Session output, directly in logs/ and not in a diagnostic directory.
	if e.readFileOrEmpty(filepath.Join(e.state, "logs", id+".log")) == "" {
		t.Error("the session's output log is missing from logs/")
	}
	if e.readFileOrEmpty(filepath.Join(e.state, "logs", "shim", "shim-"+id+".log")) != "" {
		t.Error("a shim log kept its old prefixed name, so the layout is inconsistent")
	}
}

// --clear works per subcommand, emptying only the log it names.
//
// Each subcommand owns one file, so clearing one must not touch the others: a --clear that emptied everything
// would destroy evidence a reader was about to look at.
func TestLogsClearPerSubcommand(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.enableLogging()

	e.mustRun("run", "--session", "sel", "-d", "--", "/bin/sh", "-c", "sleep 30")
	id := e.sessionID("sel")
	e.waitFor("both logs to have content", 15*time.Second, func() bool {
		return e.readFileOrEmpty(filepath.Join(e.state, "logs", "server", "server.log")) != "" &&
			e.readFileOrEmpty(filepath.Join(e.state, "logs", "shim", id+".log")) != ""
	})

	e.mustRun("logs", "shim", "sel", "--clear")

	if body := e.readFileOrEmpty(filepath.Join(e.state, "logs", "shim", id+".log")); body != "" {
		t.Errorf("the shim log was not cleared: %q", body)
	}
	// The server's is untouched, which is the point of clearing one at a time.
	if e.readFileOrEmpty(filepath.Join(e.state, "logs", "server", "server.log")) == "" {
		t.Error("clearing the shim log also cleared the server's")
	}
}

// A shim log for an unknown session reports that rather than printing nothing.
//
// `cm logs shim typo` returning silence looks like a session with no diagnostics, which is a different and
// more worrying thing than a name that does not exist.
func TestLogsShimUnknownSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	r := e.run("logs", "shim", "nosuchsession")
	if r.code == 0 {
		t.Errorf("exit code = 0 for an unknown session, want non-zero\nstdout: %s", r.stdout)
	}
}

// Ordinary use logs no warnings, so `cm doctor` reports a healthy installation as healthy.
//
// This was a real complaint. Three clients -- `cm run -d`, `cm attach --no-attach`, and an interrupted
// follower -- send Detach as their last act and exit without reading the acknowledgement. The server's reply
// then lost a race about 40% of the time, measured at 8 of 20 runs, and each failure logged a warning for
// behavior that was entirely intended. No session was ever at risk, so the warning was pure noise -- and
// worse, it made doctor report log-warnings on an installation where nothing was wrong.
//
// Fixed by letting a client say it will not wait, so the server skips both the reply and the warning. The
// log is quiet because nothing failed rather than because a genuine failure was downgraded. Every client
// now says it will not wait, so this covers all of them rather than only the three above.
//
// Repeated, because the bug it guards against was probabilistic: one round would have passed more often than
// not even before the fix. Even so, this test alone does not reliably catch a client that stops setting the
// flag -- removing it from `cm run -d` needs about three runs of this test to show up. The deterministic
// assertion is TestDetachWithNoAckIsNotAcknowledged in internal/server, which checks the protocol directly;
// this one proves the noise is gone from ordinary use, which that cannot.
func TestOrdinaryUseLogsNoWarnings(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "quiet", "-d", "--", "/bin/sh", "-c", "sleep 60")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("quiet")
		return ok && s.State == "running"
	})

	// Several detached runs, not one. The warning was probabilistic -- 8 of 20 before the fix -- so a single
	// `run -d` passes more often than not even when broken, and a mutation removing its no_ack survived a test
	// that only ran it once.
	for i := range 5 {
		e.mustRun("run", "--session", "d"+itoa(i), "-d", "--", "/bin/sh", "-c", "sleep 30")
	}

	for i := range 5 {
		// A follower interrupted mid-stream with SIGINT, which is how a watcher normally stops and the case
		// that produced the noise. followFor rather than runWithin, since the interruption is the point here
		// and runWithin treats a timeout as a failure.
		e.followFor(700*time.Millisecond, "read", "--follow", "quiet")
		// And a session created without attaching, which does return on its own.
		e.mustRunWithin(15*time.Second, "attach", "--no-attach", "na"+itoa(i))
	}

	// No detach-acknowledgement warnings, which is the specific noise this is about.
	log := e.readFileOrEmpty(e.serverLogPath())
	if n := strings.Count(log, "acknowledging a detach failed"); n != 0 {
		t.Errorf("the server logged %d detach-acknowledgement warnings for ordinary use:\n%s", n, log)
	}

	// And doctor does not report warnings, which is the consequence that made it worth fixing.
	got, _ := e.doctor()
	if slices.Contains(got.kinds(), "log-warnings") {
		t.Errorf("doctor reports log-warnings for a healthy installation: %v", got.kinds())
	}
}
