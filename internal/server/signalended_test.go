package server

import (
	"context"
	"strings"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Signalling a session that has ended but is still registered succeeds, and one that has left the
// registry is reported as ended.
//
// Both halves are asserted here because the difference is deliberate and easy to "fix" in the wrong
// direction. Service.Signal reports "has ended" only when Manager.Get misses; for a session still in
// the registry whose shell has gone it returns success, since the caller wanted the process stopped and
// it is stopped.
//
// Constructed rather than raced for, which is the point. The e2e version of this drove `cm run` to
// completion and then signalled, on the assumption that a finished command means a deregistered
// session. It does not: the registry entry is removed on the watch goroutine afterwards, so on a slower
// CI runner the signal arrived first and the test failed with "exited 0, want an error" while passing
// locally 8 runs out of 8.
//
// Waiting for `state == "exited"` does not fix it either, which is worth recording because it reads like
// the obvious answer. A live session reports itself exited before the manager writes its record back,
// deliberately, so that a caller polling a short command does not see it flip from running to dead.
// Verified by widening the window in Manager.watch: the wait succeeded and the signal still returned 0.
// The condition the error depends on is registry membership, and nothing exposes that over the RPC, so
// the honest place to test it is here.
func TestSignalOnASessionThatEndedButIsStillRegistered(t *testing.T) {
	svc := endedButRegistered(t, "ended-signal", "done\r\n")

	// SIGTERM. Accepted rather than refused: the session is still registered, so Manager.Get hits and
	// the "has ended" path is not taken.
	//
	// Note what this does and does not cover. The shim in this fixture is still alive, so sess.Signal
	// succeeds and the Ended() fallback inside Service.Signal is never reached: mutating that fallback
	// to return an error leaves this test passing. What is asserted is the registry check above it,
	// which is the half the flaky e2e test was actually racing. Covering the fallback needs a session
	// whose shim has gone while its entry remains, which this fixture does not build.
	_, err := svc.Signal(context.Background(), &serverv1.SignalRequest{
		Session: "ended-signal",
		Signal:  15,
	})
	if err != nil {
		t.Errorf("Signal() on an ended but still registered session returned %v, want success.\n"+
			"The caller wanted the process stopped and it is stopped, so reporting a failure would "+
			"describe a problem that does not exist.", err)
	}
}

// Once the session has left the registry, signalling it says so rather than quietly succeeding.
//
// This is the assertion the flaky e2e test was reaching for. A recorded session whose shell has gone has
// nothing to receive a signal, and succeeding at nothing would tell a caller its signal was delivered.
func TestSignalOnADeregisteredSessionReportsItEnded(t *testing.T) {
	svc := endedButRegistered(t, "gone-signal", "done\r\n")

	// What watch() does once the session is finished business. Removing it from the registry while
	// leaving the store record is exactly the state a caller signalling a finished session hits.
	svc.mgr.mu.Lock()
	delete(svc.mgr.sessions, "gone-signal")
	svc.mgr.mu.Unlock()

	_, err := svc.Signal(context.Background(), &serverv1.SignalRequest{
		Session: "gone-signal",
		Signal:  15,
	})
	if err == nil {
		t.Fatal("Signal() on a deregistered session returned success, want an error saying it ended")
	}
	if !strings.Contains(err.Error(), "ended") {
		t.Errorf("Signal() error = %q, want it to say the session has ended.\n"+
			"The message distinguishes finished business from a wrong name, which is what a caller's "+
			"next move depends on.", err)
	}
}

// A name nothing knows about is a wrong name, not a finished session.
func TestSignalOnAnUnknownSessionSaysNotFound(t *testing.T) {
	svc := endedButRegistered(t, "known", "done\r\n")

	_, err := svc.Signal(context.Background(), &serverv1.SignalRequest{
		Session: "no-such-session",
		Signal:  15,
	})
	if err == nil {
		t.Fatal("Signal() on an unknown session returned success, want an error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("Signal() error = %q, want it to say the session was not found: a wrong name and a "+
			"finished session need different responses from the caller.", err)
	}
}
