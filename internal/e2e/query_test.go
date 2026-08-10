package e2e

import (
	"strings"
	"testing"
	"time"
)

// A query reaches the attached terminal and that terminal's answer is the only one the shell sees.
//
// The end-to-end half of the fix for "exiting vim leaves garbage below my prompt" and the
// ";rgb:2828/2c2c/3434" that followed `wallfacer -h`. Both were one defect: cm's emulator answered a
// query and wrote the reply to the pty while a real terminal was answering too. An injected reply is
// not addressed to whoever is reading, so some program consumed the wrong one and the leftover was
// printed by the shell's line editor.
//
// The unit tests in internal/server cover the decision. This covers the wiring the unit tests cannot:
// that a query really does travel out to a client's terminal, and that the terminal's reply really
// does come back to the shell, with cm staying silent throughout.
func TestAttachedTerminalAnswersQueries(t *testing.T) {
	skipIfShort(t)

	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "q")

	c := attachOnPty(t, e, "q")
	c.waitReady()

	// printf writes a real ESC [ c, which is what a program querying the terminal sends. Written as
	// an octal escape the shell expands rather than a Go escape: a needle holding a literal
	// backslash-x never matches real bytes, and that has made tests here pass while asserting nothing.
	e.mustRun("send", "q", `printf 'BEFORE\033[cAFTER\n'`, "--enter")

	// The query must reach the pty, because the attached terminal is now the thing that answers it.
	// Suppressing it would leave nothing to answer and the program that asked would hang.
	onPty := ""
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		onPty = c.output()
		if strings.Contains(onPty, "\x1b[c") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(onPty, "\x1b[c") {
		t.Fatalf("the DA1 query never reached the pty, got %q.\n"+
			"With a client attached the terminal is the answerer, so the query has to get there.", onPty)
	}

	// Answer the way a real terminal would, with a reply distinguishable from libghostty's own
	// (?62;22c). Real kitty answers ?62;52;c, where 52 is its clipboard feature code.
	const terminalReply = "\x1b[?64;1;9;15;52;c"
	c.write([]byte(terminalReply))
	time.Sleep(2 * time.Second)

	raw := e.mustRun("read", "q", "--raw", "--lines", "40")

	// The terminal's answer must reach the shell, or a querying program waits forever.
	if !strings.Contains(raw, "64;1;9;15;52;c") {
		t.Errorf("session output %q does not contain the terminal's reply.\n"+
			"A client's terminal answered the query and that answer has to reach the shell.", raw)
	}

	// And cm must not have answered as well. Two answerers on one pty is the defect: whichever reply
	// a program happens to read, the other is left over and lands in the line editor.
	if strings.Contains(raw, "62;22c") {
		t.Errorf("session output %q contains cm's own DA1 reply (62;22c) alongside the terminal's.\n"+
			"With a client attached cm must stay silent, or a program gets two answers and prints the "+
			"one it did not consume.", raw)
	}
}
