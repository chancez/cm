package server

import (
	"testing"

	"github.com/chancez/cm/internal/shim"
)

// TestARepaintMovesWhoSizesTheSession records what a repaint costs under the resize policies that are keyed
// on attach order.
//
// A repaint is delivered as a gap, which makes the client drop its resume position and reattach. That is a
// real attach: a new token, with a new order. Both ResizeFirstAttach and ResizeLastAttach decide who sizes
// the session from that order, so a reattach can move sizing from one client to another without anybody
// having attached or detached on purpose.
//
// It matters now because the alternate-screen repaint made reattaches ordinary. Before it they happened only
// on a real output gap, which is rare; now one happens every time a full-screen program exits with a client
// that attached during it. Under first-attach, if that client was the earliest, quitting vim hands sizing to
// somebody else's window.
//
// Driven through registerClientSize rather than a real reconnect, because the question is only which token
// the policy picks, and constructing the token order says that exactly.
func TestARepaintMovesWhoSizesTheSession(t *testing.T) {
	// A real shim, because detaching one of two clients resizes the session to the remaining one, and that
	// reaches the shim. A literal Session panics there, which is a fixture failing rather than a finding.
	rec := startShimFor(t, shim.Config{
		Session: "sizing",
		Command: []string{"/bin/sh", "-c", "sleep 30"},
		Rows:    24, Cols: 80,
	})
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("SCREEN")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()
	sess.resizePolicy = ResizeFirstAttach

	// Two clients of different sizes. The first one attached sizes the session, which is what the policy
	// promises.
	first, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, _, _, resize := sess.registerClientSize(first.token, 40, 100, 0, 0, false); !resize {
		t.Fatal("the first client does not size the session, so the policy is not in effect and nothing " +
			"below is being tested")
	}
	if _, _, _, _, resize := sess.registerClientSize(second.token, 24, 80, 0, 0, false); resize {
		t.Fatal("the second client sizes the session under first-attach, so the fixture is wrong")
	}

	// The first client is repainted: it drops its position and reattaches, which is a fresh attach with a
	// later order. Modelled as detach then attach, which is exactly what the client does.
	sess.detach(first)
	rejoined, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.detach(rejoined)
	defer sess.detach(second)

	// The question. Under first-attach the earliest *remaining* client sizes the session, and after the
	// reattach that is the second client, so sizing has moved to a window nobody touched.
	_, _, _, _, secondNowSizes := sess.registerClientSize(second.token, 24, 80, 0, 0, false)
	_, _, _, _, rejoinedSizes := sess.registerClientSize(rejoined.token, 40, 100, 0, 0, false)

	// Recorded as current behaviour rather than asserted as correct, because it is not correct and the fix
	// is a design decision rather than a local repair.
	//
	// The policy is being applied consistently: after the reattach the repainted client genuinely is not the
	// earliest. What is wrong is the hidden reattach, and there are two shapes for fixing it. The server
	// could deliver a repaint without tearing the attachment down, by sending the fresh restore blob to the
	// existing stream, which needs a message the client can tell apart from session output. Or a reattaching
	// client could inherit its predecessor's order, which the server can already identify from the client
	// PID it records in noteClientIdentity, and which would also stop an outage reconnect reshuffling
	// sizing.
	//
	// Not introduced by the repaint. A gap has always caused a reattach, so this has always been reachable;
	// what changed is the frequency, from a rare output gap to every full-screen program exit with a client
	// that attached during it. Worth knowing before picking a fix: this is a promotion, not a regression.
	if !secondNowSizes {
		t.Errorf("the second client no longer takes sizing after a repaint reattached the first, so the gap " +
			"this test pins has been closed. Update it deliberately, and check whether an outage reconnect " +
			"preserves sizing too, since that is the same window.")
	}
	if rejoinedSizes {
		t.Errorf("the repainted client kept sizing across its reattach, so the gap this test pins has been " +
			"closed. Update it deliberately.")
	}
}
