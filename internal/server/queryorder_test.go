package server

import (
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/shim"
)

// A reply cm can produce immediately must wait behind a question still out with a client.
//
// This is the ordering guarantee, and it is the mechanism the `wallfacer -h` corruption needed. That
// incident: wallfacer sent OSC 11 and blocked reading the background colour, which only a real terminal
// can answer. Meanwhile a zsh prompt hook sent CSI 6n, cm's emulator answered it, and cm wrote the cursor
// report to the pty while wallfacer was mid-read. wallfacer consumed it as though it were its own answer
// and exited; the terminal's real OSC 11 reply then arrived with nobody waiting and the line editor
// printed it as ";rgb:2828/2c2c/3434".
//
// A program asks its questions in order down one pty and reads the answers in order. cm answering the
// second question first breaks that, whatever the queries are, so the fix is not about OSC 11 specifically:
// any locally-answerable reply must queue behind any outstanding proxied one.
//
// Asserted on what reaches the pty rather than end to end through a real terminal, because the property is
// about write ordering in the server and a real terminal would answer in microseconds, making the window
// this protects almost impossible to hit deliberately.
func TestLocalReplyWaitsForAnOutstandingProxiedQuery(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "ordering",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})

	// Deliberately *not* configured with `answers`, unlike most tests here. A fakeTerminal with answers
	// replies to any Write containing the query, and the pty echoes cm's reply back into the output stream,
	// which the pump feeds to the model again: the fixture then answers its own echo forever. The first
	// version of this test used one and saw a cursor report in the stream that no code path under test had
	// produced, which read as an ordering failure. The reply is injected explicitly below instead.
	term := &fakeTerminal{restore: []byte("R")}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// The program asks the background colour first. Only the client can answer, so this becomes an
	// outstanding request.
	sess.noteQueries([]byte("\x1b]11;?\x07"))
	select {
	case <-att.queries:
	case <-time.After(2 * time.Second):
		t.Fatal("the client was never asked the OSC 11 query, so the ordering cannot be tested")
	}

	// Then it asks the cursor position, which cm can answer itself. That reply must not overtake the
	// OSC 11 answer the program is still waiting for.
	sess.queueOrWriteReply([][]byte{[]byte("\x1b[2;1R")})

	// Checked well inside requestTimeout, which matters and was wrong first. Waiting 700ms against a 500ms
	// timeout let the sweeper legitimately expire the question and release the reply, and the test read that
	// as an ordering failure. The window has to be shorter than the timeout, or this asserts the opposite of
	// what it means to.
	if got := awaitStream(t, sess, echoedCursorReport, requestTimeout/2); strings.Contains(got, echoedCursorReport) {
		t.Errorf("cm's cursor report reached the pty while an OSC 11 question was still out; stream was %q.\n"+
			"The program asked the background colour first and is blocked reading that answer, so it will "+
			"consume this cursor report as though it were the colour. That is the recorded `wallfacer -h` "+
			"corruption, which ended with \";rgb:2828/2c2c/3434\" printed beside a prompt.", got)
	}

	// Once the client answers, both replies go out, in the order the program asked.
	sess.answerFromClient(att.token, []byte("\x1b]11;rgb:2828/2c2c/3434\x07"))

	got := awaitStream(t, sess, echoedCursorReport, 3*time.Second)
	colour := strings.Index(got, "]11;rgb:2828/2c2c/3434")
	cursor := strings.Index(got, echoedCursorReport)
	if colour < 0 || cursor < 0 {
		t.Fatalf("both replies should have reached the pty; stream was %q", got)
	}
	if colour > cursor {
		t.Errorf("the cursor report was written before the colour reply; stream was %q.\n"+
			"The program asked the colour first, so it must be answered first.", got)
	}
}

