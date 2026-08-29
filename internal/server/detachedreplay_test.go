package server

import (
	"github.com/chancez/cm/internal/seq"
	"strings"
	"testing"
	"time"
)

// A query cm answered while detached must not be answered again by a client that resumes from behind it.
//
// This was a known, *unfixed* gap under the previous design, recorded there as a skipped reproduction:
// cm answered a query with nothing attached, the query stayed in the client log, and whenever a client
// later resumed from a position behind it, that client was served the question and its terminal answered
// a second time. Same visible symptom as the restart bug, a stray reply printed by the line editor, but
// reached without any restart.
//
// The proxy design closes it rather than mitigating it, and the reason is worth stating because it is the
// argument for that design in one sentence: clients do not answer. A reply arriving from a client matches
// no request cm made, so it is discarded, and no amount of replaying a query out of the log can produce a
// second answer.
//
// Kept as a test rather than deleted along with the gap. It is the case that motivated the whole
// redesign, and an assertion that it stays closed is worth more than the note that it once was open.
func TestDetachedAnswerIsNotReplayedToALaterClient(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("detachedreplay",
		`printf 'BEFORE\n'; printf 'A\033[6nB'; printf 'AFTER\n'; sleep 5`))

	term := &fakeTerminal{restore: []byte("R"), answers: cursorReport}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// Nothing is attached, so cm answers: the detached case working as designed. Waiting for the echoed
	// reply proves the answer really reached the pty before the assertion below, rather than racing it.
	stream := awaitStream(t, sess, echoedCursorReport, 5*time.Second)
	if !strings.Contains(stream, echoedCursorReport) {
		t.Fatalf("cm did not answer while detached; stream was %q.\n"+
			"That is the premise of this test rather than the bug: with nobody attached cm must answer.", stream)
	}

	// A client attaches later and resumes from before the query, which is what a reconnect after any
	// interruption does. It is served the question, because cm forwards output verbatim.
	var resume seq.Log
	att, err := sess.attach(&resume, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	got := readUntil(t, att.reader, "AFTER")
	if !strings.Contains(got, "\x1b[6n") {
		t.Fatalf("the resuming client was not served the query; it received %q.\n"+
			"It should be: cm forwards output verbatim, and suppressing the query is the reverted strip. "+
			"If the query is absent this test cannot show that answering it again is harmless.", got)
	}

	// Its terminal answers, exactly as a real one would on seeing a query it was never asked.
	sess.answerFromClient(att.token, []byte("\x1b[2;1R"))

	// That answer must not reach the pty. cm asked this client nothing, so the reply belongs to no
	// question and would be a second answer to one the emulator already gave.
	//
	// Counted rather than checked for presence, because cm's own legitimate answer is already in the
	// stream from before the client attached. One occurrence is correct; two is the bug.
	after := awaitStream(t, sess, "NEVERAPPEARS", time.Second)
	if n := strings.Count(after, echoedCursorReport); n != 1 {
		t.Errorf("the pty carries %d cursor reports, want exactly 1; stream was %q.\n"+
			"cm answered this query while detached. The resuming client's terminal answered it too, and "+
			"that reply matches no request cm made, so it must be discarded. Forwarding it is how a stray "+
			"reply reached the shell's line editor.", n, after)
	}
}
