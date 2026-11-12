package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestClientsRestartAllAsksClientsOnTheServersBuild pins what separates restart from upgrade. Client and server
// come from one binary here, so a plain upgrade would skip the client as already current and nothing would
// re-exec, which is exactly the case restart exists for: a config change with no new build.
func TestClientsRestartAllAsksClientsOnTheServersBuild(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "restartall", "--", "/bin/sh")
	c.waitReady()
	c.typeLine("echo READY")
	c.waitForOutput("READY", 15*time.Second)

	s, ok := e.session("restartall")
	if !ok {
		t.Fatal("session restartall not listed")
	}
	// --all reports each session by ID, since the server enumerates them rather than being given names.
	got := e.mustRun("clients", "restart", "--all", "--json")
	want := `[{"session":"@` + strings.TrimPrefix(s.ID, "@") + `","asked":1,"already_current":0}]`
	if compact(got) != want {
		t.Fatalf("cm clients restart --all --json = %s, want %s", got, want)
	}

	e.waitFor("the client to come back", 30*time.Second, func() bool {
		s, ok := e.session("restartall")
		return ok && s.Clients == 1
	})
	c.typeLine("echo STILL_HERE")
	c.waitForOutput("STILL_HERE", 15*time.Second)
}

// compact strips the whitespace writeJSON indents with, so a test can compare against one line.
func compact(s string) string {
	return strings.Join(strings.Fields(s), "")
}
