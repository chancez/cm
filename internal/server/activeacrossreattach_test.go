package server

import (
	"testing"
	"time"

	"github.com/chancez/cm/internal/capability"
)

// TestTheActiveClientSurvivesAReattach covers the other thing a rebuilt clientSize entry loses.
//
// activeClientLocked names the window someone is using, from lastInputAt, and that one definition drives the
// `*` in `cm clients list`, `cm clients current`, and which window `cm clients upgrade --current` acts on.
//
// lastInputAt lives on the per-attachment entry, which is deleted on detach and built fresh on attach. So a
// client that reattaches has never typed as far as the server knows, and activeClientLocked skips it. A
// reattach is not rare: a repaint is delivered as one every time a full-screen program leaves the alternate
// screen, and an outage reconnect is another.
//
// The sequence that makes it wrong: type in A, then B, then A, so A is active. A runs vim and quits, which
// repaints A. A comes back with no typing recorded, B still has some, and B becomes the active window. A
// keybinding bound to `--current` pressed in A then acts on B.
func TestTheActiveClientSurvivesAReattach(t *testing.T) {
	sess := sessionForSizing(t)

	a, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.noteClientIdentity(a.token, "v1", clientPID, capability.Set{})
	b, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.noteClientIdentity(b.token, "v1", clientPID+1, capability.Set{})
	defer sess.detach(b)

	// Typed in B and then in A, so A is the window in use.
	sess.noteClientInput(b.token)
	time.Sleep(2 * time.Millisecond)
	sess.noteClientInput(a.token)

	if got, ok := sess.ActiveClient(); !ok || got.PID != clientPID {
		t.Fatalf("active client = %+v (named %v), want the one that typed last: the fixture is wrong so "+
			"nothing below is being tested", got, ok)
	}

	// A is repainted: its stream drops and it reattaches, the way the handler does it.
	sess.rememberOrder(clientPID, a.token)
	sess.detach(a)
	rejoined, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.detach(rejoined)
	sess.noteClientIdentity(rejoined.token, "v1", clientPID, capability.Set{})

	got, ok := sess.ActiveClient()
	if !ok {
		t.Fatalf("no active client after a reattach, though two windows are attached and both have typed: " +
			"the mark disappears from `cm clients list` and --current reports 0 asked")
	}
	if got.PID != clientPID {
		t.Errorf("active client is pid %d, want %d: the window that was being used reattached and lost its "+
			"typing, so the mark moved to the other window and --current would act on it", got.PID, clientPID)
	}
}

// TestADeliberateDetachGivesUpTheActiveMark is the control, matching the one for the attach order.
//
// Somebody who presses the detach key has stopped using that window, so the mark belongs to whoever is still
// there. Without this the fix would mean "the mark never moves", which points `--current` at a window that is
// gone: an upgrade would then report zero asked rather than acting on the window in use.
func TestADeliberateDetachGivesUpTheActiveMark(t *testing.T) {
	sess := sessionForSizing(t)

	a, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.noteClientIdentity(a.token, "v1", clientPID, capability.Set{})
	b, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.noteClientIdentity(b.token, "v1", clientPID+1, capability.Set{})
	defer sess.detach(b)

	sess.noteClientInput(b.token)
	time.Sleep(2 * time.Millisecond)
	sess.noteClientInput(a.token)

	// A deliberate detach records nothing, so the returning window starts fresh. That is the whole
	// difference: the handler calls rememberOrder only when the stream ended without one.
	sess.detach(a)

	if got, ok := sess.ActiveClient(); !ok || got.PID != clientPID+1 {
		t.Errorf("active client = %+v (named %v) after the window in use detached, want the other window: "+
			"the mark has to follow somebody who is still attached", got, ok)
	}

	// And a window that comes back after detaching on purpose does not reclaim the mark by arriving.
	rejoined, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.detach(rejoined)
	sess.noteClientIdentity(rejoined.token, "v1", clientPID, capability.Set{})

	if got, _ := sess.ActiveClient(); got.PID != clientPID+1 {
		t.Errorf("active client is pid %d after a deliberate detach and reattach, want %d: attaching is not "+
			"using a window, and treating it as such would move the mark on every new window",
			got.PID, clientPID+1)
	}
}
