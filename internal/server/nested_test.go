package server

import (
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
)

// Nested sessions all share one shape of bug, so they share one setup.
//
// A `cm attach` run inside a session writes the child's output to the parent's pty, because that is
// what the parent's terminal is. Everything the child's shell reports about itself therefore arrives
// in the parent's output stream as bytes indistinguishable from the parent shell's own: OSC 7 for the
// directory, OSC 2 for the title, OSC 133 for the command, and cm's own OSC 25453 report.
//
// The parent used to record all four against itself. `cm list` showed the child's directory and title
// beside the parent, the values reached the store so they survived a server restart, and a `cm wait` on
// the parent was satisfied by a report the child made. Reproduced by hand first, with lsof confirming
// that neither shell had actually changed directory.
//
// Built rather than driven through a real nested attach, per the project's testing rules: the state
// these assertions are about can be constructed exactly, where racing two real attachments to land on
// it would test the timing instead of the behavior.
func newNestedTestSession(t *testing.T, term Terminal) *Session {
	t.Helper()
	sess := &Session{
		id:          "parent",
		recent:      seqlog.NewAt[seq.Log](DefaultRecentBytes, 0),
		term:        term,
		clientSizes: make(map[*attachToken]*clientSize),
		evicts:      make(map[*attachToken]chan struct{}),
		metaSubs:    make(map[*metaSub]struct{}),
		hosting:     make(map[string]int),
		boundaries:  osc.NewBoundaryTracker(0),
		done:        make(chan struct{}),
		log:         cmlog.Discard(),
	}
	return sess
}

// The parent's own directory and title must survive a nested attach reporting different ones.
//
// The whole value is asserted rather than the two fields, so a fix that froze the decoded path while
// leaking the raw URI to `cm list` does not pass. That is a real failure mode here: CwdURI reads a
// different field from Cwd, and an early version of this fix rebaselined the two together.
func TestNestedAttachDoesNotRetargetParentDirectory(t *testing.T) {
	term := &fakeTerminal{}
	sess := newNestedTestSession(t, term)

	// The parent's shell reports where it really is, before anything is nested.
	term.mu.Lock()
	term.title = "parent-title"
	term.pwd = "file:///parent/dir"
	term.mu.Unlock()
	sess.noteMetadata()

	title, cwd := sess.Metadata()
	wantCwd := osc.Cwd{Path: "/parent/dir", IsLocal: true}
	if title != "parent-title" || !reflect.DeepEqual(cwd, wantCwd) {
		t.Fatalf("before nesting: Metadata() = (%q, %+v), want (%q, %+v)",
			title, cwd, "parent-title", wantCwd)
	}

	// A client attaches to "child" from inside this session. From here the parent's shell is blocked
	// inside `cm attach` and cannot report anything, so nothing arriving is its own.
	sess.beginHosting("child")

	// The child cds and retitles. Both reach the parent's terminal model, because the child's bytes
	// pass through the parent's pty on their way to the real terminal.
	term.mu.Lock()
	term.title = "child-title"
	term.pwd = "file:///child/dir"
	term.mu.Unlock()
	sess.noteMetadata()

	title, cwd = sess.Metadata()
	if title != "parent-title" || !reflect.DeepEqual(cwd, wantCwd) {
		t.Errorf("while hosting: Metadata() = (%q, %+v), want (%q, %+v): "+
			"the child's report was recorded against the parent",
			title, cwd, "parent-title", wantCwd)
	}
	if got := sess.CwdURI(); got != "file:///parent/dir" {
		t.Errorf("while hosting: CwdURI() = %q, want %q: the raw URI leaked even though the "+
			"decoded path did not", got, "file:///parent/dir")
	}
}

