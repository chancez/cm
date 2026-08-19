package client

import (
	"context"
	"strings"
	"testing"
)

// A gap means the client's screen was built from an incomplete stream, so it repaints instead of
// writing more bytes against state that never existed.
//
// Continuing across a hole is what makes the loss visible as corruption rather than as missing text.
// The escape sequences that established the current screen may be part of what was lost, so the next
// chunk is interpreted against the wrong state: typically the front of a sequence is gone and its
// remainder renders as literal characters. That is how a dropped resume position presented as a
// garbled TUI which a ctrl-l fixed, since a forced repaint is exactly the recovery this does
// automatically.
//
// The recovery reuses what the server already has. Dropping the resume position turns the next attach
// into a fresh one, which the server answers with a serialized screen.
func TestRunSessionRepaintsOnAGap(t *testing.T) {
	h := newHarness(t)
	seq := uint64(500)
	h.resumeFrom = &seq

	h.stream.opened("test", 500, nil)
	h.stream.output(500, "before")
	h.stream.outputGap(600, "after")
	// Queued after the gap so both outcomes terminate. Reaching the repaint returns before reading it,
	// and an implementation that ignored the gap instead falls through to the exit and fails the
	// assertions below. Without it, that implementation blocks waiting for a message that never comes,
	// so the regression shows up as a test timeout rather than as a clear failure.
	h.stream.exited(0)

	oc, err := h.run(context.Background())
	if err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	if oc != outcomeReconnect {
		t.Errorf("outcome = %v, want outcomeReconnect: a gap can only be recovered by reattaching, "+
			"since resuming is what cannot rebuild state the missing bytes established", oc)
	}
	if h.resumeFrom != nil {
		t.Errorf("resumeFrom = %d, want nil.\n"+
			"Left set, the reconnect resumes again from a position whose preceding bytes are still "+
			"missing, so the corruption repeats rather than being repaired. Nil is what makes the next "+
			"attach a fresh one, which the server answers with a repaint.", *h.resumeFrom)
	}

	// The gapped chunk is not written. Its bytes are part of the snapshot the fresh attach replays, so
	// writing them here would paint them twice, once against state that never existed.
	got := h.terminalOutput()
	if !strings.Contains(got, "before") {
		t.Errorf("terminal output = %q, want the bytes received before the gap", got)
	}
	if strings.Contains(got, "after") {
		t.Errorf("terminal output = %q, want the gapped chunk not written: it arrives again in the "+
			"snapshot the repaint replays, so writing it now duplicates it", got)
	}
}

// A follower must not repaint, because a repaint is exactly what would corrupt what it is writing.
//
// `cm read --follow` streams bytes to a pipe and sets NoRestore for that reason: the screen as it
// stands is either already printed by the caller or deliberately not wanted. So for one of these a gap
// is a fact about the stream rather than something to fix, and the bytes are delivered as they arrive.
//
// Keyed on NoRestore rather than on Output being nil. Both say "not painting a terminal", but
// NoRestore is the one that says a repaint is unwanted, and it is the flag the server already reads.
func TestRunSessionDoesNotRepaintAFollowerOnAGap(t *testing.T) {
	h := newHarness(t)
	h.opts.NoRestore = true
	var sb strings.Builder
	h.opts.Output = &sb
	seq := uint64(500)
	h.resumeFrom = &seq

	h.stream.opened("test", 500, nil)
	h.stream.outputGap(600, "after")
	h.stream.exited(0)

	oc, err := h.run(context.Background())
	if err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	if oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone: a follower reads to the end of the session rather "+
			"than reattaching, since it has no screen to repair", oc)
	}
	if got := sb.String(); got != "after" {
		t.Errorf("follower received %q, want %q: a gapped chunk is still real output and dropping it "+
			"would lose bytes from a stream being piped somewhere", got, "after")
	}
	// The position still advances, so a follower that is waiting to reach a known point still gets
	// there. 600 + len("after") == 605.
	if h.resumeFrom == nil {
		t.Fatal("resumeFrom is nil for a follower, want it advanced past the chunk it received")
	}
	if *h.resumeFrom != 605 {
		t.Errorf("resumeFrom = %d, want 605 (one past the last byte received)", *h.resumeFrom)
	}
}

// Output with no gap flag is written normally, so the repaint only fires on the condition it is for.
//
// The control for the two tests above. Without it, an implementation that repainted on every chunk
// would pass the first test and fail nothing.
func TestRunSessionDoesNotRepaintWithoutAGap(t *testing.T) {
	h := newHarness(t)
	seq := uint64(500)
	h.resumeFrom = &seq

	h.stream.opened("test", 500, nil)
	h.stream.output(500, "clean")
	h.stream.exited(0)

	oc, err := h.run(context.Background())
	if err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	if oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone: nothing was lost, so there is nothing to repair", oc)
	}
	if h.resumeFrom == nil || *h.resumeFrom != 505 {
		t.Errorf("resumeFrom = %v, want 505: an ungapped chunk advances the position rather than "+
			"clearing it", h.resumeFrom)
	}
	if got := h.terminalOutput(); !strings.Contains(got, "clean") {
		t.Errorf("terminal output = %q, want %q written normally", got, "clean")
	}
}
