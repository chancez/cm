package e2e

import (
	"strings"
	"testing"
	"time"
)

// A terminal query must not reach the real terminal, because cm answers it and a second answer has
// nowhere to go.
//
// The end-to-end half of the fix for "exiting vim leaves garbage below my prompt". The unit tests in
// internal/osc and internal/server cover the matching and the pump; this covers the wiring, which is
// the part they cannot: that the query really does stop at the server rather than arriving on a pty.
//
// Answering as the outer terminal is what makes this a real reproduction. Before the fix, cm's own
// reply and this one both reached the shell, and the second was printed at the prompt.
func TestQueriesDoNotReachTheRealTerminal(t *testing.T) {
	skipIfShort(t)

	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "q")

	c := attachOnPty(t, e, "q")
	c.waitReady()

	// printf writes a real ESC [ c, which is what a program querying the terminal sends. Written as
	// an octal escape the shell expands, not a Go escape: a needle containing a literal backslash-x
	// never matches real bytes, and that mistake has made tests here pass while asserting nothing.
	e.mustRun("send", "q", `printf 'BEFORE\033[cAFTER\n'`, "--enter")

	// Wait for the text after the query, so its absence is a real absence and not a race with
	// delivery. Polled rather than waitForOutput because a timeout here should fail with the
	// assertion below rather than a bare harness error.
	var onPty string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		onPty = c.output()
		if strings.Contains(onPty, "AFTER") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !strings.Contains(onPty, "BEFORE") || !strings.Contains(onPty, "AFTER") {
		t.Fatalf("pty never showed the output around the query, got %q", onPty)
	}

	// The query itself must not have arrived. A real terminal seeing this would answer it.
	if strings.Contains(onPty, "\x1b[c") {
		t.Errorf("the DA1 query reached the pty: %q\n"+
			"cm's emulator already answered it, so the real terminal answers a second time and the "+
			"spare reply is printed by the shell's line editor.", onPty)
	}

	// Answer the way a real terminal would, with a reply distinguishable from libghostty's
	// (?62;22c). Nothing asked, so nothing should consume it: this proves the assertion above is not
	// merely about timing, since a session that echoed this would show it.
	c.write([]byte("\x1b[?64;1;9;15;52;c"))
	time.Sleep(500 * time.Millisecond)

	// And cm's own answer must have reached the shell, or the program that asked would hang. This is
	// the control that keeps the test honest: if stripping had removed the query before the emulator
	// saw it, no reply would exist at all and the assertions above would pass for the wrong reason.
	raw := e.mustRun("read", "q", "--raw", "--lines", "40")
	if !strings.Contains(raw, "62;22c") {
		t.Errorf("session output %q contains no reply from cm's emulator (62;22c).\n"+
			"The query was stripped from the client stream but must still reach the terminal model, "+
			"which is what answers it. Otherwise a program querying the terminal blocks forever.", raw)
	}
}
