package e2e

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// A read-only follower must observe an ended session rather than restart it.
//
// The bug: `cm read --follow` on a session whose shell had exited started a fresh shell under the same ID
// and streamed that instead. So a read command silently resurrected what it was asked to report on, and
// then never returned, because the session it was following was alive again by its own doing and no exit
// was ever coming. It hung until `--timeout`, and without one it hung forever.
//
// The revive itself is deliberate and stays: a terminal emulator restoring a saved window attaches by
// name and must get a working session whether or not the previous one is still alive. What was missing is
// that an observer is not that caller.
//
// No `--timeout` here on purpose. One would end the follow on a clock rather than on the session being
// over, which is the symptom masking the cause, and this test would then pass against the bug.
func TestFollowOnAnAlreadyEndedSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "gone", "-d", "--", "/bin/sh", "-c", `printf 'DONE\n'`)

	// Followed only once the session is over, which is the case a slow client hits by accident: the
	// original report was a race a race-instrumented binary lost, and this makes it the normal path.
	e.waitFor("session gone to end", 20*time.Second, func() bool {
		return strings.Contains(e.run("ls").stdout, "exited")
	})
	before := e.sessionDetail(t, "gone")

	out := e.mustRunWithin(10*time.Second, "read", "--follow", "gone")
	if !strings.Contains(out, "DONE") {
		t.Errorf("the follower did not print the session's output:\n%q", out)
	}

	// The assertion that names the defect. Returning is necessary and not sufficient: a version that
	// revived the session and also reported an exit would satisfy the check above while still having
	// started a shell nobody asked for. So the whole record has to be unchanged, shell pid included.
	//
	// The record rather than the `cm ls` table, which was the first version and failed under -race: that
	// table carries a relative CREATED column, and on a slow build it ticks over between the two reads, so
	// the test reported a side effect that was a clock.
	after := e.sessionDetail(t, "gone")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("following an ended session changed it, so a read had a side effect.\n"+
			"before: %+v\nafter:  %+v", before, after)
	}
}
