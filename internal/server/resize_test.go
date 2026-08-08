package server

import (
	"testing"
)

// resizeFixture returns a session with a policy and no shim, since these tests exercise the policy
// decision rather than the pty call.
func resizeFixture(t *testing.T, policy ResizePolicy) *Session {
	t.Helper()
	rec := startShimFor(t, shimConfigFor("resize-"+string(policy), "sleep 5"))
	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	sess.SetResizePolicy(policy)
	return sess
}

// attachAt attaches a client and reports its size, returning the decision.
func attachAt(t *testing.T, sess *Session, rows, cols uint16, readOnly bool) (*attachToken, bool) {
	t.Helper()
	att, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	_, _, _, _, resize := sess.registerClientSize(att.token, rows, cols, 0, 0, readOnly)
	return att.token, resize
}

// Under the default policy, a second client attaching must not resize the session. That is the bug
// this policy exists to fix: opening another window silently reflowed the first.
func TestResizeLeaderSecondAttachDoesNotSteal(t *testing.T) {
	sess := resizeFixture(t, ResizeLeader)

	if _, resize := attachAt(t, sess, 24, 80, false); !resize {
		t.Error("first attach did not size the session, want it to")
	}
	if _, resize := attachAt(t, sess, 60, 200, false); resize {
		t.Error("second attach resized the session, want the first client's size kept")
	}
}

// Typing transfers sizing, which is how a user moves between windows.
func TestResizeLeaderTypingTransfers(t *testing.T) {
	sess := resizeFixture(t, ResizeLeader)

	attachAt(t, sess, 24, 80, false)
	second, _ := attachAt(t, sess, 60, 200, false)

	rows, cols, _, _, resize := sess.claimLeadership(second)
	if !resize {
		t.Fatal("typing in the second client did not transfer sizing")
	}
	if rows != 60 || cols != 200 {
		t.Errorf("transferred size = %dx%d, want 60x200", rows, cols)
	}

	// Typing again as the same client is not a change, so nothing should resize.
	if _, _, _, _, again := sess.claimLeadership(second); again {
		t.Error("typing as the existing leader resized again, want no change")
	}
}

// A follower must never become leader, however much it sends: reflowing the window it is watching is
// the hazard.
func TestResizeLeaderFollowerNeverClaims(t *testing.T) {
	sess := resizeFixture(t, ResizeLeader)

	attachAt(t, sess, 24, 80, false)
	follower, resize := attachAt(t, sess, 60, 200, true)
	if resize {
		t.Error("a read-only client sized the session on attach")
	}
	if _, _, _, _, claimed := sess.claimLeadership(follower); claimed {
		t.Error("a read-only client became leader, want followers excluded")
	}
}

// A leader leaving must not reflow a remaining window. Sizing is unclaimed until someone types,
// because a window nobody touched changing shape is exactly the surprise being avoided.
func TestResizeLeaderDetachLeavesSizeAlone(t *testing.T) {
	sess := resizeFixture(t, ResizeLeader)

	leader, _ := attachAt(t, sess, 24, 80, false)
	other, _ := attachAt(t, sess, 60, 200, false)

	if _, _, _, _, resize := sess.releaseClientSize(leader); resize {
		t.Error("the leader detaching resized the session, want the size kept until someone types")
	}
	// The remaining client can now claim it by typing.
	if _, _, _, _, claimed := sess.claimLeadership(other); !claimed {
		t.Error("the remaining client could not claim sizing after the leader left")
	}
}

// last-attach is the old behavior, kept for anyone who wants it.
func TestResizeLastAttach(t *testing.T) {
	sess := resizeFixture(t, ResizeLastAttach)

	if _, resize := attachAt(t, sess, 24, 80, false); !resize {
		t.Error("first attach did not size the session")
	}
	rows, cols := uint16(0), uint16(0)
	att, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	rows, cols, _, _, resize := sess.registerClientSize(att.token, 60, 200, 0, 0, false)
	if !resize {
		t.Fatal("second attach did not resize under last-attach")
	}
	if rows != 60 || cols != 200 {
		t.Errorf("size = %dx%d, want 60x200", rows, cols)
	}
}

