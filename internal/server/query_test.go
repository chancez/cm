package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/shim"
)

// cursorReport makes a fakeTerminal answer a cursor position request the way libghostty does. The
// exact reply does not matter, only that it is recognizable in the pty stream.
var cursorReport = map[string]string{"\x1b[6n": "\x1b[2;1R"}

// echoedCursorReport is how cursorReport's reply appears once the pty has echoed it back.
//
// Caret notation, not the raw bytes. A pty in its default mode has `echoctl` set, so it renders a
// control character as ^X rather than passing it through: an injected "\x1b[2;1R" comes back as
// "^[[2;1R". Searching the stream for the raw sequence therefore never matches, and a test asserting
// absence passes no matter what cm does. That failure mode has bitten this repo before, which is why
// the two forms are named separately here instead of one constant being reused for both directions.
const echoedCursorReport = "^[[2;1R"

// awaitStream accumulates a session's output until want appears or the timeout expires, and returns
// everything seen.
//
// Unlike readUntil it does not fail on timeout, because the tests here assert both that something
// does appear and that something does not. A helper that failed on absence could only express half
// of that, and the half it could not express is the regression.
//
// Reads the session's own log rather than an attachment, so it observes what the pty produced
// including bytes echoed back after being written to it. That echo is how an unread reply became
// visible as garbage at the prompt, so it is the right signal to assert on.
func awaitStream(t *testing.T, sess *Session, want string, timeout time.Duration) string {
	t.Helper()

	sub := sess.recent.Subscribe(0)
	defer sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var sb strings.Builder
	for {
		c, err := sub.Next(ctx)
		if err != nil {
			return sb.String()
		}
		sb.Write(c.Data)
		if strings.Contains(sb.String(), want) {
			return sb.String()
		}
	}
}

// While a client is attached, cm must not inject its own answers into the pty.
//
// The regression test for the bug behind "exiting vim leaves garbage below my prompt" and
// ";rgb:2828/2c2c/3434 after wallfacer -h". Both had the same cause: two answerers on one pty. cm's
// emulator answered a query and wrote the reply to the pty while the real terminal was also
// answering, so some program's read consumed a reply addressed to nobody in particular. The reply
// that was left over ended up in the shell's line editor.
//
// Written against the pty rather than by running vim or wallfacer, because the bug is about which
// bytes reach the pty and does not depend on either program. A test that shelled out to a specific
// binary would also silently stop testing anything the day that binary changed its startup probes.
func TestNoAnswerInjectedWhileClientAttached(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "attached",
		// The shell emits a cursor position request, which is what a prompt hook does on every
		// prompt. MARK bounds the wait so the assertion is not racing delivery.
		Command: []string{"/bin/sh", "-c", `printf 'A\033[6nB'; printf 'MARK\n'; sleep 5`},
		Rows:    24, Cols: 80,
	})

	term := &fakeTerminal{restore: []byte("RESTORED"), answers: cursorReport}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	got := readUntil(t, att.reader, "MARK")

	// The query must reach the client, since the attached terminal is now the thing that answers it.
	if !strings.Contains(got, "\x1b[6n") {
		t.Errorf("client received %q, want it to contain the query \\x1b[6n.\n"+
			"With a client attached the real terminal is the answerer, so suppressing the query would "+
			"leave nothing to answer it and the program that asked would hang.", got)
	}

	// And cm must not have answered it. The emulator still generates a reply; the point is that the
	// reply is not written to the pty while someone else will answer.
	//
	// Waiting for the reply and expecting not to find it, rather than checking once, so this cannot
	// pass merely by looking before the write would have happened.
	if got := awaitStream(t, sess, echoedCursorReport, 2*time.Second); strings.Contains(got, echoedCursorReport) {
		t.Errorf("the pty received cm's own cursor report while a client was attached; stream was %q.\n"+
			"Two answerers on one pty is the defect: an injected reply can land in the middle of an "+
			"unrelated program's read, which is how the terminal's own reply ended up printed at the "+
			"prompt.", got)
	}
}

// With no client attached, cm must answer, or a program that queries the terminal hangs forever.
//
// The other half of the condition, and the more dangerous one to get wrong: the failure is a hang
// rather than a cosmetic artifact. `cm run` and a detached session both live here.
func TestAnswerInjectedWhenNoClientAttached(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "detached",
		Command: []string{"/bin/sh", "-c", `printf 'A\033[6nB'; printf 'MARK\n'; sleep 5`},
		Rows:    24, Cols: 80,
	})

	term := &fakeTerminal{restore: []byte("RESTORED"), answers: cursorReport}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// Subscribing to the log rather than attaching, so the session genuinely has no client. An
	// attach would be the case the previous test covers.
	sub := sess.recent.Subscribe(0)
	defer sub.Close()
	readUntil(t, sub, "MARK")

	// The answer must reach the shell. Observed through the pty echoing it back into the output
	// stream, which is both a real signal and exactly how the bug became visible in the first place:
	// an unread reply appears in the session's own output.
	if got := awaitStream(t, sess, echoedCursorReport, 5*time.Second); !strings.Contains(got, echoedCursorReport) {
		t.Errorf("cm's cursor report never reached the pty; stream was %q.\n"+
			"With nobody attached, cm is the only possible answerer. Staying silent means a program "+
			"that queried the terminal waits for a reply that never comes, which is a hang rather "+
			"than an artifact.", got)
	}
}

