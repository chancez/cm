package e2e

import (
	"path/filepath"
	"testing"
	"time"
)

// A client attached to a session when something else shelves it stays up on the shelved notice instead of
// exiting, and its r restores the session in the same window.
//
// The regression is the window closing: the attach stream ended as it does for a shell that exits, so
// shelving from `cm tui`, the CLI or a keeper closed every window on the session. Restored here, the
// declared resume command runs, which only a revive does.
func TestAClientOnAShelvedSessionRestoresItInPlace(t *testing.T) {
	skipIfShort(t)
	e := shelveEnv(t)
	startDeclared(e, "agent", filepath.Join(e.state, "recorded-dir"))
	c := attachOnPty(t, e, "agent")
	c.waitForOutput(promptMarker, 15*time.Second)

	e.mustRun("shelve", "agent")
	c.waitForOutput("agent is shelved", 15*time.Second)
	if s, ok := e.session("agent"); !ok || s.State != "shelved" {
		t.Fatalf("session after the shelve = %+v, %v; want it shelved", s, ok)
	}

	c.write([]byte("r"))
	c.waitForOutput("RESUMED-recorded-dir", 15*time.Second)
	e.waitFor("the restored session to have this client attached", 15*time.Second, func() bool {
		s, ok := e.session("agent")
		return ok && s.State == "running" && s.Clients == 1
	})
	e.mustRun("kill", "agent")
}

// d on the shelved notice leaves the way the detach key does, and the session stays shelved.
func TestAClientOnAShelvedSessionDetaches(t *testing.T) {
	skipIfShort(t)
	e := shelveEnv(t)
	startDeclared(e, "agent", filepath.Join(e.state, "recorded-dir"))
	c := attachOnPty(t, e, "agent")
	c.waitForOutput(promptMarker, 15*time.Second)

	e.mustRun("shelve", "agent")
	c.waitForOutput("agent is shelved", 15*time.Second)
	c.write([]byte("d"))
	c.waitExit(10 * time.Second)
	c.waitForOutput("detached from agent", 5*time.Second)
	if s, ok := e.session("agent"); !ok || s.State != "shelved" {
		t.Errorf("session after detaching from the notice = %+v, %v; want it still shelved", s, ok)
	}
	e.mustRun("kill", "agent")
}

// Every client on the shelved notice goes back to the session once it runs again, however it was restored:
// here `cm restore`, which no window asked for.
func TestClientsOnAShelvedSessionReattachWhenItIsRestored(t *testing.T) {
	skipIfShort(t)
	e := shelveEnv(t)
	startDeclared(e, "agent", filepath.Join(e.state, "recorded-dir"))
	first := attachOnPty(t, e, "agent")
	first.waitForOutput(promptMarker, 15*time.Second)
	second := attachOnPty(t, e, "agent")
	second.waitForOutput(promptMarker, 15*time.Second)

	e.mustRun("shelve", "agent")
	first.waitForOutput("agent is shelved", 15*time.Second)
	second.waitForOutput("agent is shelved", 15*time.Second)

	e.mustRun("restore", "agent")
	first.waitForOutput("RESUMED-recorded-dir", 15*time.Second)
	second.waitForOutput("RESUMED-recorded-dir", 15*time.Second)
	e.waitFor("both clients to be attached to the restored session", 15*time.Second, func() bool {
		s, ok := e.session("agent")
		return ok && s.State == "running" && s.Clients == 2
	})
	e.mustRun("kill", "agent")
}
