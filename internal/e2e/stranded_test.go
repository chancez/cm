package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/paths"
)

// A server whose runtime directory is deleted reports itself, and the next server surfaces it.
//
// This is the incident `cm doctor` was blind to, reproduced. Deleting the runtime directory under a running
// server unlinks its socket without stopping it: the process keeps listening on an inode nothing can name,
// every later command starts a second server, and the first goes on holding its sessions and their ptys. `cm
// list` showed the session as dead while its shell was alive, and the diagnosis reported no problems, because
// it reached the replacement server and asked about a directory it had just recreated.
//
// It has to be an e2e test rather than a unit test, and not because e2e is more thorough. The detection lives
// on the stranded server and the reporting is read by a different process through a file, so the thing being
// checked is precisely that two servers and a log line join up. A unit test can assert the check fires, which
// it does, and cannot assert that anyone ever sees it.
func TestAStrandedServerReportsItselfThroughTheLog(t *testing.T) {
	skipIfShort(t)

	// The instrumented build, for the socket-watch interval: production checks once a minute, which is right
	// for a server that runs for weeks and would make this test sleep a minute to assert one thing.
	e := newEnvWith(t, cmVersionBinary(t), "")
	e.extraEnv = []string{paths.Env(paths.SocketWatchEnvSuffix) + "=200ms"}
	// Restart so the server picks up the interval. newEnvWith starts one before extraEnv is set, and a
	// server already running would keep polling once a minute and this test would time out.
	e.restartServer()
	e.mustRun("list")

	e.mustRun("run", "--session", "stranded", "-d", "--", "/bin/sh", "-c", "sleep 300")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("stranded")
		return ok && s.State == "running"
	})
	before, _ := e.session("stranded")

	// Delete the runtime directory under the running server, which is what happened. Only the runtime
	// directory: the state directory holds the log and the database, and its survival is what makes the
	// reporting path work at all.
	if err := os.RemoveAll(e.runtime); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}

	// The stranded server notices and says so in the shared log.
	e.waitFor("the stranded server to report itself", 15*time.Second, func() bool {
		body, err := os.ReadFile(e.serverLogPath())
		return err == nil && strings.Contains(string(body), "this server is unreachable")
	})

	// A command now starts a second server, which is the silent part: everything appears to work.
	e.mustRun("list")

	// And that server's log check surfaces the stranded one's report. This is the assertion the whole
	// mechanism exists for: the finding reaches a human through a server that is not the broken one.
	res, code := e.doctor()
	errs := res.ofKind("server-errors")
	if len(errs) != 1 {
		t.Fatalf("findings = %v, want server-errors carrying the stranded server's report", res.kinds())
	}
	if !strings.Contains(errs[0].Detail, "unreachable-server") {
		t.Errorf("server-errors does not carry the unreachable-server report: %q", errs[0].Detail)
	}
	// The advice is in the message, since the state is confusing enough that knowing what to do is the
	// useful part.
	if !strings.Contains(errs[0].Detail, "starts another server") {
		t.Errorf("the report does not explain the consequence: %q", errs[0].Detail)
	}
	if code == 0 {
		t.Error("exit code = 0 with a finding, want non-zero")
	}

	// The stranded session's shell really is still alive, which is what makes this a leak rather than a
	// cosmetic inconsistency. Without this the test would pass for a server that had simply exited.
	if before.ShellPID == 0 {
		t.Fatal("no shell pid recorded, so this test cannot show the shell was stranded")
	}
	if err := processAlive(before.ShellPID); err != nil {
		t.Errorf("the stranded shell (pid %d) is not alive: %v; this test is not reproducing the leak",
			before.ShellPID, err)
	}

	// Cleanup: the stranded server cannot be reached by name, so the harness's own teardown cannot stop it.
	// Signalled rather than asked, which is the one case where there is no polite channel.
	t.Cleanup(func() { killStrandedServers(t, e) })
}
