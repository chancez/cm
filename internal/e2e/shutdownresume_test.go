package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chancez/cm/internal/fault"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/store"
)

// TestShutdownInsideTheAppendWindowStoresConsistentPositions stops the server while its pump is between
// appending a chunk to the client log and advancing lastSeq.
//
// That is the window resumePoints documents. Manager.Close used to read the resume positions there, so the
// client position counted the chunk and the shim position did not. The next server then resubscribed from
// before the chunk, received it again, and appended it to a log already numbered past it: output shown
// twice, and a report in it counted as new, which satisfies a `cm wait` issued after the restart.
//
// Ordered by two pauses rather than by timing. The pump is held inside the window, and Close is held right
// after it lets go of the session, which both the old and the fixed ordering reach without the pump moving.
// Only then is the pump released. Positions read before that point are the stale pair; positions read after
// the pump has returned are the real one.
//
// Asserted as the stored pair after this restart against the stored pair after a second, unpaused one. The
// session prints nothing in between, so a correct pair survives a restart unchanged, while a stale one moves
// by the length of the re-delivered chunk.
func TestShutdownInsideTheAppendWindowStoresConsistentPositions(t *testing.T) {
	skipIfShort(t)

	dir := t.TempDir()
	pumpGate := filepath.Join(dir, "pump")
	closeGate := filepath.Join(dir, "close")
	e := newEnvWith(t, cmHooksBinary(t), "",
		"CM_TESTHOOK_FAULTS=after-log-append:pause="+pumpGate+",after-session-release:pause="+closeGate)

	// MARK is the session's only output until a line of input arrives, and none ever does, so the chunk
	// held at the pause is MARK and nothing can move the positions afterwards.
	e.mustRun("attach", "--no-attach", "window", "--", "/bin/sh", "-c", "printf 'MARK\\r\\n'; read line")
	id := e.sessionDetail(t, "window").ID

	waitForFile(t, e, "the pump to be held inside the append window", fault.Reached(pumpGate))

	stop := e.startInBackground(t, "server", "stop")
	waitForFile(t, e, "Close to have let go of the session", fault.Reached(closeGate))
	release(t, pumpGate)
	release(t, closeGate)
	if err := stop.wait(t, 20*time.Second); err != nil {
		t.Fatalf("cm server stop: %v\nstderr: %s", err, stop.errBuf.String())
	}
	e.waitServerGone()
	first := storedResume(t, e, id)

	// Adopted by a new server, which is held to nothing now that both gates exist. MARK on its screen is
	// the synchronization: adoption replays history only up to the stored shim position, so with a stale
	// pair MARK can only arrive as the re-delivered chunk, and waiting for it waits for that to be consumed.
	e.list()
	e.waitForOutputInSession("window", "MARK", 20*time.Second)
	e.restartServer()
	second := storedResume(t, e, id)

	if first != second {
		t.Errorf("stored resume positions = %+v after a restart inside the append window, then %+v after a "+
			"restart with nothing printed in between.\nThe first pair was read while the pump had appended "+
			"a chunk without advancing lastSeq, so the next server received that chunk again.", first, second)
	}
}

// resumePair is the two resume positions a stopped server leaves in a session's record.
type resumePair struct {
	Shim   seq.Shim
	Client seq.Log
}

// storedResume reads a session's resume positions from the database. Requires no server running.
func storedResume(t *testing.T, e *env, id string) resumePair {
	t.Helper()
	dirs := paths.Dirs{Runtime: e.runtime, State: e.state}
	st, err := store.OpenExisting(context.Background(), dirs.Database())
	if err != nil {
		t.Fatalf("store.OpenExisting() error = %v", err)
	}
	defer st.Close()
	rec, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return resumePair{Shim: rec.LastSeq, Client: rec.ClientSeq}
}

// waitForFile waits for a fault pause to say it has been reached.
func waitForFile(t *testing.T, e *env, what, path string) {
	t.Helper()
	e.waitFor(what, 20*time.Second, func() bool {
		_, err := os.Stat(path)
		return err == nil
	})
}
