package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// sum totals a per-session client count.
func sum(counts map[string]uint32) int {
	total := 0
	for _, n := range counts {
		total += int(n)
	}
	return total
}

// upgradeJSON is what `cm upgrade --json` reports.
type upgradeJSON struct {
	ServerBefore    string            `json:"server_before"`
	ServerAfter     string            `json:"server_after"`
	Asked           map[string]uint32 `json:"asked"`
	AlreadyCurrent  map[string]uint32 `json:"already_current"`
	KeptShims       int               `json:"kept_shims"`
	ClientsBefore   int               `json:"clients_before"`
	ClientsReturned int               `json:"clients_returned"`
}

// `cm upgrade` has to find the clients it is meant to upgrade, which means waiting for them.
//
// This is a regression test for the bug that shipped in the first version of the command and could only be
// seen with a real client attached. Restarting the server disconnects every client, each reconnects on its
// own 100ms retry, and asking in that gap found nobody: the command reported "no clients were attached"
// against a window that was plainly attached, and the client then came back on the old binary. Nothing
// without a real client could have caught it, since a listing cannot be missing a client that never existed.
//
// Asserted on the counts rather than on the versions, because a test runs one binary: what broke was the
// looking, not the asking, and clients_returned is exactly the number that was zero.
func TestUpgradeWaitsForClientsToReconnect(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "upgraded", "--", "/bin/sh")
	c.waitReady()
	c.typeLine("echo BEFORE_UPGRADE")
	c.waitForOutput("BEFORE_UPGRADE", 15*time.Second)

	if s, ok := e.session("upgraded"); !ok || s.Clients != 1 {
		t.Fatalf("session = %+v, want one attached client before upgrading", s)
	}

	out := e.mustRun("upgrade", "--json")
	var got upgradeJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("upgrade --json is not valid JSON: %v\n%s", err, out)
	}

	if got.ClientsBefore != 1 {
		t.Errorf("clients_before = %d, want 1: the client attached before the restart was not counted",
			got.ClientsBefore)
	}
	if got.ClientsReturned != 1 {
		t.Errorf("clients_returned = %d, want 1: the upgrade did not wait for the client to reconnect",
			got.ClientsReturned)
	}
	// One or the other: asked when its build differs from the server's, skipped when it matches, which it
	// does here since both come from one binary. What must not happen is neither, which is the bug.
	//
	// Summed rather than counted by map entry, which was this test's own first mistake: both maps carry an
	// entry per session whatever the count, so a session with nothing attached puts a zero in Asked and
	// len() reads as though a client had been considered.
	considered := sum(got.Asked) + sum(got.AlreadyCurrent)
	if considered == 0 {
		t.Errorf("upgrade considered no clients at all, so none was upgraded: %+v", got)
	}
	if got.ServerAfter == "" {
		t.Errorf("server_after is empty, want the build the new server reports: %+v", got)
	}
	// The session predates the restart, so its shim is not replaced and the report has to say so.
	if got.KeptShims != 1 {
		t.Errorf("kept_shims = %d, want 1", got.KeptShims)
	}

	// And the client is still there afterwards, holding the same session: an upgrade that lost the window
	// would be worse than one that skipped it.
	e.waitFor("the client to be attached after the upgrade", 20*time.Second, func() bool {
		s, ok := e.session("upgraded")
		return ok && s.Clients == 1
	})
	c.typeLine("echo AFTER_UPGRADE")
	c.waitForOutput("AFTER_UPGRADE", 15*time.Second)
}

// With nothing attached the command still restarts the server and says so, which is the scripted case: a
// post-install hook runs it with no windows open.
func TestUpgradeWithNoClients(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("attach", "--no-attach", "detached")

	// One line, and nothing about clients or shims: there were no clients, and a kept shim is true on
	// every run where a session exists, so saying it every time made the least actionable line the most
	// repeated one. The count stays in --json.
	out := e.mustRun("upgrade")
	if lines := strings.Count(strings.TrimSpace(out), "\n") + 1; lines != 1 {
		t.Errorf("upgrade printed %d lines, want 1:\n%s", lines, out)
	}
	if !strings.Contains(out, "on ") {
		t.Errorf("upgrade output = %q, want it to name the build it is on", out)
	}
	for _, unwanted := range []string{"client", "shim"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("upgrade output = %q, want no mention of %q when there is nothing to say",
				out, unwanted)
		}
	}

	// The session outlived the restart, which is the property the whole design rests on.
	if s, ok := e.session("detached"); !ok || s.State != "running" {
		t.Errorf("session = %+v, want it still running after the upgrade", s)
	}
}

// Run with no server it starts one, rather than failing on there being nothing to replace. That is the
// state a machine is in right after installing and before opening anything.
func TestUpgradeWithNoServerStartsOne(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	// The harness has a server by the time a test runs, since creating the environment starts one, so the
	// case has to be arranged rather than assumed.
	e.restartServer()
	e.mustRun("server", "stop")
	e.waitServerGone()

	out := e.mustRun("upgrade")
	if !strings.Contains(out, "started on") {
		t.Errorf("upgrade output = %q, want it to report starting a server", out)
	}
	// Not called an upgrade, since there was nothing to upgrade from.
	if strings.Contains(out, "upgraded") {
		t.Errorf("upgrade output = %q, want it to say started rather than upgraded", out)
	}
}
