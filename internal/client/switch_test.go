package client

import (
	"context"
	"testing"
)

// A switch is a trip back through the loop, not the end of the attachment.
//
// The distinction is the whole design: an upgrade has to replace the process because the point is to run a
// different binary, while a switch runs the same binary against a different session, which is what a
// reconnect already does. Returning outcomeDone here would end the attachment and leave the caller to
// re-exec, which is what this replaced.
func TestRunSessionSwitchesRatherThanFinishing(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)
	h.stream.switchTo("@bbbb3333")

	oc, err := h.run(context.Background())
	if err != nil {
		t.Errorf("runSession() error = %v, want nil", err)
	}
	if oc != outcomeSwitch {
		t.Errorf("outcome = %v, want outcomeSwitch", oc)
	}
	if h.result.SwitchTo != "@bbbb3333" {
		t.Errorf("SwitchTo = %q, want the target the server named", h.result.SwitchTo)
	}
}

// And a plain detach still finishes, so the two are told apart by the field rather than by both looping.
func TestRunSessionPlainDetachFinishes(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)
	h.stream.detached()

	oc, err := h.run(context.Background())
	if err != nil {
		t.Errorf("runSession() error = %v, want nil", err)
	}
	if oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone for a plain detach", oc)
	}
	if !h.result.Detached {
		t.Error("Detached = false, want it set for a plain detach")
	}
	if h.result.SwitchTo != "" {
		t.Errorf("SwitchTo = %q, want empty for a plain detach", h.result.SwitchTo)
	}
}
