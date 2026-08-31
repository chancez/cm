package server

import (
	"testing"
	"time"

	"github.com/chancez/cm/internal/capability"
)

// TestLeadershipDoesNotGoToWhoeverReconnectsFirst covers the default resize policy across a restart.
//
// Under ResizeLeader the window somebody is typing in owns the session's size. Leadership is held as a token,
// and detach releases it rather than transferring it: the session keeps its size until somebody types, because
// reflowing a window nobody touched is the surprise that rule avoids.
//
// On attach, though, a client claims leadership whenever nothing else holds it. That is right for the first
// window on a session and wrong after every client has dropped at once, which is what a server restart does:
// each client reconnects and the first one in takes leadership, and with it the session's size. Which window
// that is comes down to reconnect order, so a restart resizes the session to an arbitrary one of them.
//
// Observed while chasing something else: after restarting a server with two clients attached at different
// sizes, the session came back at the second window's size.
//
// The information to do better now exists. lastInputAt is preserved across a dropped stream, so a returning
// client knows whether it was the one being used, and leadership can go to that window rather than to whoever
// happened to reconnect first.
func TestLeadershipDoesNotGoToWhoeverReconnectsFirst(t *testing.T) {
	sess := sessionForSizing(t)
	// The default, stated rather than assumed: this is the policy most people run.
	sess.resizePolicy = ResizeLeader

	a, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.noteClientIdentity(a.token, "v1", clientPID, capability.Set{})
	sess.registerClientSize(a.token, 40, 100, 0, 0, false)

	b, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess.noteClientIdentity(b.token, "v1", clientPID+1, capability.Set{})
	sess.registerClientSize(b.token, 24, 80, 0, 0, false)

	// A is the window in use.
	sess.noteClientInput(b.token)
	time.Sleep(2 * time.Millisecond)
	sess.noteClientInput(a.token)

	// The restart: every client's stream drops, and each reconnects. B happens to get there first, which is
	// the whole point: nothing about the reconnect order reflects which window somebody was using.
	sess.rememberOrder(clientPID, a.token)
	sess.rememberOrder(clientPID+1, b.token)
	sess.detach(a)
	sess.detach(b)

	bBack, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.detach(bBack)
	sess.noteClientIdentity(bBack.token, "v1", clientPID+1, capability.Set{})
	_, _, _, _, bResizes := sess.registerClientSize(bBack.token, 24, 80, 0, 0, false)

	aBack, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.detach(aBack)
	sess.noteClientIdentity(aBack.token, "v1", clientPID, capability.Set{})
	_, _, _, _, aResizes := sess.registerClientSize(aBack.token, 40, 100, 0, 0, false)

	// The end state is what is guaranteed. B reconnecting first is legitimately the most recently used window
	// at the moment it arrives, because it is the only one attached, so it does take leadership and the
	// session does resize to it. Asserting otherwise would require waiting for reconnects to settle, which is
	// a timer and a guess at how long.
	_ = bResizes

	if !aResizes {
		t.Errorf("the window that was being used did not reclaim the session's size after reconnecting, so " +
			"the session keeps whatever the reconnect order gave it")
	}
	if got := sess.leaderPID(t); got != clientPID {
		t.Errorf("leadership ended with pid %d, want %d: the session's size follows reconnect order rather "+
			"than the window somebody was typing in", got, clientPID)
	}
}

// leaderPID reports the pid of the client currently holding leadership, or zero.
func (s *Session) leaderPID(t *testing.T) int32 {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leader == nil {
		return 0
	}
	cs := s.clientSizes[s.leader]
	if cs == nil {
		return 0
	}
	return cs.pid
}