// A read-only follower cannot answer, so it must not count as an answerer.
//
// `cm read --follow` and `cm attach --read-only` drop their input (see recvLoop), so a terminal
// behind one never reaches the shell. Counting one would make cm go silent while nothing else
// answered, turning a normal thing to be doing into a hang.
func TestReadOnlyFollowerDoesNotSuppressAnswers(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "follower",
		Command: []string{"/bin/sh", "-c", `printf 'A\033[6nB'; printf 'MARK\n'; sleep 5`},
		Rows:    24, Cols: 80,
	})

	term := &fakeTerminal{restore: []byte("RESTORED"), answers: cursorReport}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	// What the RPC layer does for a read-only client, immediately after attaching.
	sess.markReadOnly(att.token)

	readUntil(t, att.reader, "MARK")

	if got := awaitStream(t, sess, echoedCursorReport, 5*time.Second); !strings.Contains(got, echoedCursorReport) {
		t.Errorf("cm's cursor report never reached the pty with only a follower attached; stream was "+
			"%q.\nA follower's input is dropped, so its terminal cannot answer. cm must still answer "+
			"or the querying program hangs.", got)
	}
}

// hasAnsweringClient is the predicate the whole fix rests on, so it is asserted directly rather than
// only through the three behavioral tests above.
func TestHasAnsweringClient(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "predicate",
		Command: []string{"/bin/sh", "-c", "sleep 5"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	if sess.hasAnsweringClient() {
		t.Error("hasAnsweringClient() = true with nothing attached, want false")
	}

	interactive, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	if !sess.hasAnsweringClient() {
		t.Error("hasAnsweringClient() = false with an interactive client attached, want true")
	}

	follower, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	sess.markReadOnly(follower.token)
	if !sess.hasAnsweringClient() {
		t.Error("hasAnsweringClient() = false with one interactive and one follower, want true: " +
			"the interactive client still answers")
	}

	// Only the follower left, which cannot answer.
	sess.detach(interactive)
	if sess.hasAnsweringClient() {
		t.Error("hasAnsweringClient() = true with only a read-only follower attached, want false: " +
			"a follower's input is dropped, so nothing would answer a query")
	}

	sess.detach(follower)
	if sess.hasAnsweringClient() {
		t.Error("hasAnsweringClient() = true after everything detached, want false")
	}
}

// Exactly one attached client answers a terminal query, however many are attached.
//
// Output fans out to every client, so two attached terminals both see a query and both reply. The
// shell then gets two answers to one question: measured against a real kitty with two clients on one
// session, a single CSI c came back as "\x1b[?62;52;c\x1b[?62;52;c". A program reads one and the spare
// is left for the shell's line editor, which is the same visible artifact as cm answering alongside a
// terminal, with a different second answerer.
//
// Asserted on the predicate rather than by driving two real terminals, because what the server
// controls is which attachment is permitted to forward a reply. The e2e test covers the wiring.
func TestOnlyOneClientAnswersQueries(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "answerer",
		Command: []string{"/bin/sh", "-c", "sleep 5"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	first, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("first attach() error = %v", err)
	}
	second, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("second attach() error = %v", err)
	}

	// The oldest attachment answers. Which one matters less than that it is exactly one and that it
	// does not move between queries.
	if !sess.isAnswerer(first.token) {
		t.Error("the first attachment is not the answerer, want it to be: the oldest client answers")
	}
	if sess.isAnswerer(second.token) {
		t.Error("the second attachment is also an answerer, want only one.\n" +
			"Two clients forwarding replies means the shell gets two answers to one question, and the " +
			"one the program does not consume is printed by its line editor.")
	}

	// Stable across repeated checks, so a program asking twice is answered by the same terminal and a
	// drop decision cannot flip between queries.
	for i := 0; i < 20; i++ {
		if !sess.isAnswerer(first.token) || sess.isAnswerer(second.token) {
			t.Fatalf("the answerer moved between checks on iteration %d.\n"+
				"Picking by map iteration order would do this, and an unstable pick lets a duplicate "+
				"reply through whenever it changes.", i)
		}
	}

	// When the answerer leaves, the remaining client takes over, or nothing answers at all.
	sess.detach(first)
	if !sess.isAnswerer(second.token) {
		t.Error("after the answerer detached the remaining client is not the answerer, want it to be.\n" +
			"Otherwise a query has no answerer while a terminal is still attached, and the program " +
			"that asked hangs.")
	}

	sess.detach(second)
	if sess.hasAnsweringClient() {
		t.Error("hasAnsweringClient() = true with nothing attached, want false")
	}
}

// A read-only follower is never the answerer, even when it attached first.
//
// Its input is dropped, so electing it would answer nothing while cm stayed silent because a client
// looked attached. That is a hang rather than an artifact.
func TestReadOnlyFollowerIsNeverTheAnswerer(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "followerfirst",
		Command: []string{"/bin/sh", "-c", "sleep 5"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// The follower attaches first, so an implementation that simply took the oldest would pick it.
	follower, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("follower attach() error = %v", err)
	}
	sess.markReadOnly(follower.token)

	interactive, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("interactive attach() error = %v", err)
	}

	if sess.isAnswerer(follower.token) {
		t.Error("a read-only follower is the answerer, want the interactive client.\n" +
			"A follower's input is dropped, so it can never deliver a reply.")
	}
	if !sess.isAnswerer(interactive.token) {
		t.Error("the interactive client is not the answerer, want it to be")
	}
}
