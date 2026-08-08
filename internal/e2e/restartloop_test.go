package e2e

import (
	"testing"
)

// Stopping and starting a server repeatedly must keep working.
//
// This caught a real bug that only appeared on the second or third restart. The server removed its
// socket by name when it exited, which is redundant, since Go's *net.UnixListener already unlinks the
// path it bound. It is also harmful: the removal deletes whatever is at that path at the time, and
// after a restart that is the *next* server's socket.
//
// The symptom was confusing enough to be worth recording. The new server started, logged
// "server starting" and "adopted session", and was genuinely running, while the client still failed
// with "server did not become ready within 10s" because the socket it was waiting on had been unlinked
// under it. It looked like a slow server rather than a deleted socket, and a 10-second stall in a test
// suite reads as load rather than a bug.
func TestRepeatedServerRestarts(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "survivor", "-d", "--", "/bin/sh", "-c", "sleep 120")

	// Several rounds, because the first restart worked and the failure appeared later: the socket only
	// gets deleted out from under a successor once there is a successor.
	for i := range 5 {
		e.restartServer()

		// mustRun rather than run, so a failure to reach the new server fails here with its message
		// rather than surfacing as a confusing assertion later.
		if _, ok := e.session("survivor"); !ok {
			t.Fatalf("after restart %d the session was not found", i+1)
		}
	}
}
