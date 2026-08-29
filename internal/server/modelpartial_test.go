package server

import (
	"strings"
	"testing"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
)

// TestAttachReplaysASequenceTheModelHasNotFinished is the other half of the model-lag hazard next door.
//
// TestAttachStreamsFromTheModelNotTheLog covers output the model has not reached: the log is ahead, so
// streaming from the log's end would skip it. This covers output the model has *consumed* but cannot
// represent. A partial escape sequence lives in the emulator's parser rather than on its screen, and
// Restore serializes the screen, so resuming at the model's raw position puts those bytes in neither the
// snapshot nor the stream and no client ever sees them.
//
// The symptom is the one this branch exists for: the front of a sequence disappears and its tail renders
// as text. A program wrote `ESC ] 2;fidelity BEL ESC [ 38:2:1` and paused, and an attaching client got
// the title set and `:2:3m` printed on screen, with the nine bytes that opened the SGR gone.
//
// Constructed rather than driven through a pty for the reason the neighbour states: the state asserted
// about is "the model has consumed a sequence it cannot yet render", and racing a real program onto it
// made the end-to-end version of this fail about one run in eight. Here it is exact.
func TestAttachReplaysASequenceTheModelHasNotFinished(t *testing.T) {
	// A complete sequence, then the start of one that is not finished. The split point is what matters:
	// the model consumes both, and can only represent the first.
	const (
		complete = "\x1b]2;fidelity\x07"
		partial  = "\x1b[38:2:1"
	)

	sess := &Session{
		id:          "modelpartial",
		recent:      seqlog.NewAt[seq.Log](DefaultRecentBytes, 0),
		term:        &fakeTerminal{restore: []byte("SCREEN")},
		clientSizes: make(map[*attachToken]*clientSize),
		evicts:      make(map[*attachToken]chan struct{}),
		metaSubs:    make(map[*metaSub]struct{}),
		boundaries:  osc.NewBoundaryTracker(0),
		done:        make(chan struct{}),
	}

	// Delivered and then fed, which is the order the pump uses, and fed through feedTerminal rather than
	// by setting modelSeq directly: the point is that the tracker inside it notices the unfinished
	// sequence, so a test that set the fields by hand would assert the arithmetic and not the wiring.
	sess.recent.Append([]byte(complete + partial))
	sess.feedTerminal([]byte(complete+partial), sess.recent.Next())

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	got := readUntil(t, att.reader, partial)

	// The unfinished sequence has to be replayed, since the screen cannot carry it.
	if !strings.Contains(got, partial) {
		t.Errorf("attached client received %q, want it to contain %q: the model consumed a sequence it "+
			"cannot serialize, so skipping those bytes loses them from both the snapshot and the stream",
			got, partial)
	}

	// And the completed one must not be, or the client paints it twice. This is the control that keeps the
	// fix from being "resume from further back and hope": backing off to the last boundary is correct,
	// backing off further is duplication.
	if strings.Contains(got, complete) {
		t.Errorf("attached client received %q, want it not to contain %q: a completed sequence is already "+
			"in the replayed screen and was streamed again", got, complete)
	}
}

// TestModelPendingIsZeroAtASequenceBoundary is the negative case, and the reason the fix costs nothing in
// the common path.
//
// Almost every chunk ends cleanly, and for those the resume position must be exactly the model's. A fix
// that always rewound would replay bytes on every attach.
func TestModelPendingIsZeroAtASequenceBoundary(t *testing.T) {
	sess := &Session{
		id:          "modelwhole",
		recent:      seqlog.NewAt[seq.Log](DefaultRecentBytes, 0),
		term:        &fakeTerminal{restore: []byte("SCREEN")},
		clientSizes: make(map[*attachToken]*clientSize),
		evicts:      make(map[*attachToken]chan struct{}),
		metaSubs:    make(map[*metaSub]struct{}),
		boundaries:  osc.NewBoundaryTracker(0),
		done:        make(chan struct{}),
	}

	const whole = "\x1b[31mred\x1b[0m"
	sess.recent.Append([]byte(whole))
	sess.feedTerminal([]byte(whole), sess.recent.Next())

	sess.termMu.Lock()
	pending := sess.modelPending
	sess.termMu.Unlock()

	if pending != 0 {
		t.Errorf("modelPending = %d after a stream ending at a boundary, want 0: an attach would replay "+
			"the last %d bytes for no reason", pending, pending)
	}
}
