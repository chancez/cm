package e2e

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestUpgradeKeepsTheClientProcessID pins the property a whole feature was almost built on the opposite of.
//
// `cm clients upgrade` replaces a client's binary with syscall.Exec, and exec keeps the process id. That is not
// incidental: reexecForUpgrade uses exec precisely so the pid, the open descriptors and the terminal survive,
// and its comment says so.
//
// It is asserted here because the opposite was written down first, in a comment and in docs/ideas.md, and read
// as a reason to give clients an identity that outlives the process. The server keys a returning client's place
// in the attach order on the pid, so if a re-exec really did change it, that would break on every upgrade. It
// does not, and this is the test that says so rather than a comment nobody re-measures.
func TestUpgradeKeepsTheClientProcessID(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "pidkeep", "--", "/bin/sh")
	c.waitReady()
	// Typed so this client is the active one, which is what --current resolves to.
	c.typeLine("echo READY")
	c.waitForOutput("READY", 15*time.Second)

	before := clientPIDs(t, e, "pidkeep")
	if len(before) != 1 {
		t.Fatalf("clients before = %v, want exactly one so the comparison is unambiguous", before)
	}

	// --force, because client and server come from one binary here so a plain upgrade reports every client
	// already-current and nothing re-execs at all. That is how an earlier version of this measured nothing.
	e.mustRun("clients", "upgrade", "pidkeep", "--current", "--force")

	e.waitFor("the client to come back", 30*time.Second, func() bool {
		s, ok := e.session("pidkeep")
		return ok && s.Clients == 1
	})
	// Settle, so this reads the returning attachment rather than the departing one.
	time.Sleep(500 * time.Millisecond)

	after := clientPIDs(t, e, "pidkeep")
	if len(after) != 1 {
		t.Fatalf("clients after = %v, want exactly one", after)
	}
	if after[0] != before[0] {
		t.Errorf("the client's pid changed across an upgrade: %d became %d. exec is supposed to keep it, and "+
			"the attach order keys on it, so a changed pid means an upgraded client loses its place and "+
			"sizing can move to another window", before[0], after[0])
	}

	// And the client is still working afterwards, so this is not measuring a corpse.
	c.typeLine("echo STILL_HERE")
	c.waitForOutput("STILL_HERE", 15*time.Second)
}

// clientPIDs returns the pids `cm clients list` reports for a session.
func clientPIDs(t *testing.T, e *env, session string) []int {
	t.Helper()

	out := e.mustRun("clients", "list", session)
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "*"))
		// SESSION PID KIND BUILD ATTACHED, so a data row starts with the session name and has a numeric pid.
		if len(fields) < 2 || fields[0] != session {
			continue
		}
		pid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}
