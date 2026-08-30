package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestAQueryOutstandingAcrossARestartIsStillAnswered asks what happens to a question in flight when the
// server restarts.
//
// cm proxies a query only a real terminal can answer to one client and records it as outstanding, so the reply
// coming back can be matched to the question that asked. Those records are in memory on the Session and
// nothing persists or rebuilds them, so a restart forgets them. The query itself is not re-asked either:
// adoption resubscribes from where the old server stopped, so the bytes carrying it are in the past.
//
// That is what happens. The reply arrives at a server with nothing outstanding, answerFromClient discards it
// as unsolicited, and the program that asked gets nothing: the same hang the proxy exists to prevent, reached
// through a restart instead of a chunk boundary.
//
// The program here uses `read`, so it recovers on the newline the test sends afterwards and the loss shows up
// as an empty value rather than a hang. A program waiting only for its reply, which is what `wallfacer -h`
// does on OSC 11, does not recover.
//
// Deterministic rather than raced: the test owns the client's pty, so it decides when the terminal answers.
// The query goes out, the restart happens, and only then is the reply written.
func TestAQueryOutstandingAcrossARestartIsStillAnswered(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty, and restarts the server")
	}

	// The program asks for the background colour and waits for the answer on its stdin, which is where a
	// terminal's reply arrives.
	// The sleep before the query is load-bearing. proxyQuery does nothing when no client can answer yet, and
	// a session created by an attach starts its program while that attach is still completing, so a program
	// that queries immediately can find no answerer and wait forever. That is existing behaviour rather than
	// the bug under test, and without the pause it raced this test: the query never reached the pty at all.
	// The answer is echoed back rather than just acknowledged, and that is the difference between a test and
	// a decoration. `read` returns on the newline the test sends after the reply, whether or not the reply
	// itself arrived, so printing a bare marker passed with the reply discarded. Printing what was read makes
	// an empty value visible.
	const script = `printf 'ASKING\r\n'; sleep 1; printf '\033]11;?\007'; IFS= read -r reply; ` +
		`printf 'GOT[%s]\r\n' "$reply"`

	t.Run("without a restart", func(t *testing.T) {
		e := newEnv(t)
		c := attachOnPty(t, e, "qnorestart", "--", "/bin/sh", "-c", script)
		waitForOnPty(t, c, "ASKING")
		// The query has to reach this client's terminal, which is what makes it the one cm asked.
		waitForOnPty(t, c, "\x1b]11;?")

		answerBackgroundColour(t, c)

		// The control: without a restart the reply is matched and forwarded to the program. If this fails the
		// fixture is wrong and the restart case below proves nothing.
		waitForOnPty(t, c, "GOT[")
		if got := c.output(); !strings.Contains(got, "rgb:") {
			t.Fatalf("the program read no reply even without a restart, so the fixture cannot show one being "+
				"lost:\n%s", got)
		}
	})

	t.Run("across a restart", func(t *testing.T) {
		e := newEnv(t)
		c := attachOnPty(t, e, "qrestart", "--", "/bin/sh", "-c", script)
		waitForOnPty(t, c, "ASKING")
		waitForOnPty(t, c, "\x1b]11;?")

		// The server goes away while the question is outstanding, and comes back.
		e.restartServer()
		e.list()
		e.waitFor("the session to be adopted", 25*time.Second, func() bool {
			return e.sessionDetail(t, "qrestart").State == "running"
		})
		// The client reconnects on its own; wait for it so the reply has somewhere to go.
		e.waitFor("the client to reconnect", 25*time.Second, func() bool {
			return e.sessionDetail(t, "qrestart").Clients == 1
		})

		// Now the terminal answers, as it would have done all along.
		answerBackgroundColour(t, c)

		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(c.output(), "GOT[") {
			if time.Now().After(deadline) {
				t.Fatalf("the program never got past its read after the server restarted:\npty:\n%s",
					c.output())
			}
			time.Sleep(50 * time.Millisecond)
		}
		// The answer has to arrive. The client re-offers the question it was handed on its reconnect and the
		// server records it again, so the reply the terminal produced during the restart matches.
		//
		// The budget this rests on is deliberate rather than incidental: requestTimeout is 500ms and a restart
		// takes seconds, so a re-offered question is recorded as freshly asked. Without that, preserving the
		// record would change nothing, because the sweep would abandon it immediately. See
		// TestASlowAnswerIsDiscardedWithoutAnyRestart, which is the measurement that the budget still applies
		// on a connection that never dropped.
		if got := c.output(); !strings.Contains(got, "rgb:") {
			t.Errorf("the program read an empty answer after the restart, so its question was lost: the "+
				"reply reached a server with nothing outstanding and was discarded as unsolicited.\npty:\n%s",
				got)
		}
	})
}

// answerBackgroundColour writes what a real terminal would send for OSC 11, plus a newline so the shell's
// `read` returns.
//
// The newline is ordinary typing rather than part of the reply: the reply is matched and forwarded to the pty
// by cm, and `read` needs a line ending to stop waiting.
func answerBackgroundColour(t *testing.T, c *ptyClient) {
	t.Helper()
	c.write([]byte("\x1b]11;rgb:2828/2c2c/3434\x07"))
	time.Sleep(200 * time.Millisecond)
	c.write([]byte("\r"))
}

// TestASlowAnswerIsDiscardedWithoutAnyRestart isolates what actually governs a lost answer.
//
// requestTimeout is 500ms, and sweepRequests abandons a question older than that: the reply then matches
// nothing and is discarded exactly as it is after a restart. A restart takes seconds, so a query in flight when
// one happens was going to lose its answer to expiry regardless of whether the record survived.
//
// That matters for what "fix the restart case" would mean. Preserving the record across a restart changes
// nothing on its own; the question would have to be treated as fresh again, which is a change to the timeout
// policy rather than a repair. This test is the measurement that says so, and it is deliberately next to the
// restart one so the two are read together.
func TestASlowAnswerIsDiscardedWithoutAnyRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty")
	}

	const script = `printf 'ASKING\r\n'; sleep 1; printf '\033]11;?\007'; IFS= read -r reply; ` +
		`printf 'GOT[%s]\r\n' "$reply"`

	e := newEnv(t)
	c := attachOnPty(t, e, "qslow", "--", "/bin/sh", "-c", script)
	waitForOnPty(t, c, "ASKING")
	waitForOnPty(t, c, "\x1b]11;?")

	// Well past requestTimeout, and nothing else has gone wrong: no restart, no reconnect, one client that
	// simply took its time.
	time.Sleep(2 * time.Second)
	answerBackgroundColour(t, c)

	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(c.output(), "GOT[") {
		if time.Now().After(deadline) {
			t.Fatalf("the program never got past its read:\npty:\n%s", c.output())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Recorded as current behaviour. A reply later than requestTimeout is discarded, which the constant's own
	// comment defends: the asking program gets no answer to that query, which is what happened for every
	// terminal-only query before the proxy existed.
	if strings.Contains(c.output(), "rgb:") {
		t.Errorf("a reply arriving %s after the question was still delivered, so requestTimeout is not what "+
			"governs this and the restart case needs looking at on its own terms.\npty:\n%s",
			2*time.Second, c.output())
	}
}
