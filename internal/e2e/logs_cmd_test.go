package e2e

import (
	"path/filepath"
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
	e.waitFor("the shim to log", 15*time.Second, func() bool {
		return e.readFileOrEmpty(filepath.Join(e.state, "logs", "shim", "lay.log")) != ""
	})

	// Diagnostics, each in its own directory.
	for _, rel := range []string{
		filepath.Join("logs", "server", "server.log"),
		filepath.Join("logs", "shim", "lay.log"),
	} {
		if e.readFileOrEmpty(filepath.Join(e.state, rel)) == "" {
			t.Errorf("%s is missing or empty", rel)
		}
	}

	// Session output, directly in logs/ and not in a diagnostic directory.
	if e.readFileOrEmpty(filepath.Join(e.state, "logs", "lay.log")) == "" {
		t.Error("the session's output log is missing from logs/")
	}
	if e.readFileOrEmpty(filepath.Join(e.state, "logs", "shim", "shim-lay.log")) != "" {
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
	e.waitFor("both logs to have content", 15*time.Second, func() bool {
		return e.readFileOrEmpty(filepath.Join(e.state, "logs", "server", "server.log")) != "" &&
			e.readFileOrEmpty(filepath.Join(e.state, "logs", "shim", "sel.log")) != ""
	})

	e.mustRun("logs", "shim", "sel", "--clear")

	if body := e.readFileOrEmpty(filepath.Join(e.state, "logs", "shim", "sel.log")); body != "" {
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
