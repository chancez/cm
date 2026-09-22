package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/shim"
)

// cm answers the text area report itself, promptly, with no client attached.
//
// This used to be proxied, and with nothing attached the question parked, expired at requestTimeout, and
// every reply queued behind it waited that out: 507ms, 510ms and 525ms measured. The program got no answer
// at all, which is the hang docs/testing.md calls the worse of the two failures.
//
// The timeout is the assertion, not just a bound. It is well under requestTimeout, so a reply that arrives
// through the old parking path fails this rather than passing slowly.
//
// Asserted on the session's own log, which includes what the pty echoed back after cm wrote it. That echo is
// how an unread reply became visible as garbage at a prompt, so it is the signal that matches the symptom.
func TestTheTextAreaReportIsAnsweredWithNoClient(t *testing.T) {
	const (
		rows = 30
		cols = 100
	)
	rec := startShimFor(t, shim.Config{
		Session: "localreport",
		// Asks for the text area and stays alive, so the pty is still there when the answer is written.
		Command: []string{"/bin/sh", "-c", `printf '\033[18t'; sleep 10`},
		Rows:    rows,
		Cols:    cols,
	})

	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// No attach at all, which is the case under test: nothing could ever have answered this.
	//
	// The needle starts after the ESC on purpose. The pty echoes what cm writes to it, and it renders a
	// control character in caret notation, so the answer appears in the log as `^[[8;30;100t` rather than
	// with a real escape byte. A needle containing \x1b matches neither form, which is trap two in
	// docs/testing.md and is how this test first reported a working answer as missing. Leaving the ESC out
	// matches the raw bytes and the echoed form on both platforms.
	want := fmt.Sprintf("[8;%d;%dt", rows, cols)
	if got := awaitStream(t, sess, want, 250*time.Millisecond); !strings.Contains(got, want) {
		t.Errorf("the text area report was not answered within 250ms; stream was %q, want it to contain %q.\n"+
			"A reply that only appears after requestTimeout means this is still being proxied and parked.",
			got, want)
	}

	// And the size is the session's rather than a default. Without this the test would pass on a 24x80
	// fallback while reporting a size the session is not at, which is the failure a program acts on.
	if r, c := sess.Size(); r != rows || c != cols {
		t.Errorf("Size() = (%d, %d), want (%d, %d): the report above is only meaningful if the session "+
			"really is at this size", r, c, rows, cols)
	}

	// Nothing may be left in the queue. A local answer that enqueued an entry it never resolved would
	// stall every later reply until the sweeper expired it, which is the failure this change removes.
	sess.mu.Lock()
	queued := len(sess.requests)
	sess.mu.Unlock()
	if queued != 0 {
		t.Errorf("%d request(s) still queued after a locally answered report, want 0", queued)
	}
}
