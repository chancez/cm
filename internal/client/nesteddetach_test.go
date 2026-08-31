package client

import (
	"context"
	"testing"
)

// The detach key belongs to the innermost session.
//
// The bug: a nested `cm attach` reads its input from the parent's pty, so the parent's client sees ctrl-\
// first and detached itself. For a per-window session that closes the window, which is the opposite of
// what the press asked for, and the only workaround was --detach-key on every nested attach.
//
// The ordering here is load-bearing. The hosting event and the keystroke arrive on different channels and
// select picks arbitrarily among ready cases, so an output event is pushed behind the hosting event as a
// marker: the stream is ordered, so seeing the marker painted proves the hosting event was consumed
// first. Without it this test passes whatever the client does, because the key can be fed before the
// handover is known.
func TestRunSessionForwardsTheDetachKeyWhileHostingANestedClient(t *testing.T) {
	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}

	h := newHarness(t)
	h.opts.DetachKey = key
	h.stream.opened("test", 0, nil)
	h.stream.hosting(true)
	h.stream.output(0, "nested")

	done := h.runAsync(context.Background())
	if got := h.terminalOutput(); got != "nested" {
		t.Fatalf("terminal output = %q, want the marker that proves the hosting event was processed", got)
	}

	h.input <- []byte{key.Byte}
	// The Open, then the forwarded key.
	h.stream.waitForRequests(t, 2)

	if got := string(h.stream.inputs()); got != string([]byte{key.Byte}) {
		t.Errorf("input forwarded = %q, want the detach key passed through to the inner client", got)
	}
	if n := h.stream.detaches(); n != 0 {
		t.Errorf("client sent %d Detach messages, want 0: the key was the inner client's", n)
	}

	// Ended the way a session exiting does, since nothing here detaches.
	h.stream.exited(0)
	if oc := <-done; oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone", oc)
	}
	if h.result.Detached {
		t.Error("result.Detached = true, want false: this client kept its session")
	}
}

// And it takes the key back when the nesting ends, or the window could never be detached again.
func TestRunSessionTakesTheDetachKeyBackWhenNestingEnds(t *testing.T) {
	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}

	h := newHarness(t)
	h.opts.DetachKey = key
	h.stream.opened("test", 0, nil)
	h.stream.hosting(true)
	h.stream.hosting(false)
	h.stream.output(0, "back")

	done := h.runAsync(context.Background())
	if got := h.terminalOutput(); got != "back" {
		t.Fatalf("terminal output = %q, want the marker that proves both hosting events were processed", got)
	}

	h.input <- []byte{key.Byte}
	h.stream.detached()

	if oc := <-done; oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone", oc)
	}
	if !h.result.Detached {
		t.Error("result.Detached = false, want true once the nested client has gone")
	}
	if got := h.stream.inputs(); len(got) != 0 {
		t.Errorf("the detach key reached the session: %q", got)
	}
}