// A question a client never answers must not hold later replies forever.
//
// The bound is what keeps one unanswerable question from wedging a session. A terminal that does not
// implement OSC 52, or a client whose window has gone while its connection has not noticed, simply never
// replies, and everything queued behind it would wait indefinitely without an expiry.
//
// Driven through the injectable clock rather than by sleeping past requestTimeout, so the test is
// deterministic and fast. Sleeping would make it slow and would turn a timing assertion into a race.
func TestAnUnansweredProxiedQueryExpires(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "expiry",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})

	term := &fakeTerminal{restore: []byte("R"), answers: cursorReport}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// A clock the test advances by hand.
	now := time.Now()
	sess.mu.Lock()
	sess.clock = func() time.Time { return now }
	sess.mu.Unlock()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	sess.noteQueries([]byte("\x1b]11;?\x07"))
	select {
	case <-att.queries:
	case <-time.After(2 * time.Second):
		t.Fatal("the client was never asked the query")
	}

	// A locally-answerable reply queues behind it, and the client never answers.
	sess.queueOrWriteReply([][]byte{[]byte("\x1b[2;1R")})

	sess.mu.Lock()
	queued := len(sess.requests)
	sess.mu.Unlock()
	if queued != 2 {
		t.Fatalf("%d requests queued, want 2: the question and the reply behind it", queued)
	}

	// Past the timeout, the sweep releases what was held.
	now = now.Add(requestTimeout + time.Millisecond)
	sess.sweepRequests()

	if got := awaitStream(t, sess, echoedCursorReport, 3*time.Second); !strings.Contains(got, echoedCursorReport) {
		t.Errorf("the queued cursor report never reached the pty after the question expired; stream was %q.\n"+
			"An unanswerable question must release what is behind it, or one terminal that does not "+
			"implement a query wedges every later query on the session.", got)
	}

	sess.mu.Lock()
	left := len(sess.requests)
	sess.mu.Unlock()
	if left != 0 {
		t.Errorf("%d requests left after the sweep, want 0", left)
	}
}

// A client detaching releases anything it was asked, rather than leaving it to expire.
//
// Waiting for the timeout would hold every reply behind a question for up to requestTimeout after the only
// client that could have answered it has gone, which is latency for no possible benefit.
func TestDetachReleasesOutstandingQueries(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "detachrelease",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})

	term := &fakeTerminal{restore: []byte("R"), answers: cursorReport}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}

	sess.noteQueries([]byte("\x1b]11;?\x07"))
	select {
	case <-att.queries:
	case <-time.After(2 * time.Second):
		t.Fatal("the client was never asked the query")
	}
	sess.queueOrWriteReply([][]byte{[]byte("\x1b[2;1R")})

	// The client goes away without answering.
	sess.detach(att)

	if got := awaitStream(t, sess, echoedCursorReport, 3*time.Second); !strings.Contains(got, echoedCursorReport) {
		t.Errorf("the queued reply never reached the pty after the client detached; stream was %q.\n"+
			"Nothing can answer a question put to a client that has gone, so it must be released at "+
			"detach rather than held until the timeout.", got)
	}
}

// An unsolicited reply from a client is discarded.
//
// The case the whole design exists to make safe, and the one that produced the reported symptom. A client
// reconnecting after a server restart is served the backlog from the log, which contains queries cm already
// answered; its terminal answers them again. Those replies match no outstanding request, so they must not
// reach the pty. Forwarding them is what typed a git branch name into the prompt.
func TestUnsolicitedClientReplyIsDiscarded(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "unsolicited",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// Nothing was asked of this client, and it answers anyway: a title report carrying a branch name,
	// which is the shape of the reported symptom.
	const branchReport = "\x1b]l~/p/w/h/.w/integrate_dex (pr/chancez/integrate_dex)\x1b\\"
	sess.answerFromClient(att.token, []byte(branchReport))

	if got := awaitStream(t, sess, "integrate_dex", 700*time.Millisecond); strings.Contains(got, "integrate_dex") {
		t.Errorf("an unsolicited client reply reached the pty; stream was %q.\n"+
			"cm asked nothing, so this answers no question and the shell's line editor prints it. That is "+
			"how a branch name from a title report was typed into the commandline after a server restart.", got)
	}
}
