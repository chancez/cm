package client

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// A lone escape must reach the session without waiting for another keystroke.
//
// The bug this pins: escape is a one-byte prefix of both CSI encodings of the detach key, so the
// holdback withheld it to see whether a longer sequence followed, and nothing flushed it until the
// next read. Pressing escape and then waiting delivered nothing at all.
//
// It is not a corner case. Escape is the keypress that leaves insert mode in zsh's vi mode, in vim,
// and in Claude Code's vi mode, so the symptom is that the mode indicator does not change until you
// press another key, and the next key is then interpreted in the mode you thought you had left.
func TestRunSessionDeliversALoneEscapeWithoutWaitingForAnotherKey(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)

	// Cancelled rather than closing the input channel: stdin here is a pipe, so IsTerminal is
	// false and exhausted input deliberately does not end the loop.
	ctx, cancel := context.WithCancel(context.Background())
	done := h.runAsync(ctx)
	defer func() {
		cancel()
		<-done
	}()

	h.input <- []byte{0x1b}

	// Polled rather than slept: the answer is "within a bounded time", and a fixed sleep either
	// makes the test slow or makes it flaky.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := h.stream.inputs(); len(got) > 0 {
			if string(got) != "\x1b" {
				t.Fatalf("input sent = %q, want a single escape", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a lone escape was never delivered to the session; it is still held back " +
				"waiting for a keystroke that may never come")
		}
		time.Sleep(time.Millisecond)
	}
}

// The grace is a bound on an existing wait, not a new delay: nothing is released before it, and the
// detach key still works when its halves straddle it.
//
// Under synctest so the clock is virtual and the assertions are exact. Measuring this against real time
// would mean sleeping for tens of milliseconds and asserting on "about", which is how a timing test
// becomes flaky and then gets deleted.
func TestRunSessionReleasesAHeldEscapeAfterTheGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.stream.opened("test", 0, nil)

		ctx, cancel := context.WithCancel(context.Background())
		done := h.runAsync(ctx)
		defer endSession(h, cancel, done)

		h.input <- []byte{0x1b}
		// Every goroutine is blocked here, so the loop has taken the escape and armed its timer.
		// Nothing needs to be waited for: whatever it was going to send, it has sent.
		synctest.Wait()
		if got := h.stream.inputs(); len(got) != 0 {
			t.Fatalf("escape forwarded after %v, want it withheld: a CSI-encoded detach key starts "+
				"with the same byte, so releasing at once would break the split case", time.Duration(0))
		}

		// One nanosecond short of the deadline, to pin the bound rather than merely the eventual
		// release. A test that only slept past it would pass with any timeout at all, including one
		// long enough to be the bug again.
		time.Sleep(escapeGrace - time.Nanosecond)
		synctest.Wait()
		if got := h.stream.inputs(); len(got) != 0 {
			t.Fatalf("escape released early as %q, want nothing before %v", got, escapeGrace)
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if got := h.stream.inputs(); string(got) != "\x1b" {
			t.Errorf("input sent = %q after %v, want a single escape", got, escapeGrace)
		}
	})
}

// A detach sequence split across reads still detaches when the halves arrive inside the grace, and the
// escape that began it never reaches the session.
//
// This is what the holdback is for, and the regression risk of bounding it: a grace that released too
// eagerly would type "[92;5u" at the shell instead of detaching. zmx hit the unbounded version of this
// failure with Claude Code, which enables modifyOtherKeys so ctrl-\ arrives as a CSI sequence.
func TestRunSessionDetachesOnASequenceSplitInsideTheGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.stream.opened("test", 0, nil)

		ctx, cancel := context.WithCancel(context.Background())
		done := h.runAsync(ctx)

		// The kitty-protocol encoding of ctrl-\, cut in two.
		h.input <- []byte("\x1b[92;5")
		synctest.Wait()
		if got := h.stream.inputs(); len(got) != 0 {
			t.Fatalf("forwarded %q, want the partial sequence withheld", got)
		}

		time.Sleep(escapeGrace / 2)
		h.input <- []byte("u")

		if oc := <-done; oc != outcomeDone {
			t.Errorf("outcome = %v, want outcomeDone: a completed detach sequence must not reconnect", oc)
		}
		if !h.result.Detached {
			t.Error("result.Detached = false, want true")
		}
		if h.stream.detaches() != 1 {
			t.Errorf("sent %d detach events, want exactly 1", h.stream.detaches())
		}
		if got := h.stream.inputs(); len(got) != 0 {
			t.Errorf("input sent = %q, want none: the sequence was the detach key, not typing", got)
		}

		// runSession has already returned, but its output reader has not.
		cancel()
		close(h.stream.recv)
	})
}

// endSession stops runSession and lets every goroutine it started finish.
//
// Needed inside a synctest bubble, which fails the test if any bubbled goroutine is still blocked when
// the body returns. Cancelling the context ends the loop but not its output reader: that one is blocked
// inside Recv rather than in a select, so only closing the stream wakes it.
func endSession(h *harness, cancel context.CancelFunc, done <-chan outcome) {
	cancel()
	close(h.stream.recv)
	<-done
}
