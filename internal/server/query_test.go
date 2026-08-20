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

// cm answers a query its model can answer, whether or not a client is attached.
//
// This is the behavior the proxy design establishes, and it is the *reverse* of what this file asserted
// before. The old rule was that cm stayed silent whenever an attached client could answer, so that one
// question got one answer; the answer came from the client's terminal and cm forwarded it.
//
// That rule needed an election, and the election was wrong in four distinct ways, each of which shipped:
// a read-only follower elected meant nothing answered; a reserved-but-unattached client elected meant the
// same; two attached clients meant a single CSI c came back doubled as "\x1b[?62;52;c\x1b[?62;52;c"; and
// after a server restart cm answered a query from the backlog which the reconnecting client then answered
// again from the log, typing a git branch name into the prompt.
//
// So cm answers these itself, always, and a client's own reply to such a query is discarded because cm
// never asked for it. One question, one answer, with no state to get wrong. Only the queries cm genuinely
// cannot answer are put to a client, and those are matched to a request.
func TestCMAnswersItsOwnQueriesWhateverIsAttached(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, sess *Session) func()
	}{
		{
			name:  "nothing attached",
			setup: func(t *testing.T, sess *Session) func() { return func() {} },
		},
		{
			name: "one interactive client",
			setup: func(t *testing.T, sess *Session) func() {
				att, err := sess.attach(nil, nil)
				if err != nil {
					t.Fatalf("attach() error = %v", err)
				}
				return func() { sess.detach(att) }
			},
		},
		{
			name: "a read-only follower",
			setup: func(t *testing.T, sess *Session) func() {
				att, err := sess.attach(nil, nil)
				if err != nil {
					t.Fatalf("attach() error = %v", err)
				}
				sess.markReadOnly(att.token)
				return func() { sess.detach(att) }
			},
		},
		{
			name: "a reservation that has not attached",
			setup: func(t *testing.T, sess *Session) func() {
				tok := sess.reserveClient()
				sess.registerClientSize(tok, 40, 120, 0, 0, false)
				return func() { sess.releaseClient(tok) }
			},
		},
		{
			name: "two interactive clients",
			setup: func(t *testing.T, sess *Session) func() {
				first, err := sess.attach(nil, nil)
				if err != nil {
					t.Fatalf("first attach() error = %v", err)
				}
				second, err := sess.attach(nil, nil)
				if err != nil {
					t.Fatalf("second attach() error = %v", err)
				}
				return func() { sess.detach(first); sess.detach(second) }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := startShimFor(t, shim.Config{
				Session: "answers",
				// A cursor position request, which is what a zsh prompt hook sends on every prompt. MARK
				// bounds the wait so the assertion is not racing delivery.
				Command: []string{"/bin/sh", "-c", `printf 'A\033[6nB'; printf 'MARK\n'; sleep 5`},
				Rows:    24, Cols: 80,
			})

			term := &fakeTerminal{restore: []byte("RESTORED"), answers: cursorReport}
			sess, err := newSession(rec, term, 0, 0)
			if err != nil {
				t.Fatalf("newSession() error = %v", err)
			}
			defer sess.Close()

			cleanup := tc.setup(t, sess)
			defer cleanup()

			// Observed through the pty echoing the reply back into the output stream, which is both a real
			// signal and how the original bug became visible.
			got := awaitStream(t, sess, echoedCursorReport, 5*time.Second)
			if !strings.Contains(got, echoedCursorReport) {
				t.Errorf("cm's cursor report never reached the pty; stream was %q.\n"+
					"cm answers what its model can answer regardless of what is attached, because it is "+
					"the only writer of a reply to this pty. Staying silent leaves the querying program "+
					"waiting for an answer that never comes, which is a hang rather than an artifact.", got)
			}
		})
	}
}

// A query cm cannot answer goes to exactly one attached client.
//
// The proxied half. Output fans out to every client, so a question put into the output stream would be
// asked of every attached terminal and each would answer: measured previously with two clients on one
// session, a single CSI c came back as "\x1b[?62;52;c\x1b[?62;52;c". A question is therefore addressed to
// one client through its own channel, which is also why it never appears in the session's scrollback.
func TestTerminalOnlyQueryGoesToOneClient(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "proxyone",
		Command: []string{"/bin/sh", "-c", "sleep 5"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	first, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("first attach() error = %v", err)
	}
	defer sess.detach(first)
	second, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("second attach() error = %v", err)
	}
	defer sess.detach(second)

	// OSC 11 asks the background colour, which only a real terminal knows.
	sess.noteQueries([]byte("\x1b]11;?\x07"))

	got := 0
	for _, att := range []attachment{first, second} {
		select {
		case q := <-att.queries:
			got++
			if string(q) != "\x1b]11;?\x07" {
				t.Errorf("client received %q, want the query verbatim", q)
			}
		default:
		}
	}
	if got != 1 {
		t.Errorf("%d of 2 clients were asked the query, want exactly 1.\n"+
			"Asking several means the shell receives several answers to one question, and the spare is "+
			"printed by the line editor.", got)
	}
}

// A read-only follower is never asked, because it cannot answer.
//
// Its input is dropped on the way back (see recvLoop), so a question put to one is guaranteed to expire.
// That is not merely wasteful: everything queued behind it waits for the timeout, so one follower would
// add latency to every later query on the session.
func TestReadOnlyFollowerIsNeverAsked(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "followernotasked",
		Command: []string{"/bin/sh", "-c", "sleep 5"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// The follower attaches first, so an implementation picking by attach order alone would choose it.
	follower, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("follower attach() error = %v", err)
	}
	defer sess.detach(follower)
	sess.markReadOnly(follower.token)

	interactive, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("interactive attach() error = %v", err)
	}
	defer sess.detach(interactive)

	sess.noteQueries([]byte("\x1b]11;?\x07"))

	select {
	case q := <-follower.queries:
		t.Errorf("a read-only follower was asked %q, want the interactive client asked instead.\n"+
			"A follower's input is dropped, so it can never answer and the question would expire, "+
			"holding every reply queued behind it for the timeout.", q)
	default:
	}
	select {
	case <-interactive.queries:
	default:
		t.Error("the interactive client was not asked the query, want it to be")
	}
}

// A reservation that has not attached is never asked.
//
// The same reasoning one step earlier: an entry made by reserveClient has no stream to carry the question.
// Service.Attach deliberately opens this window to resize the pty before snapshotting the screen, and the
// resize makes the shell redraw, so a query from a SIGWINCH handler lands in exactly this interval. Under
// the old election that window elected a client that could not answer and the query vanished.
func TestReservationIsNeverAsked(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "reservednotasked",
		Command: []string{"/bin/sh", "-c", "sleep 5"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	tok := sess.reserveClient()
	sess.registerClientSize(tok, 40, 120, 0, 0, false)
	defer sess.releaseClient(tok)

	// No client can answer, so nothing is asked and nothing is recorded as outstanding. Recording a
	// request nobody can answer would stall the reply queue for the timeout and achieve nothing.
	sess.noteQueries([]byte("\x1b]11;?\x07"))

	sess.mu.Lock()
	n := len(sess.requests)
	sess.mu.Unlock()
	if n != 0 {
		t.Errorf("%d requests outstanding with only a reservation present, want 0.\n"+
			"A reservation has no stream to carry a question, so registering one holds every later "+
			"reply behind a question that can never be answered.", n)
	}
}
