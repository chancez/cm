package e2e

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// tagged starts three sessions in one group plus an untagged one, so a selector has something to
// exclude as well as match.
//
// Sleeping shells rather than anything that finishes, since these tests are about which sessions a
// command acts on rather than about what they print.
func (e *env) taggedGroup(run string) {
	e.t.Helper()
	for _, area := range []string{"api", "ui", "docs"} {
		e.mustRun("run", "--session", "s-"+area, "--tag", "run="+run, "--tag", "area="+area,
			"-d", "--", "/bin/sh", "-c", "sleep 120")
	}
	e.mustRun("run", "--session", "untagged", "-d", "--", "/bin/sh", "-c", "sleep 120")
}

// `cm kill --tag` ends exactly the selected sessions and leaves the rest.
//
// The property that matters is what it does *not* kill. This is offered as the safe alternative to
// --all, so a selector reaching a session outside the group would be worse than the flag not existing.
func TestKillByTagLeavesOtherSessions(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.taggedGroup("abc")

	e.mustRun("kill", "--tag", "run=abc")

	var names []string
	for _, s := range e.list() {
		if s.State == "running" {
			names = append(names, s.Name)
		}
	}
	if want := []string{"untagged"}; !reflect.DeepEqual(names, want) {
		t.Errorf("running sessions after kill --tag = %v, want %v", names, want)
	}
}

// A selector matching nothing must fail rather than report success.
//
// The dangerous shape: a teardown script running `cm kill --tag run=typo` and exiting 0 looks exactly
// like a successful cleanup, so it would move on believing the sessions are gone.
func TestKillByTagFailsWhenNothingMatches(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.taggedGroup("abc")

	r := e.run("kill", "--tag", "run=absent")
	if r.code == 0 {
		t.Errorf("kill --tag with no matches exited 0, want non-zero: %s", r.stdout)
	}
	// Nothing was killed by the failed call.
	if n := len(e.list()); n != 4 {
		t.Errorf("%d sessions remain, want all 4 untouched", n)
	}
}

// --all and --tag together is refused, since one means everything and the other a subset.
func TestKillRejectsAllWithTag(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.taggedGroup("abc")

	r := e.run("kill", "--all", "--tag", "run=abc")
	if r.code == 0 {
		t.Error("kill --all --tag exited 0, want it refused")
	}
	if n := len(e.list()); n != 4 {
		t.Errorf("%d sessions remain, want all 4 untouched by a refused call", n)
	}
}

// `cm wait --tag` returns only once every selected session has reached the state.
//
// Uses exited, which needs no shell integration: these sessions run /bin/sh with no OSC 133, so idle
// and busy are never reported and a wait for them would time out for reasons unrelated to selectors.
func TestWaitByTagWaitsForAll(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// Staggered so they cannot all be finished when the wait is issued: if the wait returned after the
	// first, the third would still be running.
	e.mustRun("run", "--session", "quick", "--tag", "run=abc",
		"-d", "--", "/bin/sh", "-c", "exit 0")
	e.mustRun("run", "--session", "slower", "--tag", "run=abc",
		"-d", "--", "/bin/sh", "-c", "sleep 1")

	e.mustRunWithin(30*time.Second, "wait", "--tag", "run=abc", "--until", "exited", "--timeout", "20s")

	// Both actually finished, which is what the wait claimed.
	for _, name := range []string{"quick", "slower"} {
		s, ok := e.session(name)
		if !ok {
			t.Fatalf("session %s is missing", name)
		}
		if s.State != "exited" {
			t.Errorf("session %s is %q after the wait returned, want exited", name, s.State)
		}
	}
}

// The default form fails when one selected session does not reach the state.
//
// A partial success has to be a failure, or `cm wait --tag ... && collect` would collect from sessions
// that are still working.
func TestWaitByTagFailsWhenOneSessionDoesNot(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "finishes", "--tag", "run=abc",
		"-d", "--", "/bin/sh", "-c", "exit 0")
	e.mustRun("run", "--session", "lingers", "--tag", "run=abc",
		"-d", "--", "/bin/sh", "-c", "sleep 120")

	r := e.runWithin(30*time.Second, "wait", "--tag", "run=abc", "--until", "exited", "--timeout", "2s")
	if r.code == 0 {
		t.Error("wait --tag exited 0 with one session still running, want non-zero")
	}
	// Names the session that did not get there, which is the whole point of the per-session report: a
	// bare count would not say which to look at.
	if !strings.Contains(r.stderr, "lingers") {
		t.Errorf("stderr = %q, want it to name the session that did not finish", r.stderr)
	}
	// And not the one that did.
	if strings.Contains(r.stderr, "for finishes to be") {
		t.Errorf("stderr = %q, want the satisfied session left out", r.stderr)
	}
}

// --any returns as soon as one selected session reaches the state.
//
// The timeout is the assertion: one session never exits, so a wait that required all of them would run
// the full 20s rather than returning on the one that finished.
func TestWaitByTagAnyReturnsOnTheFirst(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "finishes", "--tag", "run=abc",
		"-d", "--", "/bin/sh", "-c", "exit 0")
	e.mustRun("run", "--session", "never", "--tag", "run=abc",
		"-d", "--", "/bin/sh", "-c", "sleep 300")

	start := time.Now()
	e.mustRunWithin(30*time.Second,
		"wait", "--tag", "run=abc", "--until", "exited", "--any", "--timeout", "20s")
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("wait --any took %v, want it to return on the session that exited", elapsed)
	}
}

