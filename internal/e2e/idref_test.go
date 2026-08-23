package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every command that takes a session must accept an "@id" reference, not only a name.
//
// This is the boundary that broke, and it broke in a way no unit test could see: each command validated
// its argument with the *name* rules before sending it, and '@' is not a legal character in a name, so
// `cm attach @a7k2m9x4` failed with "session name contains disallowed character '@'" while every test in
// the repo passed. Nothing below the CLI had been given a reference to validate.
//
// It surfaced as `cm switch` closing the window instead of moving it: a switch re-execs the client as
// `cm attach @<id>`, so a validation that rejects that form turns the whole feature into a window that
// exits. Driven here through the commands themselves for that reason, since the defect lived in the
// argument handling rather than in anything a resolver does with the value.
func TestCommandsAcceptAnIDReference(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "byid", "-d", "--", "/bin/sh", "-c", "echo READY; sleep 30")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("byid")
		return ok && s.State == "running"
	})
	ref := "@" + e.sessionID("byid")

	// Attaching by ID must reach the existing session rather than being refused or creating a second
	// one. The count is what makes the second half of that checkable: a reference cm failed to resolve
	// would have produced another session here.
	out := e.mustRun("attach", "--no-attach", ref)
	if strings.Contains(out, "disallowed character") {
		t.Errorf("attach by ID reported a validation error: %q", out)
	}
	if got := len(e.list()); got != 1 {
		t.Errorf("%d sessions after attaching by ID, want 1: the reference created a session "+
			"instead of finding one", got)
	}

	// The rest of the commands a person or a script reaches for, each of which validated its argument
	// the same way and so failed the same way.
	e.mustRun("info", ref)
	e.mustRun("read", ref)
	e.mustRun("tag", ref, "checked=yes")
	e.mustRun("send", ref, "true", "--enter")
	e.mustRun("clients", "list", ref)
	e.mustRun("get-env", ref)

	// The tag landed on the session the reference names, which is the check that the reference resolved
	// rather than being accepted and then applied somewhere else.
	s, ok := e.session("byid")
	if !ok {
		t.Fatal("the session is gone after being addressed by ID")
	}
	if s.Tags["checked"] != "yes" {
		t.Errorf("tags = %v, want the tag applied via the ID reference", s.Tags)
	}

	// And killing by ID, which is also the only way to end a session whose every name is a borrower.
	e.mustRun("kill", ref)
	if got := len(e.list()); got != 0 {
		t.Errorf("%d sessions after killing by ID, want 0", got)
	}
}

// A session whose shell has exited comes back when attached to by ID, keeping that ID.
//
// The behavior this replaces refused: reviving allocated a new identity, so attaching by ID could not
// return the one asked for and gave up instead. That made an ID a handle that stopped working the moment a
// shell exited, and left a session with no names impossible to revive at all.
//
// Driven end to end because the claim is about what a real shim and a real log do: a unit test cannot spawn
// one, so it can only show that the revive path was entered.
func TestAttachByIDRevivesAndKeepsTheID(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	// A log to restore from, which is what makes the revive more than starting a fresh shell.
	e.writeConfig("[persist]\nsessions = [\"*\"]\n")

	e.mustRun("run", "--session", "revived", "-d", "--", "/bin/sh", "-c", "echo BEFORE_EXIT; exit 3")
	e.waitFor("the session to have ended", 15*time.Second, func() bool {
		s, ok := e.session("revived")
		return ok && s.State == "exited"
	})
	before := e.sessionID("revived")

	// The reference a caller recorded while it was running still works.
	e.mustRun("attach", "--no-attach", "@"+before)
	e.waitFor("the revived session to be running", 15*time.Second, func() bool {
		s, ok := e.session("revived")
		return ok && s.State == "running"
	})

	after := e.sessionID("revived")
	if after != before {
		t.Errorf("session ID = %q after being revived, want the ID it was attached by (%q)",
			after, before)
	}

	// One log, not two. A revive under a new ID would name a new log file and orphan the old one, which
	// nothing would ever remove: expiry deletes a log through the record that names it, and that record
	// is the one the revive replaced.
	logs, err := filepath.Glob(filepath.Join(e.state, "logs", "*.log"))
	if err != nil {
		t.Fatalf("Glob() error = %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("session logs = %v, want exactly one: a revive must reuse its log rather than "+
			"orphaning it", logs)
	}

	e.mustRun("kill", "@"+after)
}
