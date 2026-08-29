package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/ansi"
	"github.com/chancez/cm/internal/vt"
)

// TestClientStreamRendersTheSameScreenAsTheModel is the differential oracle: two independent paths from
// the same output, compared against each other.
//
// The server keeps a terminal model and renders it for `cm read`. The client receives a byte stream and
// its terminal renders that. Both describe the same session, so feeding the client's stream to a second
// emulator must produce the screen the server thinks it has. Nothing here says what the screen should
// look like, which is what makes it cheap to write and hard to fool.
//
// This is the check that catches what the transcript validator cannot. Validate proves nothing cm
// injected split a sequence; it cannot see a byte that was dropped or delivered twice. Those are the two
// sequence-number-space failures, and they show up here as a screen that does not match.
//
// The second emulator is libghostty again rather than a different implementation, which is a real
// limitation: a bug in the emulator itself cancels out. It is still the right trade. The failures this
// family produces are cm dropping, duplicating or interleaving bytes, and for those the emulator is a
// faithful ruler even if it is not an independent one.
func TestClientStreamRendersTheSameScreenAsTheModel(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty")
	}

	e := newEnvWith(t, cmHooksBinary(t), "")
	transcript := e.state + "/differential.jsonl"

	// A program that paints a screen and then holds it, so the model and the client have both settled on
	// the same content when they are compared. The marker is the readiness signal.
	const marker = "DIFFERENTIAL-READY"
	script := strings.Join([]string{
		`printf '\033[2J\033[H'`,
		`i=1; while [ $i -le 8 ]; do printf '\033[%d;1H\033[38:2:%d:100:200mline %d of the screen\033[m' "$i" "$((i*20))" "$i"; i=$((i+1)); done`,
		`printf '\033]7;file://host/tmp\033\\'`,
		`printf '\0337\033[9;1H` + marker + `\0338'`,
		"sleep 30",
	}, "; ")

	c := attachOnPtyWithEnv(t, e,
		[]string{"CM_TESTHOOK_TRANSCRIPT=" + transcript},
		"differential", "--", "/bin/sh", "-c", script)

	waitForOnPty(t, c, marker)
	// The model is read after the pty has the marker, so both sides have seen the same output. The pump
	// feeds the model on the same path that wakes clients, so the client cannot be ahead of it.
	time.Sleep(500 * time.Millisecond)

	assertClientRendersLikeModel(t, e, "differential", transcript)
}