// first-attach keeps sizing with the earliest client, so a later window cannot take it.
func TestResizeFirstAttach(t *testing.T) {
	sess := resizeFixture(t, ResizeFirstAttach)

	first, resize := attachAt(t, sess, 24, 80, false)
	if !resize {
		t.Error("first attach did not size the session")
	}
	if _, resize := attachAt(t, sess, 60, 200, false); resize {
		t.Error("a later attach resized under first-attach")
	}
	// Typing must not transfer it either, since that is the leader policy's behavior.
	second, _ := attachAt(t, sess, 70, 210, false)
	if _, _, _, _, claimed := sess.claimLeadership(second); claimed {
		t.Error("typing transferred sizing under first-attach")
	}

	// When the earliest client leaves, the next earliest takes over.
	rows, cols, _, _, resize := sess.releaseClientSize(first)
	if !resize {
		t.Fatal("sizing did not transfer when the earliest client left")
	}
	if rows != 60 || cols != 200 {
		t.Errorf("size after handoff = %dx%d, want the next earliest client's 60x200", rows, cols)
	}
}

// smallest fits every client, so nothing is cut off for anyone.
func TestResizeSmallest(t *testing.T) {
	sess := resizeFixture(t, ResizeSmallest)

	rows, cols, _, _, resize := func() (uint16, uint16, uint16, uint16, bool) {
		att, err := sess.attach(nil)
		if err != nil {
			t.Fatalf("attach() error = %v", err)
		}
		return sess.registerClientSize(att.token, 40, 120, 0, 0, false)
	}()
	if !resize || rows != 40 || cols != 120 {
		t.Errorf("first attach = (%dx%d, %v), want (40x120, true)", rows, cols, resize)
	}

	// A smaller client constrains both dimensions independently, since a window can be shorter and
	// wider at once.
	att, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	rows, cols, _, _, resize = sess.registerClientSize(att.token, 24, 200, 0, 0, false)
	if !resize {
		t.Fatal("second attach did not resize under smallest")
	}
	if rows != 24 || cols != 120 {
		t.Errorf("size = %dx%d, want 24x120: the minimum of each dimension", rows, cols)
	}

	// When the constraining client leaves, the session can grow again.
	rows, cols, _, _, resize = sess.releaseClientSize(att.token)
	if !resize {
		t.Fatal("sizing did not relax when the smaller client left")
	}
	if rows != 40 || cols != 120 {
		t.Errorf("size after release = %dx%d, want 40x120", rows, cols)
	}
}

// A client that reports no size cannot own sizing, since there is nothing to resize to. This happens
// for real: a client with piped stdio has no tty and reports zeros.
func TestResizeIgnoresZeroSizes(t *testing.T) {
	sess := resizeFixture(t, ResizeLeader)

	if _, resize := attachAt(t, sess, 0, 0, false); resize {
		t.Error("a client reporting no size resized the session")
	}
	// And a real client after it still works.
	if _, resize := attachAt(t, sess, 24, 80, false); !resize {
		t.Error("a sized client could not claim sizing after an unsized one")
	}
}

// A detached client has no say, which matters because a resize can race its own disconnect.
func TestResizeAfterDetachIsIgnored(t *testing.T) {
	sess := resizeFixture(t, ResizeLeader)

	tok, _ := attachAt(t, sess, 24, 80, false)
	sess.releaseClientSize(tok)

	if _, _, _, _, resize := sess.registerClientSize(tok, 99, 300, 0, 0, false); resize {
		t.Error("a detached client resized the session")
	}
	if _, _, _, _, claimed := sess.claimLeadership(tok); claimed {
		t.Error("a detached client claimed leadership")
	}
}

// An unset policy must behave as leader, so an existing config keeps working and a single client is
// unaffected.
func TestResizeDefaultPolicyIsLeader(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("resize-default", "sleep 5"))
	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()
	// No SetResizePolicy call.

	if _, resize := attachAt(t, sess, 24, 80, false); !resize {
		t.Error("first attach did not size the session under the default policy")
	}
	if _, resize := attachAt(t, sess, 60, 200, false); resize {
		t.Error("second attach resized under the default policy, want leader behavior")
	}
}
