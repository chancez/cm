package server

import (
	"strings"
	"testing"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seqlog"
)

// TestAttachStreamsFromTheModelNotTheLog checks that a fresh attach loses no output when the
// terminal model is behind the log.
//
// This is the hazard created by delivering output before feeding the model. A fresh attach replays a
// serialized screen and then streams from a position, and the obvious position, the log's end, is
// now wrong: the model can be several chunks behind, so the screen does not contain those bytes and
// streaming past them means no client ever shows them. The bytes are not delayed, they are gone, and
// only on a fresh attach, which makes it the kind of thing noticed as "output goes missing after
// attaching" long after the change that caused it.
//
// The lag is real rather than hypothetical: the pump holds termMu for one chunk's write, so chunks
// arriving during that write land in the log while the model is still on the earlier one.
//
// Built from a Session literal rather than driven through a shim and a pty. The state being asserted
// about is "the log is ahead of the model by a known amount", and racing a real pump to land on it
// would either be flaky or, with a blocking fake, deadlock against attach's own wait for the model.
// Constructing it makes the test deterministic and names the invariant directly.
func TestAttachStreamsFromTheModelNotTheLog(t *testing.T) {
	const (
		consumed = "SEEN-BY-MODEL"
		lagging  = "NOT-YET-MODELED"
	)

	sess := &Session{
		name:        "modellag",
		recent:      seqlog.NewAt(DefaultRecentBytes, 0),
		term:        &fakeTerminal{restore: []byte("SCREEN")},
		clientSizes: make(map[*attachToken]*clientSize),
		evicts:      make(map[*attachToken]chan struct{}),
		metaSubs:    make(map[*metaSub]struct{}),
		boundaries:  osc.NewBoundaryTracker(0),
		done:        make(chan struct{}),
	}

	// The model consumed the first chunk, so modelSeq sits at its end and the serialized screen
	// accounts for it.
	sess.recent.Append([]byte(consumed))
	sess.modelSeq = sess.recent.Next()

	// The second chunk reached the log but not yet the model, which is the window the pump opens
	// every time it delivers before feeding.
	sess.recent.Append([]byte(lagging))

	att, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	got := readUntil(t, att.reader, lagging)

	// The lagging chunk must arrive, since the replayed screen does not contain it.
	if !strings.Contains(got, lagging) {
		t.Errorf("attached client received %q, want it to contain %q: output the terminal model had "+
			"not yet consumed was skipped rather than replayed", got, lagging)
	}

	// And the chunk the screen already shows must not be sent again, or the client paints it twice.
	if strings.Contains(got, consumed) {
		t.Errorf("attached client received %q, want it not to contain %q: output the replayed screen "+
			"already covers was streamed a second time", got, consumed)
	}
}
