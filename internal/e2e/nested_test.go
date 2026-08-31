package e2e

import (
	"testing"
	"time"
)

// The gap this file exists for.
//
// internal/server has thorough tests for what a session does while it is hosting a nested attach, and
// every one of them calls beginHosting directly. Nothing tested that a real nested `cm attach` ever
// reaches beginHosting, and for a long time it did not: the client declares the session it is inside by
// sending CM_SESSION, which is "@<id>", and the server looked that up as a bare ID. Every lookup missed,
// so attribution was never suspended in any shipped build while the whole unit suite passed.
//
// So these drive a real nested attach and assert the consequences the mechanism exists for, at the level
// a person sees them. They are deliberately about *observable bookkeeping* rather than about internals:
// `cm list` and the JSON it prints are what was wrong.

// osc7 renders a directory report the way a shell's integration writes it.
//
// Driven with printf rather than by running a real shell integration, per docs/testing.md: a real one
// brings its own moving parts and can stop emitting the sequence without the test noticing.
func osc7(path string) string {
	return `printf '\033]7;file://localhost` + path + `\033\\'`
}

// osc25453 renders one of cm's own state reports, as a program inside a session writes it.
func osc25453(state, detail string) string {
	return `printf '\033]25453;state=` + state + `;detail=` + detail + `;source=test\007'`
}

// A nested attach must not rewrite the bookkeeping of the session it was launched from.
//
// Every assertion here corresponds to a symptom that was real, measured by hand with lsof confirming
// neither shell had changed directory: `cm list` showed the child's directory against the parent, the
// values reached the store so they survived a restart, and `cm wait outer --until blocked` was satisfied
// by a report made inside the child.
//
// The parent reports its own directory and state *first*, which is what makes the assertions mean
// something: without that, "the parent still says /nested/outer" and "the parent says nothing at all"
// are the same observation, and a broken build passes.
func TestANestedAttachDoesNotRewriteItsParentsBookkeeping(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	outer := attachOnPty(t, e, "outer", "--", "/bin/sh")
	outer.waitReady()

	outer.typeLine(osc7("/nested/outer"))
	outer.typeLine(osc25453("busy", "outer work"))
	e.waitFor("the outer session to report its own directory and state", 15*time.Second, func() bool {
		s, ok := e.session("outer")
		return ok && s.Cwd == "/nested/outer" && s.ReportedState == "busy"
	})

	// A nested attach, started by the shell inside the outer session. Nothing is arranged for it: the
	// shim exports CM_SESSION, and the client sending that is the whole mechanism under test.
	outer.typeLine(e.bin + " attach inner -- /bin/sh")
	e.waitFor("the nested client to attach", 20*time.Second, func() bool {
		s, ok := e.session("inner")
		return ok && s.Clients == 1
	})

	// The declaration reached the server. Asserted on its own because everything below depends on it,
	// and because this is the field that was silently empty: `cm list` never printed "(hosting ...)".
	innerID := e.sessionID("inner")
	e.waitFor("the outer session to report that it is hosting the inner one", 10*time.Second, func() bool {
		s, ok := e.session("outer")
		return ok && len(s.Hosting) == 1 && s.Hosting[0] == innerID
	})

	// The child reports a directory and a state of its own. Both travel through the outer session's pty,
	// because the nested client's stdout *is* the outer session's terminal, so the outer server sees
	// bytes it cannot distinguish from its own shell's.
	outer.typeLine(osc7("/nested/inner"))
	outer.typeLine(osc25453("blocked", "inner work"))

	// The inner session claims them, which is what proves the sequences were really written and really
	// parsed. Without this the assertions below pass on a build that dropped the bytes entirely.
	e.waitFor("the inner session to record its own report", 15*time.Second, func() bool {
		s, ok := e.session("inner")
		return ok && s.Cwd == "/nested/inner" && s.ReportedState == "blocked"
	})

	// And the outer session does not. Both values, since they take different paths through the server:
	// a directory is a value that is rebaselined, a report is an event that is dropped.
	s, ok := e.session("outer")
	if !ok {
		t.Fatal("the outer session is gone")
	}
	if s.Cwd != "/nested/outer" {
		t.Errorf("outer cwd = %q, want %q: the child's directory was recorded against the parent",
			s.Cwd, "/nested/outer")
	}
	if s.ReportedState != "busy" || s.ReportedDetail != "outer work" {
		t.Errorf("outer reported = (%q, %q), want (%q, %q): the child's report became the parent's "+
			"state, which is what satisfied a `cm wait` on the parent",
			s.ReportedState, s.ReportedDetail, "busy", "outer work")
	}

	// The nesting ends. The key detaches the innermost session, which is also the thing that makes this
	// reachable without a second terminal.
	outer.detachKey()
	e.waitFor("the nested client to detach", 15*time.Second, func() bool {
		s, ok := e.session("inner")
		return ok && s.Clients == 0
	})
	e.waitFor("the outer session to stop hosting", 10*time.Second, func() bool {
		s, ok := e.session("outer")
		return ok && len(s.Hosting) == 0
	})

	// The parent's own last values survived, rather than the child's being published the moment the
	// freeze lifted. That is the half a freeze alone gets wrong, and it fails silently.
	s, _ = e.session("outer")
	if s.Cwd != "/nested/outer" || s.ReportedState != "busy" {
		t.Errorf("after nesting: outer cwd = %q, state = %q, want %q and %q: the child's last values "+
			"were published once the freeze lifted", s.Cwd, s.ReportedState, "/nested/outer", "busy")
	}

	// And the parent is tracking again rather than frozen for good, which is the other failure mode:
	// a session that never reports its own directory again.
	outer.typeLine(osc7("/nested/moved"))
	e.waitFor("the outer session to report its own directory again", 15*time.Second, func() bool {
		s, ok := e.session("outer")
		return ok && s.Cwd == "/nested/moved"
	})
}

