package server

import (
	"testing"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
)

// newAltSession builds a session whose model is on the alternate screen, as one running a full-screen
// program is.
func newAltSession(t *testing.T, term *fakeTerminal) *Session {
	t.Helper()
	return &Session{
		id:          "alt",
		recent:      seqlog.NewAt[seq.Log](DefaultRecentBytes, 0),
		term:        term,
		clientSizes: make(map[*attachToken]*clientSize),
		evicts:      make(map[*attachToken]chan struct{}),
		metaSubs:    make(map[*metaSub]struct{}),
		boundaries:  osc.NewBoundaryTracker(0),
		done:        make(chan struct{}),
	}
}

// TestClientAttachedDuringAFullScreenProgramIsRepaintedWhenItQuits is the server half of the
// alternate-screen exit problem.
//
// A client that attaches while a full-screen program is running receives a blob describing the alternate
// screen. Nothing describes the main screen, and nothing can: libghostty serializes the active screen and
// GhosttyTerminalScreen is a read of which one that is rather than a selector, so cm has no way to put the
// session's main screen into that blob. When the program quits and sends `?1049l`, the client's terminal
// pops onto whatever its own window held before the attach.
//
// The symptom is the everyday one: attach to a session running vim, quit vim, and the screen fills with
// content from before the attach instead of the session's shell.
//
// So the transition out is the moment to repaint, and the flag that does it is the existing gap flag: a gap
// already makes a client drop its resume position and reattach, and a fresh attach answers with a
// serialized screen. That is exactly the recovery wanted, on a path that already works.
func TestClientAttachedDuringAFullScreenProgramIsRepaintedWhenItQuits(t *testing.T) {
	term := &fakeTerminal{restore: []byte("ALT-SCREEN"), onAltScreen: true}
	sess := newAltSession(t, term)

	// Attaches while the program is running, so its blob is the alternate screen.
	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	repaint := sess.repaintChan(att.token)
	if repaint == nil {
		t.Fatal("the attachment has no repaint channel, so it can never be told to repaint")
	}
	// Nothing signalled yet: the program is still running and the client is showing the right thing.
	select {
	case <-repaint:
		t.Error("a repaint was signalled while the program was still running")
	default:
	}

	// The program quits, and the fake follows the bytes, so feedTerminal sees the mode change across the
	// write the way it does against the real emulator.
	sess.recent.Append([]byte("\x1b[?1049l"))
	sess.feedTerminal([]byte("\x1b[?1049l"), sess.recent.Next())

	// Signalled without waiting for any further output, which is the part that matters: a program leaving
	// the alternate screen usually produces nothing more, so a repaint that rode on the next chunk would
	// wait for a byte that never comes. The first version did exactly that and failed about one run in four.
	select {
	case <-repaint:
	default:
		t.Error("no repaint was signalled after the program left the alternate screen, so the client keeps " +
			"showing whatever its own window held before it attached")
	}
	// One transition is one repaint, so the client does not reattach in a loop.
	select {
	case <-repaint:
		t.Error("a second repaint came from one transition, so the client would reattach repeatedly")
	default:
	}
}

// TestClientAttachedBeforeAFullScreenProgramIsNotRepainted is the control, and it is what keeps the fix
// from being a flicker on every program exit.
//
// A client attached before the program started received a main-screen blob and then the program's own
// bytes, including the `?1049h`. Its terminal's main screen holds the right content, so `?1049l` returns it
// to a correct screen and repainting it would be a visible flash for nothing. Quitting vim is common; a
// fix that repainted every attached client each time would be worse than the bug for anyone whose client
// was already correct.
func TestClientAttachedBeforeAFullScreenProgramIsNotRepainted(t *testing.T) {
	term := &fakeTerminal{restore: []byte("MAIN-SCREEN"), onAltScreen: false}
	sess := newAltSession(t, term)

	// Attaches on the main screen, before any program takes over.
	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// A program starts and takes the alternate screen.
	sess.recent.Append([]byte("\x1b[?1049h"))
	sess.feedTerminal([]byte("\x1b[?1049h"), sess.recent.Next())

	// And quits again.
	sess.recent.Append([]byte("\x1b[?1049l"))
	sess.feedTerminal([]byte("\x1b[?1049l"), sess.recent.Next())

	select {
	case <-sess.repaintChan(att.token):
		t.Error("a client attached before the program was repainted when it quit: its main screen was " +
			"already correct, so this is a flicker on every program exit for nothing")
	default:
	}
}
