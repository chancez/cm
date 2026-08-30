package e2e

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// The upgrade handover works, and leaves nothing in the process's command line.
//
// The other half of the same change, and the reason it needs a test at all: moving the position out of
// the argv is only correct if it still arrives. The old spelling shared one constant between the flag
// definition and the argv builder precisely because a mismatch there is silent, and the environment has
// the same hazard across a re-exec.
//
// `ps` is asserted directly because that is where this was noticed, and because it is what a terminal
// emulator saving a session file reads.
func TestUpgradeHandsOverThePositionOutsideTheArgv(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "handover", "--", "/bin/sh")
	c.waitReady()
	// Typed so this client is the active one, which is what --current resolves to.
	c.typeLine("echo BEFORE_UPGRADE")
	c.waitForOutput("BEFORE_UPGRADE", 15*time.Second)

	// --force, because client and server come from one binary here, so a plain upgrade finds every client
	// already current and nothing re-execs.
	e.mustRun("clients", "upgrade", "handover", "--current", "--force")
	e.waitFor("the client to come back", 30*time.Second, func() bool {
		s, ok := e.session("handover")
		return ok && s.Clients == 1
	})

	// The server says whether the returning attachment resumed or repainted, which is the only place the
	// distinction is visible: both end with the same screen, and the point of resuming is that nothing
	// changes on it.
	e.waitFor("the server to log the returning attachment", 15*time.Second, func() bool {
		return strings.Contains(e.readFileOrEmpty(e.serverLogPath()), "resuming=true")
	})

	pids := clientPIDs(t, e, "handover")
	if len(pids) != 1 {
		t.Fatalf("clients after the upgrade = %v, want exactly one", pids)
	}
	argv := processArgv(t, pids[0])
	if strings.Contains(argv, resumeFlagName) {
		t.Errorf("the upgraded client's command line is %q, which holds %s: exec makes this the "+
			"process's reported argv, so an emulator saving a session file records the position and "+
			"replays it at the next startup", argv, resumeFlagName)
	}
	// A control on the assertion above: an argv that failed to read at all would contain no flag either.
	if !strings.Contains(argv, "attach") {
		t.Errorf("the upgraded client's command line is %q, which does not look like an attach, so the "+
			"check for %s proved nothing", argv, resumeFlagName)
	}

	// Still working afterwards, so this is not measuring a corpse.
	c.typeLine("echo AFTER_UPGRADE")
	c.waitForOutput("AFTER_UPGRADE", 15*time.Second)
}

// resumeFlagName is the flag that must not appear in a live client's argv. Spelled out rather than
// imported, since cmd/cm is a main package.
const resumeFlagName = "--resume-from-seq"

// processArgv returns the command line the operating system reports for a pid.
func processArgv(t *testing.T, pid int) string {
	t.Helper()

	out, err := exec.Command("ps", "-o", "command=", "-p", itoa(pid)).Output()
	if err != nil {
		t.Fatalf("reading the command line of pid %d: %v", pid, err)
	}
	return strings.TrimSpace(string(out))
}
