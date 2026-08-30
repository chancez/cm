package e2e

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A resume position recorded in an argv must not suppress the repaint or the sizing.
//
// `cm clients upgrade` used to hand the position over by appending --resume-from-seq to the argv it
// exec'd, and exec makes that argv the process's reported command line. Anything that records a live
// command line then holds a position that was true for one instant: kitty does it under
// save_as_session --use-foreground-process, for a window it started with the shell, so a window where
// `cm attach` was run by hand gets it written into the saved session and replayed at the next startup,
// against a stream that window has never seen.
//
// Both halves of the symptom are asserted, because a resume suppresses two different things. The server
// sends no serialized screen, so the window is blank, and it used to skip sizing entirely, so the shell
// kept the size it had. The blank half heals on the next output chunk's Gap flag, but a restored window
// is a shell idle at a prompt.
//
// The flag still parses, deliberately, so the first upgrade from an older build does not exit on it. It
// is ignored instead. See resumeEnvVar.
func TestAReplayedResumeFlagStillRepaintsAndSizes(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPtySized(t, e, 24, 80, "replayed", "--", "/bin/sh")
	c.waitReady()
	c.typeLine("echo SCREEN_MARKER")
	c.waitForOutput("SCREEN_MARKER", 15*time.Second)

	c.detachKey()
	e.waitFor("the client to detach", 10*time.Second, func() bool {
		s, ok := e.session("replayed")
		return ok && s.Clients == 0
	})

	// A window coming back from a saved session file: the same command line, at whatever size the
	// restored window happens to be, carrying a position from before the terminal quit. Larger than
	// anything this session has produced, which is the out-of-range case and the one where a resume
	// leaves the client with nothing to paint and no gap to notice.
	c2 := attachOnPtySized(t, e, 30, 100, "replayed", "--resume-from-seq=999999")

	// The screen has to come back. Honoring the position means no snapshot is sent at all, so this is
	// where the blank window shows up.
	c2.waitForOutput("SCREEN_MARKER", 15*time.Second)

	// And the session has to be resized to this window. Asked of the shell rather than of cm, because
	// the failure is what a program inside the session believes: a resume skips registering the client's
	// size, so the pty keeps the 24x80 the first window set.
	c2.typeLine("stty size")
	got := c2.waitForOutput("100", 15*time.Second)
	if !strings.Contains(got, "30 100") {
		t.Errorf("stty size in the session reported %q, want 30 100: the replayed position suppressed "+
			"sizing, so the shell is still running at the size the previous window had", lastLines(got, 3))
	}
}

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

// lastLines returns the tail of s, so a failure message shows the relevant part of a whole screen.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
