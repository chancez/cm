package server

import (
	"testing"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/shim"
)

// clientPID stands in for a real client's process id, which is what identifies a returning client.
const clientPID = 4242

// TestADroppedClientKeepsItsPlaceInTheAttachOrder covers what a repaint costs under the resize policies that
// are keyed on attach order.
//
// A repaint is delivered as a gap, which makes the client drop its resume position and reattach. That is a
// real attach with a new order, and both ResizeFirstAttach and ResizeLastAttach decide who sizes the session
// from the order. So a repaint moved sizing between windows with nobody having attached or detached on
// purpose: under first-attach, if the repainted client was the earliest, quitting a full-screen program handed
// sizing to another window. An outage reconnect did the same thing for the same reason.
//
// A client whose stream dropped now keeps its place, keyed on the process id already on the wire. It is stable
// across a reconnect because the client redials from inside the same process.
func TestADroppedClientKeepsItsPlaceInTheAttachOrder(t *testing.T) {
	sess := sessionForSizing(t)
	sess.resizePolicy = ResizeFirstAttach

	// Two clients of different sizes. The first attached sizes the session, which is what the policy
	// promises.
	first, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.noteClientIdentity(first.token, "v1", clientPID, capability.Set{})
	second, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.noteClientIdentity(second.token, "v1", clientPID+1, capability.Set{})
	defer sess.detach(second)

	if _, _, _, _, resize := sess.registerClientSize(first.token, 40, 100, 0, 0, false); !resize {
		t.Fatal("the first client does not size the session, so the policy is not in effect and nothing " +
			"below is being tested")
	}
	if _, _, _, _, resize := sess.registerClientSize(second.token, 24, 80, 0, 0, false); resize {
		t.Fatal("the second client sizes the session under first-attach, so the fixture is wrong")
	}

	// The first client is repainted: its stream drops and it reattaches from the same process. Modelled the
	// way the handler does it, recording the order before the detach that deletes the entry it lives on.
	sess.rememberOrder(clientPID, first.token)
	sess.detach(first)

	rejoined, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.detach(rejoined)
	sess.noteClientIdentity(rejoined.token, "v1", clientPID, capability.Set{})

	// The window that was earliest is earliest again, so sizing did not move.
	if _, _, _, _, secondSizes := sess.registerClientSize(second.token, 24, 80, 0, 0, false); secondSizes {
		t.Errorf("after a repaint reattached the first client, the second client sizes the session. Nobody " +
			"attached or detached deliberately, so quitting a full-screen program moved the session to " +
			"another window's size.")
	}
	if _, _, _, _, sizes := sess.registerClientSize(rejoined.token, 40, 100, 0, 0, false); !sizes {
		t.Errorf("the repainted client no longer sizes the session, though it is the same window with the " +
			"same size and the policy is first-attach")
	}
}

// TestADeliberateDetachForfeitsItsPlace is the control, and it is what keeps the fix from meaning "the order
// never changes".
//
// Somebody who presses the detach key has left. Under first-attach the earliest remaining client should take
// over, and a returning window must not reclaim the slot by coming back later. Without this the fix would make
// the policy point at a window that is no longer watching.
func TestADeliberateDetachForfeitsItsPlace(t *testing.T) {
	sess := sessionForSizing(t)
	sess.resizePolicy = ResizeFirstAttach

	first, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.noteClientIdentity(first.token, "v1", clientPID, capability.Set{})
	second, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.noteClientIdentity(second.token, "v1", clientPID+1, capability.Set{})
	defer sess.detach(second)

	// A deliberate detach records nothing, which is the whole difference: the handler calls rememberOrder
	// only when the stream ended without one.
	sess.detach(first)

	rejoined, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.detach(rejoined)
	sess.noteClientIdentity(rejoined.token, "v1", clientPID, capability.Set{})

	if _, _, _, _, sizes := sess.registerClientSize(second.token, 24, 80, 0, 0, false); !sizes {
		t.Errorf("after a deliberate detach the remaining client does not size the session, so first-attach " +
			"is pointing at a window that left")
	}
	if _, _, _, _, sizes := sess.registerClientSize(rejoined.token, 40, 100, 0, 0, false); sizes {
		t.Errorf("a client that detached on purpose reclaimed the earliest slot by reattaching, so the " +
			"holdback is being applied to a case it is not for")
	}
}

// TestAnOlderClientWithoutAPidIsUnaffected keeps the change from depending on something an older client does
// not send.
//
// The pid is advisory: a client built before it existed sends nothing, which reads as zero. Such a client has
// to behave exactly as it did before, rather than every one of them sharing a single slot under pid zero.
func TestAnOlderClientWithoutAPidIsUnaffected(t *testing.T) {
	sess := sessionForSizing(t)

	first, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.detach(first)
	sess.noteClientIdentity(first.token, "", 0, capability.Set{})
	before := orderOf(t, sess, first.token)

	sess.rememberOrder(0, first.token)
	sess.mu.Lock()
	remembered := len(sess.resumeOrders)
	sess.mu.Unlock()
	if remembered != 0 {
		t.Errorf("a client with no pid was remembered under pid zero, so every older client would share " +
			"one slot")
	}

	second, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.detach(second)
	sess.noteClientIdentity(second.token, "", 0, capability.Set{})

	if after := orderOf(t, sess, second.token); after <= before {
		t.Errorf("the second client's order is %d, not after the first's %d: an older client must still get "+
			"a fresh place", after, before)
	}
}

// sessionForSizing builds a session backed by a real shim, because detaching one of two clients resizes the
// session to the remaining one and that reaches the shim. A literal Session panics there, which is a fixture
// failing rather than a finding.
func sessionForSizing(t *testing.T) *Session {
	t.Helper()
	rec := startShimFor(t, shim.Config{
		Session: "sizing",
		Command: []string{"/bin/sh", "-c", "sleep 30"},
		Rows:    24, Cols: 80,
	})
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("SCREEN")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

func orderOf(t *testing.T, s *Session, tok *attachToken) uint64 {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.clientSizes[tok]
	if cs == nil {
		t.Fatal("no size entry for this token")
	}
	return cs.order
}
