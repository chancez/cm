package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A client that detaches and exits leaves nothing in the server's log.
//
// When the Attach handler returns, ttrpc writes the stream's closing status to the client. A client that
// sent its Detach and closed the connection straight away made that write fail, and ttrpc logged it at
// ERROR as "failed sending message on channel ... broken pipe". `cm doctor` reports every ERROR line as a
// server error for a day, so `cm run -d` alone made a healthy installation report a problem about one
// create in three, which is what failed TestDoctorIsQuietAfterACleanKill in CI.
//
// Repeated, because each detach loses the race only sometimes: measured before the fix at 8 errors in 20 for
// `cm run -d`, so 20 attempts all passing without the fix is under one chance in ten thousand. The two
// interactive paths lost it far less often, 1 in 20 and 0 in 20, and are guarded deterministically by
// TestDetachWaitsForTheServerToEndTheStream in internal/client instead.
const detachReps = 20

func TestDetachingClientsLeaveNoServerError(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	n := 0
	cases := map[string]func(t *testing.T){
		"run detached": func(t *testing.T) {
			e.mustRun("run", "-d", "--", "/bin/sh", "-c", "sleep 300")
		},
		"attach without attaching": func(t *testing.T) {
			n++
			e.mustRun("attach", "--no-attach", fmt.Sprintf("noatt%d", n), "--", "/bin/sh", "-c", "sleep 300")
		},
	}
	for name, detach := range cases {
		t.Run(name, func(t *testing.T) {
			before := len(e.readFileOrEmpty(e.serverLogPath()))
			for range detachReps {
				detach(t)
			}
			time.Sleep(200 * time.Millisecond)
			added := e.readFileOrEmpty(e.serverLogPath())[before:]
			if n := strings.Count(added, "level=ERROR"); n > 0 {
				t.Errorf("server logged %d errors for %d clients detaching:\n%s", n, detachReps, added)
			}
		})
	}
}
