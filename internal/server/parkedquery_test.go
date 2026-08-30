package server

import (
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/shim"
)

// A question asked with no client attached waits for one and is then asked.
//
// This is the window at session creation: the shim starts the program as soon as the session exists, and the
// attach that created it is still completing. Dropping the question there loses it entirely, because the bytes
// predate the client's attachment and a restore blob carries a screen rather than a query, so the client never
// sees them and cannot answer of its own accord.
func TestAParkedQueryIsAskedWhenAClientArrives(t *testing.T) {
	sess := sessionWithNoClient(t)

	// Asked with nothing attached.
	sess.noteQueries([]byte("\x1b]11;?\x07"))

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	sess.askParkedQueries(att.token)

	select {
	case got := <-att.queries:
		if string(got) != "\x1b]11;?\x07" {
			t.Errorf("the client was asked %q, want the question the program asked", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the client was never asked the question the program asked before it attached, so a program " +
			"that queries at startup never hears back")
	}
	if !sess.awaitingReply(att.token) {
		t.Error("the question is not recorded against the client that was asked, so its reply would match " +
			"nothing and be discarded as unsolicited")
	}
}

// The number parked is bounded, or a session nobody attaches to holds one per query, each of them holding the
// reply queue behind it.
func TestParkedQueriesAreBounded(t *testing.T) {
	sess := sessionWithNoClient(t)

	for range maxParkedQueries + 3 {
		sess.noteQueries([]byte("\x1b]11;?\x07"))
	}

	sess.mu.Lock()
	parked := 0
	for _, r := range sess.requests {
		if r.proxied && r.tok == nil {
			parked++
		}
	}
	sess.mu.Unlock()

	if parked != maxParkedQueries {
		t.Errorf("%d questions are parked, want at most %d: an unattached session would otherwise accumulate "+
			"one per query", parked, maxParkedQueries)
	}
}

// A parked question expires on the ordinary sweep, so a session nobody ever attaches to drains rather than
// holding every later reply forever. That was the outcome before parking existed, and it still has to be the
// outcome here.
func TestAParkedQueryExpiresIfNobodyAttaches(t *testing.T) {
	sess := sessionWithNoClient(t)

	clock := &testClock{at: time.Now()}
	sess.mu.Lock()
	sess.clock = clock.now
	sess.mu.Unlock()

	sess.noteQueries([]byte("\x1b]11;?\x07"))

	// A reply the emulator produced, which has to wait behind the parked question to keep the program's
	// answers in order.
	sess.queueOrWriteReply([][]byte{[]byte("\x1b[12;34R")})
	if got := awaitStream(t, sess, "^[[12;34R", 700*time.Millisecond); strings.Contains(got, "^[[12;34R") {
		t.Fatalf("a local reply reached the pty while a question was parked, so ordering is not held and the "+
			"release below proves nothing; stream was %q", got)
	}

	clock.advance(requestTimeout + time.Millisecond)
	sess.sweepRequests()

	if got := awaitStream(t, sess, "^[[12;34R", 3*time.Second); !strings.Contains(got, "^[[12;34R") {
		t.Errorf("the queued reply was never released after the parked question expired, so a session nobody "+
			"attaches to wedges its own reply queue; stream was %q", got)
	}
}

// sessionWithNoClient builds a session with a real shim and nothing attached, which is the state a session is
// in for the moments between being created and its first client becoming able to answer.
func sessionWithNoClient(t *testing.T) *Session {
	t.Helper()
	rec := startShimFor(t, shim.Config{
		Session: "parked",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}
