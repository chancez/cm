package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAQueryAskedBeforeAClientCanAnswerIsStillAnswered covers the window at session creation.
//
// A session created by an attach starts its program while that attach is still completing. proxyQuery picks a
// client to ask from those already eligible, and returns silently when there are none, so a program that
// queries in that window has its question dropped and never hears back. A TUI asking for the background colour
// on startup does exactly this.
//
// It was found rather than predicted: an earlier test of the restart case never saw a query reach the pty at
// all until a sleep was put in front of it, which is the shape of the problem in one observation.
//
// Made an ordering rather than a race by holding the client outside the answerer set until the program has
// asked. Without the fault point this needs the program to win a millisecond-wide race, which is the kind of
// test that passes on a fast machine and reports nothing.
func TestAQueryAskedBeforeAClientCanAnswerIsStillAnswered(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty")
	}

	gate := filepath.Join(t.TempDir(), "release")
	e := newEnvWith(t, cmHooksBinary(t), "",
		"CM_TESTHOOK_FAULTS=before-client-can-answer:pause="+gate)

	// Asks immediately, with nothing between the session starting and the question. The client is held before
	// it can answer, so the question is asked into a session with no eligible answerer.
	const script = `printf '\033]11;?\007'; IFS= read -r reply; printf 'GOT[%s]\r\n' "$reply"`

	c := attachOnPty(t, e, "qstartup", "--", "/bin/sh", "-c", script)

	// Long enough for the program to have asked, and short of the 500ms a parked question is given before the
	// ordinary sweep abandons it. Spawning a shell and running one printf is a few milliseconds, so 250ms is
	// ample; a longer pause would expire the question and test the abandonment rather than the delivery.
	time.Sleep(250 * time.Millisecond)
	release(t, gate)

	// The client is now attached and its terminal answers, as it would have as soon as it saw the question.
	waitForOnPty(t, c, "\x1b]11;?")
	answerBackgroundColour(t, c)

	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(c.output(), "GOT[") {
		if time.Now().After(deadline) {
			t.Fatalf("the program never got past its read:\npty:\n%s", c.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := c.output(); !strings.Contains(got, "rgb:") {
		t.Errorf("the program read an empty answer, so its question was dropped for arriving before any "+
			"client could answer it. A program that queries at startup, which is when a TUI asks for the "+
			"background colour, races the attach that created the session.\npty:\n%s", got)
	}
}
