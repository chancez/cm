package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A client whose server dies brings one back by itself.
//
// The failure this replaces: a server stopped by accident left every attached window frozen over a live
// shell, with nothing on screen to say why and nothing in the system noticing. The recovery was to open a
// new window and run any cm command, because every command starts a server when none is running, so the
// machinery already existed and the client that had noticed was the one process unable to reach it.
//
// Three processes are required for this to mean anything: the kill has to land on a real server, the
// replacement has to be a real spawn, and the session has to survive both, which it does because its shim
// holds the pty and neither the client nor the server owns the shell.
//
// Deliberately no cm command between the kill and the recovery. Every one of them starts a server, so a
// `cm status` here would perform the recovery this test is supposed to observe and pass whatever the client
// did. The session is driven through the attached client instead, which is only possible once a server is
// serving it again.
func TestClientRecoversFromAServerThatDied(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "recover")
	c.waitReady()

	// A baseline, so the session is known to work before anything is killed.
	c.typeLine("echo BEFORE-KILL")
	c.waitForOutput("BEFORE-KILL", 15*time.Second)

	before := e.cmStatus()
	if !before.Running || before.PID == 0 {
		t.Fatalf("status = %+v, want a running server with a pid", before)
	}

	// SIGKILL rather than `cm server stop`, which is the distinction the whole feature turns on: a stop is
	// recorded and honored, while a death is what a client is expected to repair.
	proc, err := os.FindProcess(int(before.PID))
	if err != nil {
		t.Fatalf("FindProcess(%d) error = %v", before.PID, err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("killing the server = %v", err)
	}

	// The client says so on screen first. Past reconnectQuietPeriod, so this also proves the notice is not
	// painted for the routine case, which takes about 450ms.
	c.waitForOutput("lost the server", 30*time.Second)

	// And then recovers: typing reaches the shell again, which requires a server to route it.
	c.typeLine("echo AFTER-RECOVERY")
	c.waitForOutput("AFTER-RECOVERY", 30*time.Second)

	// Safe to ask now, since the client has already proved a server is serving: this can no longer be the
	// command that started one. A different pid is what says the replacement is a new process rather than
	// the original having survived the kill.
	after := e.cmStatus()
	if !after.Running {
		t.Error("status reports no server running after recovery")
	}
	if after.PID == before.PID {
		t.Errorf("server pid is still %d, so the kill did not take and this proved nothing", after.PID)
	}
}

// A server that was asked to stop stays stopped, even with a client attached and waiting.
//
// Restoring a database snapshot needs this, and so does running a server in the foreground to watch it:
// both are defeated by a client that helpfully starts one. The marker a clean shutdown leaves is what
// separates this from the case above.
func TestClientLeavesADeliberatelyStoppedServerAlone(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "stopped")
	c.waitReady()
	c.typeLine("echo BEFORE-STOP")
	c.waitForOutput("BEFORE-STOP", 15*time.Second)

	e.mustRun("server", "stop")
	e.waitServerGone()

	// The marker is what the client reads, so its absence would make this test pass for the wrong reason.
	marker := filepath.Join(e.runtime, "server-stopped")
	e.waitFor("the stop to be recorded", 15*time.Second, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})

	// The client notices and says which kind of outage this is.
	c.waitForOutput("the server was stopped", 30*time.Second)

	// Well past the point where a recovering client would have started one, and still nothing is serving.
	time.Sleep(3 * time.Second)
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the marker is gone (%v), so something started a server that was meant to stay stopped", err)
	}
	if out := c.output(); strings.Contains(out, "AFTER") {
		t.Errorf("client output = %q, want no sign of a session that came back", out)
	}
}