// TestServerRestartDoesNotChangeWhatTheClientRenders is the metamorphic relation for a restart.
//
// A restart is invisible to the session by design: the shim owns the pty, the shell keeps running, and
// the new server adopts what it finds. So the relation is that it changes nothing observable. Stated
// against the differential oracle rather than against a substring, which is what makes it able to see
// the failure this actually had: adoption resumed a client from a position counted in the wrong
// numbering, so a run of bytes was skipped and whatever escape sequence straddled that point arrived
// with its front sliced off.
//
// A substring check passes straight through that. A screen comparison does not.
func TestServerRestartDoesNotChangeWhatTheClientRenders(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty, and restarts the server")
	}

	e := newEnvWith(t, cmHooksBinary(t), "")
	transcript := e.state + "/restartdiff.jsonl"

	// Output in two halves with a prompt marker between them, since the rewrite that lengthens a prompt
	// marker is what makes the two numbering spaces drift apart. Without one the two positions agree and
	// the bug this targets cannot appear.
	const first = "BEFORE-RESTART"
	const second = "AFTER-RESTART"
	// The program produces output on both sides of the restart by itself, rather than the test injecting
	// any: the second half is emitted during a sleep long enough for the restart to land inside it. That
	// is the case that matters, output crossing the boundary, and it needs no shell at a prompt.
	script := strings.Join([]string{
		`printf '\033[2J\033[H'`,
		`printf '` + first + `\r\n'`,
		// A prompt marker, because the rewrite that appends to one is what makes the two numbering spaces
		// drift apart. Without it the two positions agree and the bug this targets cannot appear.
		`printf '\033]133;A\007prompt\r\n'`,
		`printf '\033[3;1H\033[38:2:10:20:30mstyled row\033[m'`,
		"sleep 6",
		`printf '\033[5;1H\033[38:2:200:100:50m` + second + `\033[m'`,
		"sleep 60",
	}, "; ")

	c := attachOnPtyWithEnv(t, e,
		[]string{"CM_TESTHOOK_TRANSCRIPT=" + transcript},
		"restartdiff", "--", "/bin/sh", "-c", script)
	waitForOnPty(t, c, first)

	e.restartServer()
	// Any command starts a replacement, which is what adopts the session. restartServer only stops the
	// old one, deliberately: a server told to stop stays stopped, and clients do not race to start one.
	e.list()

	// The second half arrives after the restart, delivered by the adopted session on the resumed
	// numbering. This is the half that went missing when one number was used for both spaces.
	waitForOnPty(t, c, second)
	time.Sleep(500 * time.Millisecond)

	// The screen has to match, and it does even when the resume is broken, because gap detection
	// repaints. So the load-bearing assertion is the one below: the resume was seamless.
	assertClientRendersLikeModel(t, e, "restartdiff", transcript)

	// A restart must not make the client repaint.
	//
	// This is the assertion that sees the failure a screen comparison cannot, and serve.go already names
	// the trap: an adopted session that "came back with an output gap detected repaint instead of a
	// seamless resume" looks like the gap detection working rather than the bug it is masking. Measured:
	// reintroducing the original defect, resumePoints returning the shim's number for both spaces, leaves
	// the screen correct and is invisible to every other restart test here, and shows up only as this
	// line in the client's log.
	if log := e.readFileOrEmpty(e.clientLogPath()); strings.Contains(log, "output gap detected") {
		t.Errorf("the client repainted after the restart instead of resuming seamlessly, so the resume "+
			"position named bytes it never received. The screen recovered, which is gap detection doing "+
			"its job and hiding the defect.\nclient log:\n%s", log)
	}
}

// assertClientRendersLikeModel replays the client's own stream into a fresh emulator and compares the
// result with what the server's model renders.
func assertClientRendersLikeModel(t *testing.T, e *env, session, transcript string) {
	t.Helper()

	writes := readTranscript(t, transcript)
	if len(writes) == 0 {
		t.Fatalf("the transcript at %s is empty, so nothing was compared", transcript)
	}

	// The client's terminal is 24x80, set by attachOnPtyWithEnv.
	const rows, cols = 24, 80
	term, err := vt.NewSessionTerminal(rows, cols, 200)
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Write(ansi.SessionBytes(writes)); err != nil {
		t.Fatalf("replaying the client's stream: %v", err)
	}
	// Compared as escape sequences rather than as plain text, and that is the difference between an
	// oracle and a decoration. Plain text carries no styling, so bytes dropped from the middle of an SGR
	// change the colour and nothing else, and a plain-text comparison passes. Measured: a mutation making
	// the client drop 9 bytes from every chunk over 40 was invisible to the plain form and is caught by
	// this one. Both sides are re-serializations of a model, so they are directly comparable.
	replayed, err := term.TailVT(rows)
	if err != nil {
		t.Fatalf("TailVT() error = %v", err)
	}

	model := e.mustRun("read", session, "--raw", "--lines", "24")

	if normalizeScreen(string(replayed)) != normalizeScreen(model) {
		t.Errorf("the client's stream and the server's model disagree about the screen.\n"+
			"from the client's own bytes:\n%s\nfrom the model:\n%s",
			normalizeScreen(string(replayed)), normalizeScreen(model))
	}
}

// normalizeScreen trims trailing spaces per row and drops trailing blank rows, so the comparison is
// about content rather than how far each renderer pads.
func normalizeScreen(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\r")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// waitForOnPty blocks until the client's terminal shows want.
func waitForOnPty(t *testing.T, c *ptyClient, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(c.output(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q on the pty, got %q", want, c.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
