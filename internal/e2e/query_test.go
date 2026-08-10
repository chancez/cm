package e2e

import (
	"os"
	"path/filepath"
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

// Two attached clients answer one query once, and both still deliver their own mouse events.
//
// The duplicate is the bug: output fans out to every client, so both terminals see a query and both
// reply, and the shell gets two answers to one question. Measured against a real kitty with two
// clients on one session, a single CSI c came back as "\x1b[?62;52;c\x1b[?62;52;c" where one client
// gave one reply.
//
// Mouse events are the other half, and the reason the drop is narrow rather than "anything that is
// not typing". A mouse report describes what happened in one window, so every client sends its own;
// dropping them would make a session ignore the mouse in every window but the answerer's. Both halves
// are asserted here because a fix for the first that breaks the second passes any test that only
// checks for the duplicate.
func TestTwoClientsAnswerOnceButBothSendMouse(t *testing.T) {
	skipIfShort(t)

	e := newEnv(t)
	// A shell, so waitReady sees a prompt, then a `cat` that writes what it reads to a file. The
	// file is the observation point: what a client sends becomes shell input, and a shell consumes
	// escape sequences rather than echoing them, so `cm read` cannot see them. `cm read --raw` is no
	// help either, since it re-serializes the terminal model rather than replaying bytes.
	e.mustRun("attach", "--no-attach", "two")

	first := attachOnPty(t, e, "two")
	first.waitReady()
	second := attachOnPty(t, e, "two")
	second.waitReady()

	// Start a reader that records raw stdin, so client-sent sequences are captured verbatim instead
	// of being swallowed by the shell's line editor.
	sink := filepath.Join(e.state, "stdin.bin")
	first.write([]byte("cat > " + sink + "\n"))
	time.Sleep(1 * time.Second)

	// Both clients answer the same DA1 query, as two real terminals would. Distinguishable replies,
	// so the assertion can say which arrived.
	first.write([]byte("\x1b[?62;11c"))
	second.write([]byte("\x1b[?62;22c"))

	// And both report a mouse press, which is per window and must not be dropped.
	first.write([]byte("\x1b[<0;11;11M"))
	second.write([]byte("\x1b[<0;22;22M"))

	// Newline so cat flushes, then end it so the file is complete.
	first.write([]byte("\n"))
	time.Sleep(1 * time.Second)
	first.write([]byte("\x04"))
	time.Sleep(1 * time.Second)

	captured, err := os.ReadFile(sink)
	if err != nil {
		t.Fatalf("reading captured stdin: %v", err)
	}
	raw := string(captured)

	// Exactly one of the two replies reaches the shell. Which one is not the point; that only one
	// does is.
	replies := 0
	if strings.Contains(raw, "62;11c") {
		replies++
	}
	if strings.Contains(raw, "62;22c") {
		replies++
	}
	if replies != 1 {
		t.Errorf("session output %q carried %d of the two DA1 replies, want exactly 1.\n"+
			"Two clients each answering one query means the shell gets two answers, and the one the "+
			"program does not consume is printed by its line editor.", raw, replies)
	}

	// Both mouse reports must arrive, from the answerer and the other client alike.
	if !strings.Contains(raw, "11;11M") {
		t.Errorf("session output %q is missing the first client's mouse report.", raw)
	}
	if !strings.Contains(raw, "22;22M") {
		t.Errorf("session output %q is missing the second client's mouse report.\n"+
			"Only query replies are restricted to one client. A mouse event describes one window, so "+
			"dropping it makes the session ignore the mouse everywhere but the answerer.", raw)
	}
}
