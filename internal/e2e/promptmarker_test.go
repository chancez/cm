package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/ansi"
)

// TestPromptMarkerSplitAcrossWritesStillGetsRedrawZero drives the split through a real pty.
//
// The shell writes the marker in two printfs, which is two writes to the pty and so two reads on the other
// side. That is the delivery that used to lose the rewrite: RewritePromptRedraw is stateless and passes an
// unterminated marker through unchanged, so the introducer went out unrewritten and nothing matched it
// afterwards.
//
// What the client then receives is a marker with no redraw parameter, which a terminal reads as redraw=1.
// It clears the prompt lines on the next resize and waits for the shell to repaint them, and through a
// multiplexer the repaint arrives in the pty's coordinates rather than the window's: the prompt is cleared
// and does not come back.
//
// Read from the client's own transcript rather than from `cm read`, because this is about the bytes a client
// receives. `cm read --raw` re-serializes the model, and the model is deliberately fed the pre-rewrite
// bytes, so it would show the marker as the shell wrote it either way and the test would prove nothing.
func TestPromptMarkerSplitAcrossWritesStillGetsRedrawZero(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty")
	}

	const done = "PROMPT-MARKER-DONE"

	// Split inside the introducer, which is the earliest boundary that loses it, with a sleep between the
	// halves so they cannot be coalesced into one read. Then a second marker split at a different offset,
	// since a terminated marker followed by the start of another is the case that broke the first version
	// of the holdback.
	script := strings.Join([]string{
		`printf 'first\r\n'`,
		`printf '\033]13'`,
		"sleep 0.3",
		`printf '3;A;redraw=1\007prompt-one$ '`,
		"sleep 0.3",
		`printf '\r\nsecond\r\n\033]133;A;redraw=1'`,
		"sleep 0.3",
		`printf '\007prompt-two$ '`,
		"sleep 0.3",
		`printf '\r\n` + done + `\r\n'`,
		"sleep 30",
	}, "; ")

	e := newEnvWith(t, cmHooksBinary(t), "")
	transcript := e.state + "/prompt.jsonl"

	c := attachOnPtyWithEnv(t, e,
		[]string{"CM_TESTHOOK_TRANSCRIPT=" + transcript},
		"promptmark", "--", "/bin/sh", "-c", script)

	waitForOnPty(t, c, done)
	time.Sleep(500 * time.Millisecond)

	got := string(ansi.SessionBytes(readTranscript(t, transcript)))

	// The control first: both markers reached the client at all. Without this the assertions below would be
	// satisfied by a stream containing no markers, which is how a test like this passes for the wrong
	// reason.
	if n := strings.Count(got, "\x1b]133;A"); n < 2 {
		t.Fatalf("the client received %d prompt markers, want 2: the fixture did not produce them, so "+
			"nothing below is being checked.\nstream: %q", n, got)
	}

	// Neither marker may carry redraw=1, and both must carry redraw=0. Checked as a pair because a marker
	// with the parameter dropped entirely also means redraw to a terminal, so "no redraw=1" alone is not
	// enough.
	if strings.Contains(got, "redraw=1") {
		t.Errorf("a prompt marker reached the client with redraw=1, so its terminal will clear the prompt "+
			"on the next resize and wait for a repaint that cannot arrive.\nstream: %q", got)
	}
	if n := strings.Count(got, "redraw=0"); n < 2 {
		t.Errorf("only %d markers carry redraw=0, want 2: a marker split across pty reads was passed "+
			"through unrewritten.\nstream: %q", n, got)
	}

	// And the holdback must not have corrupted anything on the way through.
	if problems := ansi.Validate(readTranscript(t, transcript)); len(problems) > 0 {
		for _, p := range problems {
			t.Errorf("%v", p)
		}
	}
}
