package server

import (
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/shim"
)

// slowTerminal is a fakeTerminal whose Write blocks, standing in for the emulator taking a
// long time on one chunk.
//
// The delay is not hypothetical. A reverse index issued while the cursor sits on the top row
// scrolls the screen down, and libghostty takes about 14ms to do that at 50x120, against about
// 10us for the same sequence anywhere else. `less` emits exactly that sequence once per line
// when paging up, so a half-page costs about 350ms in the emulator alone. Paging down emits
// plain lines and costs about 8ms. See internal/vt for the measurements.
type slowTerminal struct {
	fakeTerminal
	delay time.Duration
}

func (s *slowTerminal) Write(p []byte) error {
	time.Sleep(s.delay)
	return s.fakeTerminal.Write(p)
}

// TestPumpDeliversOutputBeforeFeedingTerminal checks that a slow terminal model does not delay
// output reaching an attached client.
//
// This is the shape of the pager-scroll-up complaint. The pump fed the emulator first and only
// then appended to the log that wakes clients, so every millisecond libghostty spent on a chunk
// was a millisecond the keystroke's response sat undelivered. The terminal model is a derived
// cache used for screen restore and `cm read`; nothing about a live client's output depends on
// it being current, so it must not sit in front of delivery.
func TestPumpDeliversOutputBeforeFeedingTerminal(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "pumplatency",
		Command: []string{"/bin/sh", "-c", "echo SCROLLED; sleep 5"},
		Rows:    24, Cols: 80,
	})

	// Far longer than any real emulator call, so the difference between "delivered before the
	// model" and "delivered after it" cannot be confused with scheduling noise.
	const delay = 2 * time.Second

	term := &slowTerminal{delay: delay}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	start := time.Now()
	readUntil(t, att.reader, "SCROLLED")
	elapsed := time.Since(start)

	// Half the delay is the threshold rather than the delay itself, so this fails clearly when
	// delivery waits on the model and passes without depending on how fast the machine is.
	if elapsed >= delay/2 {
		t.Errorf("output reached the client after %v, want well under the model's %v: "+
			"delivery is waiting on the terminal model", elapsed.Round(time.Millisecond), delay)
	}
}

// TestPumpStillFeedsTerminalModel is the control for the test above.
//
// Delivering before the model must not turn into skipping the model. A restore built from a
// model that missed output shows a screen the session never had, and that failure is invisible
// until someone reattaches.
func TestPumpStillFeedsTerminalModel(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "pumpmodel",
		Command: []string{"/bin/sh", "-c", "echo MODELED; sleep 5"},
		Rows:    24, Cols: 80,
	})

	term := &slowTerminal{delay: 10 * time.Millisecond}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	readUntil(t, att.reader, "MODELED")

	// The client has the bytes; the model may still be catching up, which is the whole point of
	// the reordering, so wait for it rather than asserting immediately.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Contains(term.Written(), "MODELED") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal model saw %q, want it to contain %q", term.Written(), "MODELED")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