// An attach whose output is not a terminal must not be treated as nested.
//
// The expensive direction of this decision. CM_SESSION is inherited by everything a session's shell
// starts, so `cm attach x > file` from inside a session has the variable without writing anything to the
// parent's pty. Freezing the parent for it silences a session that is reporting honestly, for as long as
// the redirect lasts, which is worse than the bug the freeze exists to fix.
//
// End to end because the condition is "is stdout a terminal", which only a real pty and a real redirect
// can put to the test. cmd/cm covers the decision itself; this covers that the answer reaches the server.
func TestAnAttachRedirectedToAFileIsNotNested(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	outer := attachOnPty(t, e, "outer", "--", "/bin/sh")
	outer.waitReady()
	outer.typeLine(osc7("/nested/outer"))
	e.waitFor("the outer session to report its own directory", 15*time.Second, func() bool {
		s, ok := e.session("outer")
		return ok && s.Cwd == "/nested/outer"
	})

	// A follower whose output is a file, started from inside the outer session. It has CM_SESSION and it
	// writes nothing to the outer pty.
	if got := e.run("attach", "--no-attach", "watched", "--", "/bin/sh"); got.code != 0 {
		t.Fatalf("creating the session exited %d: %s", got.code, got.stderr)
	}
	//
	// stdin comes from /dev/null and not from the pty, which is about the shell rather than about cm: a
	// background job that reads the terminal is stopped by SIGTTIN, so the client never got as far as
	// attaching and the test failed for a reason that had nothing to do with what it asserts.
	outer.typeLine(e.bin + " attach --read-only watched < /dev/null > " +
		t.TempDir() + "/redirect.out 2>&1 &")
	e.waitFor("the follower to attach", 20*time.Second, func() bool {
		s, ok := e.session("watched")
		return ok && s.Clients == 1
	})

	// Nothing is hosted, so the outer session keeps reporting.
	if s, _ := e.session("outer"); len(s.Hosting) != 0 {
		t.Fatalf("outer hosting = %v, want empty: an attach writing to a file is not nested and "+
			"freezing the parent for it silences a session that is reporting honestly", s.Hosting)
	}
	outer.typeLine(osc7("/nested/still-reporting"))
	e.waitFor("the outer session to keep reporting while a redirected attach runs", 15*time.Second,
		func() bool {
			s, ok := e.session("outer")
			return ok && s.Cwd == "/nested/still-reporting"
		})
}