// Ending the nesting must not publish the child's last values as a change the parent just made.
//
// This is the half that a freeze alone gets wrong, and it fails silently. The parent's terminal model
// really does hold the child's directory by now, so a fix that merely skips attribution while nested
// finds a "change" on the first call afterwards and records the child's value then instead of never.
// The baseline has to move even while the published value does not.
func TestParentKeepsItsOwnValuesAfterNestingEnds(t *testing.T) {
	term := &fakeTerminal{}
	sess := newNestedTestSession(t, term)

	term.mu.Lock()
	term.title = "parent-title"
	term.pwd = "file:///parent/dir"
	term.mu.Unlock()
	sess.noteMetadata()

	sess.beginHosting("child")
	term.mu.Lock()
	term.title = "child-title"
	term.pwd = "file:///child/dir"
	term.mu.Unlock()
	sess.noteMetadata()
	sess.endHosting("child")

	// The parent's shell is back at its prompt. It has not reported yet, so the next pass over the
	// model sees the child's leftover values.
	sess.noteMetadata()

	title, cwd := sess.Metadata()
	wantCwd := osc.Cwd{Path: "/parent/dir", IsLocal: true}
	if title != "parent-title" || !reflect.DeepEqual(cwd, wantCwd) {
		t.Errorf("after nesting: Metadata() = (%q, %+v), want (%q, %+v): the child's last values "+
			"were published once the freeze lifted", title, cwd, "parent-title", wantCwd)
	}

	// And the parent is tracking again rather than frozen for good, which is the failure mode of
	// getting the release wrong: a session that never reports its directory again.
	term.mu.Lock()
	term.title = "parent-moved"
	term.pwd = "file:///parent/elsewhere"
	term.mu.Unlock()
	sess.noteMetadata()

	title, cwd = sess.Metadata()
	wantCwd = osc.Cwd{Path: "/parent/elsewhere", IsLocal: true}
	if title != "parent-moved" || !reflect.DeepEqual(cwd, wantCwd) {
		t.Errorf("after nesting ended: Metadata() = (%q, %+v), want (%q, %+v): the parent stopped "+
			"tracking its own reports", title, cwd, "parent-moved", wantCwd)
	}
}

// A report from inside a nested session must not satisfy a wait on the parent.
//
// The consequence that made this more than a cosmetic listing bug. Reproduced by hand before the fix:
// with `cm wait outer --until blocked` running, a blocked report sent into the inner session returned
// satisfied against the outer one.
//
// StateRuns is asserted alongside the state because that counter is what lets a wait tell the caller's
// own work from the state a session was already in. A fix that hid the state while still counting the
// change would leave `send --wait` seeing phantom work.
func TestNestedReportDoesNotBecomeTheParentsState(t *testing.T) {
	sess := newNestedTestSession(t, nil)

	sess.beginHosting("child")
	runsBefore := sess.StateRuns()

	// A program inside the child says it is blocked. The sequence travels through the parent's pty,
	// so the parent's tracker sees it exactly as if its own shell had written it.
	sess.reports.Feed([]byte("\x1b]25453;state=blocked;detail=child work;source=test\x07"))
	sess.noteReport()

	if got := sess.Reported(); got != (Reported{}) {
		t.Errorf("Reported() = %+v, want the zero value: the child's report became the parent's "+
			"state, which is what satisfied a wait on the parent", got)
	}
	if got := sess.StateRuns(); got != runsBefore {
		t.Errorf("StateRuns() = %d, want %d: the child's report counted as a state change on the "+
			"parent, so a wait would treat it as the caller's own work", got, runsBefore)
	}

	// Once the nesting ends the parent's own reports work normally again.
	sess.endHosting("child")
	sess.reports.Feed([]byte("\x1b]25453;state=busy;detail=my work;source=test\x07"))
	sess.noteReport()

	want := Reported{State: "busy", Detail: "my work", Source: "test"}
	if got := sess.Reported(); got != want {
		t.Errorf("Reported() = %+v, want %+v: the parent stopped accepting its own reports", got, want)
	}
}

