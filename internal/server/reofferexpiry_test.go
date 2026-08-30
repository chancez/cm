package server

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chancez/cm/internal/shim"
)

// TestAReofferedQuestionCannotWedgeTheReplyQueue covers the cost of carrying a question across a reconnect.
//
// A reconnecting client re-offers questions it was handed and has not answered, and the server records them as
// freshly asked. takeReadyLocked holds every later reply behind an unanswered question, so a re-offered question
// that will never be answered delays whatever queues behind it. That is the documented price: one requestTimeout
// of delay and then it goes.
//
// The failure this rules out is the queue not draining at all. A re-offered question that the sweeper never
// expires would hold every reply on the session forever, which is far worse than the lost answer it was added to
// prevent, and the symptom would be a program hanging on an unrelated query.
//
// The clock is injected rather than waited out, so the boundary is exact instead of a sleep the suite pays for.
func TestAReofferedQuestionCannotWedgeTheReplyQueue(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "reoffer",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// A movable clock, guarded, because the sweeper reads it from its own goroutine while this test advances
	// it. The other clock-injecting tests here freeze a value and never reassign it, so they do not need this;
	// -race reported the unguarded version immediately.
	clock := &testClock{at: time.Now()}
	sess.mu.Lock()
	sess.clock = clock.now
	sess.mu.Unlock()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// A question re-offered by a client that reconnected, which this server has no other record of.
	sess.reofferQueries(att.token, [][]byte{[]byte("\x1b]11;?\x07")})
	if !sess.awaitingReply(att.token) {
		t.Fatal("the re-offered question was not recorded, so nothing below is being tested")
	}

	// The emulator answers something of its own, which has to queue behind the outstanding question to keep
	// the program's answers in the order it asked for them.
	sess.queueOrWriteReply([][]byte{[]byte("\x1b[12;34R")})

	// Observed through the shell's echo, in caret notation, which is how the other tests here see a pty write.
	// Waited out rather than checked once: "has not arrived yet" needs time to mean anything.
	if got := awaitStream(t, sess, "^[[12;34R", 700*time.Millisecond); strings.Contains(got, "^[[12;34R") {
		t.Fatalf("a local reply reached the pty while a question was outstanding, so ordering is not being "+
			"held and the release below would prove nothing; stream was %q", got)
	}

	// Past the expiry. The sweeper abandons the question and releases what queued behind it.
	clock.advance(requestTimeout + time.Millisecond)
	sess.sweepRequests()

	if sess.awaitingReply(att.token) {
		t.Error("the question is still outstanding after its expiry, so every later reply on this session " +
			"stays queued behind a client that is never going to answer")
	}
	if got := awaitStream(t, sess, "^[[12;34R", 3*time.Second); !strings.Contains(got, "^[[12;34R") {
		t.Errorf("the queued reply was never released after the question expired, so the reply queue is "+
			"wedged behind a re-offered question: a program asking anything else on this session waits "+
			"forever; stream was %q", got)
	}
}

// testClock is a clock a test can move while a background goroutine reads it.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}
