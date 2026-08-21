package server

import (
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/shim"
)

// A reply must answer the question cm asked, not merely arrive from the client cm asked.
//
// This is the reported `gh pr create --web` and `wallfacer sync` corruption: "^[[42;1R^[[42;1R" printed
// beside the prompt. Both use termenv, which probes the background colour with OSC 11 and immediately
// sends CSI 6n behind it as a sentinel, because a terminal that ignores OSC 11 still answers a cursor
// report and that is how termenv knows to stop reading.
//
// cm proxies the OSC 11, since only a real terminal knows the colour, and answers the CSI 6n from its own
// model. But the CSI 6n is forwarded to every client verbatim, deliberately (see the pump), so the
// client's real terminal answers it as well. That CPR arrives on the input path while the OSC 11 question
// is still outstanding, and matching on the token alone accepted it as the colour reply: cm wrote the
// terminal's CPR to the pty, then released its own CPR queued behind it, and discarded the real colour
// reply as unsolicited. Two cursor reports, no colour, and termenv had stopped reading by then, so the
// line editor printed both.
func TestReplyMustAnswerTheQuestionAsked(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "replymatch",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// A frozen clock, so the sweeper cannot expire the question while the absence below is being waited
	// out. Without this the wait for "no cursor report" has to be shorter than requestTimeout, and the
	// second half of this test then asserts nothing: the request would have expired legitimately and the
	// colour reply would be unsolicited for that reason rather than because of the matching. That is the
	// trap TestLocalReplyWaitsForAnOutstandingProxiedQuery records from the other direction.
	now := time.Now()
	sess.mu.Lock()
	sess.clock = func() time.Time { return now }
	sess.mu.Unlock()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// The program asks the background colour, which only the client can answer.
	sess.noteQueries([]byte("\x1b]11;?\x1b\\"))
	select {
	case <-att.queries:
	case <-time.After(2 * time.Second):
		t.Fatal("the client was never asked the OSC 11 query")
	}

	// The client's terminal answers the CSI 6n it saw in the output stream, not the question cm asked it.
	sess.answerFromClient(att.token, []byte("\x1b[42;1R"))

	if got := awaitStream(t, sess, "^[[42;1R", 700*time.Millisecond); strings.Contains(got, "^[[42;1R") {
		t.Errorf("a cursor report was accepted as the answer to an OSC 11 question; stream was %q.\n"+
			"cm answers CSI 6n from its own model, so this reply answers nothing cm asked and must be "+
			"discarded. Accepting it writes the client's cursor report to the pty and then releases cm's "+
			"own behind it, which is the reported \"^[[42;1R^[[42;1R\" beside the prompt.", got)
	}

	// The question is still outstanding, so the real colour reply still answers it.
	sess.answerFromClient(att.token, []byte("\x1b]11;rgb:2828/2c2c/3434\x1b\\"))

	if got := awaitStream(t, sess, "]11;rgb:2828/2c2c/3434", 3*time.Second); !strings.Contains(got, "]11;rgb:2828/2c2c/3434") {
		t.Errorf("the colour reply never reached the pty; stream was %q.\n"+
			"Discarding a mismatched reply must not consume the request, or the program that asked the "+
			"colour waits out the timeout for an answer its terminal already sent.", got)
	}
}

// A reply chunk carrying an answer plus something unasked passes on only the answer.
//
// The half of the reported bug that matching alone does not fix, and it was found by driving the real
// termenv probe into a session with a real kitty attached rather than by reading the code. The client
// answered both queries it had seen in one write, "OSC 11 colour" immediately followed by the cursor report
// for the CSI 6n cm forwards to every client. The colour matched the outstanding question, so the whole
// blob was stored as the answer and the cursor report reached the pty inside it. On screen that was
// "^[]11;rgb:...^[\^[]11;rgb:...^[\^[[3;1R^[[3;1R": four replies for two questions.
func TestOnlyTheAnsweringSequenceIsTakenFromAReplyChunk(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "replysplit",
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

	sess.noteQueries([]byte("\x1b]11;?\x1b\\"))
	select {
	case <-att.queries:
	case <-time.After(2 * time.Second):
		t.Fatal("the client was never asked the OSC 11 query")
	}

	// One write carrying the answer and, behind it, the terminal's own answer to the CSI 6n it saw in the
	// forwarded output stream. Exactly what a real kitty sent.
	sess.answerFromClient(att.token, []byte("\x1b]11;rgb:2828/2c2c/3434\x1b\\\x1b[3;1R"))

	got := awaitStream(t, sess, "]11;rgb:2828/2c2c/3434", 3*time.Second)
	if !strings.Contains(got, "]11;rgb:2828/2c2c/3434") {
		t.Fatalf("the colour reply never reached the pty; stream was %q", got)
	}
	if strings.Contains(got, "^[[3;1R") {
		t.Errorf("a cursor report rode along with the colour reply; stream was %q.\n"+
			"A terminal answers several questions in one write, so each sequence has to be matched to its "+
			"own question. cm answers CSI 6n from its own model, so this one answers nothing cm asked and "+
			"must be dropped rather than carried to the pty by the reply beside it.", got)
	}
}