// A command the child runs must not show as the parent's command.
//
// Asserted through Command and CommandRuns together for the same reason as the report above: `cm list`
// reads the command, and a wait reads the counter, so freezing one without the other leaves half the
// bug in place.
func TestNestedCommandDoesNotBecomeTheParentsCommand(t *testing.T) {
	sess := newNestedTestSession(t, nil)

	// The parent ran something of its own first, so the test distinguishes "frozen at the parent's
	// value" from "empty because nothing was recorded".
	sess.commands.Feed([]byte("\x1b]133;A\x07\x1b]133;C;cmdline=parent-cmd\x07"))
	sess.noteCommand()
	if got := sess.Command().Command; got != "parent-cmd" {
		t.Fatalf("before nesting: Command().Command = %q, want %q", got, "parent-cmd")
	}
	runsBefore := sess.CommandRuns()

	sess.beginHosting("child")
	sess.commands.Feed([]byte("\x1b]133;D;0\x07\x1b]133;A\x07\x1b]133;C;cmdline=child-cmd\x07"))
	sess.noteCommand()

	if got := sess.Command().Command; got != "parent-cmd" {
		t.Errorf("while hosting: Command().Command = %q, want %q: the child's command was recorded "+
			"against the parent", got, "parent-cmd")
	}
	if got := sess.CommandRuns(); got != runsBefore {
		t.Errorf("while hosting: CommandRuns() = %d, want %d: the child's command counted as a run "+
			"on the parent", got, runsBefore)
	}
}

// Two nested attachments must both have to finish before the parent resumes tracking.
//
// A bool would pass every test above and fail here: the first child detaching would unfreeze a parent
// the second is still driving, which is the case an interactive user hits by attaching twice.
func TestParentStaysFrozenUntilEveryNestedAttachEnds(t *testing.T) {
	term := &fakeTerminal{}
	sess := newNestedTestSession(t, term)

	term.mu.Lock()
	term.pwd = "file:///parent/dir"
	term.mu.Unlock()
	sess.noteMetadata()

	// Two clients attached from inside this session, to different children.
	sess.beginHosting("child-a")
	sess.beginHosting("child-b")

	if got := sess.Hosting(); !reflect.DeepEqual(got, []string{"child-a", "child-b"}) {
		t.Errorf("Hosting() = %v, want both children, sorted", got)
	}

	sess.endHosting("child-a")

	term.mu.Lock()
	term.pwd = "file:///child/b/dir"
	term.mu.Unlock()
	sess.noteMetadata()

	_, cwd := sess.Metadata()
	wantCwd := osc.Cwd{Path: "/parent/dir", IsLocal: true}
	if !reflect.DeepEqual(cwd, wantCwd) {
		t.Errorf("Metadata() cwd = %+v, want %+v: one child leaving unfroze a parent that another "+
			"was still driving", cwd, wantCwd)
	}
	if got := sess.Hosting(); !reflect.DeepEqual(got, []string{"child-b"}) {
		t.Errorf("Hosting() = %v, want only the remaining child", got)
	}

	sess.endHosting("child-b")
	if got := sess.Hosting(); got != nil {
		t.Errorf("Hosting() = %v, want nil once nothing is nested", got)
	}
}

// Attaching twice to the same child must be counted, not deduplicated.
//
// The same failure as above by a different route: keying on the child's name and deleting the entry on
// the first detach unfreezes a parent whose second attachment to that child is still running.
func TestRepeatedNestedAttachToOneChildIsCounted(t *testing.T) {
	sess := newNestedTestSession(t, nil)

	sess.beginHosting("child")
	sess.beginHosting("child")

	if got := sess.Hosting(); !reflect.DeepEqual(got, []string{"child"}) {
		t.Errorf("Hosting() = %v, want the child named once however many attachments there are", got)
	}

	sess.endHosting("child")
	if got := sess.Hosting(); !reflect.DeepEqual(got, []string{"child"}) {
		t.Errorf("Hosting() = %v, want the child still listed: one of two attachments ended", got)
	}

	sess.endHosting("child")
	if got := sess.Hosting(); got != nil {
		t.Errorf("Hosting() = %v, want nil once both attachments ended", got)
	}
}