// --any without --tag is refused, since it describes which of several sessions.
func TestWaitRejectsAnyWithoutTag(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("run", "--session", "one", "-d", "--", "/bin/sh", "-c", "sleep 120")

	if r := e.run("wait", "one", "--until", "exited", "--any"); r.code == 0 {
		t.Error("wait --any without --tag exited 0, want it refused")
	}
}

// `cm read --tag` prints every selected session under a header naming it.
func TestReadByTagHeadsEachSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "first", "--tag", "run=abc", "--", "/bin/sh", "-c", "echo ONE")
	e.mustRun("run", "--session", "second", "--tag", "run=abc", "--", "/bin/sh", "-c", "echo TWO")
	e.mustRun("run", "--session", "other", "--", "/bin/sh", "-c", "echo EXCLUDED")

	out := e.mustRun("read", "--tag", "run=abc")

	for _, want := range []string{"=== first ===", "ONE", "=== second ===", "TWO"} {
		if !strings.Contains(out, want) {
			t.Errorf("read --tag output missing %q:\n%s", want, out)
		}
	}
	// The untagged session's output must not appear, which is what proves the selector filtered.
	if strings.Contains(out, "EXCLUDED") {
		t.Errorf("read --tag included an unselected session:\n%s", out)
	}
}

// A named session prints bare, so piping one session's output is unchanged by this feature.
func TestReadByNameHasNoHeader(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("run", "--session", "solo", "--tag", "run=abc", "--", "/bin/sh", "-c", "echo ONLY")

	out := e.mustRun("read", "solo")
	if strings.Contains(out, "===") {
		t.Errorf("read by name added a header:\n%s", out)
	}

	// But a selector matching exactly that session does head it: the caller did not know which session
	// it would be, so the name is part of the answer.
	tagged := e.mustRun("read", "--tag", "run=abc")
	if !strings.Contains(tagged, "=== solo ===") {
		t.Errorf("read --tag matching one session has no header:\n%s", tagged)
	}
}

// A name and a selector together is refused rather than resolved.
func TestReadRejectsNameWithTag(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("run", "--session", "one", "--tag", "run=abc", "-d", "--", "/bin/sh", "-c", "sleep 120")

	if r := e.run("read", "one", "--tag", "run=abc"); r.code == 0 {
		t.Error("read with both a name and --tag exited 0, want it refused")
	}
}

// --follow needs one session, since an endless stream cannot be interleaved or headed.
func TestReadRejectsFollowWithSeveralSessions(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.taggedGroup("abc")

	r := e.run("read", "--tag", "run=abc", "--follow")
	if r.code == 0 {
		t.Error("read --tag --follow across several sessions exited 0, want it refused")
	}
	if !strings.Contains(r.stderr, "--follow") {
		t.Errorf("stderr = %q, want it to name --follow", r.stderr)
	}
}

// --format=html needs one session, since each is a whole document.
func TestHistoryRejectsHTMLWithSeveralSessions(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.taggedGroup("abc")

	r := e.run("history", "--tag", "run=abc", "--format", "html")
	if r.code == 0 {
		t.Error("history --tag --format=html across several sessions exited 0, want it refused")
	}
	// Plain text over the same group works, so the refusal is about the format rather than the selector.
	if out := e.mustRun("history", "--tag", "run=abc"); !strings.Contains(out, "=== s-api ===") {
		t.Errorf("history --tag output has no header:\n%s", out)
	}
}

// `cm info --tag --field` prints one value per session, so a selector plus a field is a list.
func TestInfoByTagWithField(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.taggedGroup("abc")

	out := e.mustRun("info", "--tag", "run=abc", "--field", "name")

	// Bare values with no headers, since a field is what a script reads.
	if strings.Contains(out, "===") {
		t.Errorf("info --field added a header, want bare values:\n%s", out)
	}
	got := strings.Fields(out)
	sort.Strings(got)
	if want := []string{"s-api", "s-docs", "s-ui"}; !reflect.DeepEqual(got, want) {
		t.Errorf("info --tag --field name = %v, want %v", got, want)
	}
}

// `cm info --tag --json` is an array, while a named session stays an object.
//
// The shape is a contract: an existing `cm info NAME --json | jq .cwd` must keep working, and a
// selector has to compose with `.[]`.
func TestInfoJSONShapeDependsOnHowItWasSelected(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.taggedGroup("abc")

	byTag := strings.TrimSpace(e.mustRun("info", "--tag", "run=abc", "--json"))
	if !strings.HasPrefix(byTag, "[") {
		t.Errorf("info --tag --json = %q, want an array", byTag)
	}
	byName := strings.TrimSpace(e.mustRun("info", "s-api", "--json"))
	if !strings.HasPrefix(byName, "{") {
		t.Errorf("info NAME --json = %q, want an object", byName)
	}
}
