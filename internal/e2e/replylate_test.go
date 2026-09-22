package e2e

import (
	"strings"
	"testing"
	"time"
)

// TestAnAnswerReachesTheShellAfterItsAskerExits pins the reported `62;22c` at a prompt.
//
// This asserts the bug rather than a fix, deliberately, and it is the only test here that does. Nothing
// else reproduces the artifact: three rounds of manual A/B testing produced four wrong theories, and the
// chain only became clear once the shim logged how long each answer took. Pinning it means the next attempt
// at a fix has something that fails today and a recorded shape to compare against. Whoever fixes it should
// invert the assertion below rather than delete the test.
//
// The chain, measured in a sandbox: a program asks a terminal-only query and then one cm answers itself.
// The first goes to a client and nothing answers it. The second is queued behind it to keep the program's
// answers in order, and released only when the sweeper expires the first at requestTimeout, measured at
// 507ms, 510ms and 525ms. The asking program is gone by then, so the answer lands on a pty the shell is
// reading, and zsh in vi mode inserts it into a search one character at a time.
//
// The fault makes the exit unconditionally first. Without it the test rests on the program exiting inside
// the 500ms hold, which it does easily, but an inequality is not an ordering: on a slow machine the answer
// arrives while the program is still there, which is the correct case and proves nothing.
//
// `sh -i` for job control, so the program gets its own process group and the shell takes the terminal back
// when it exits. That is what makes the shell the reader, and it is the state any candidate fix has to
// distinguish.
func TestAnAnswerReachesTheShellAfterItsAskerExits(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty")
	}

	// Comfortably longer than a printf-and-exit program, which is a few milliseconds. `sh -i` asks the
	// terminal nothing itself, so exactly one reply passes this point and the delay is paid once.
	e := newEnvWith(t, cmHooksBinary(t), "",
		"CM_TESTHOOK_FAULTS=before-reply-write:delay=700ms")

	c := attachOnPty(t, e, "replylate", "--", "/bin/sh", "-i")

	// Markers printed in pieces, so what is waited for appears in the shell's output and not in its echo of
	// the typed line. Waiting for a needle the echo contains returns before the command has run, which is
	// the trap internal/shellinit's pty test records and which cost three wrong results here.
	c.typeLine(`printf 'REA%s\n' DY`)
	waitForOnPty(t, c, "READY")

	// Asks primary DA and exits. The shell's printf expands the escape, so the query reaches the pty as real
	// bytes rather than as the four characters of a Go literal.
	//
	// The leading sleep is load-bearing and platform-deciding, and it is here because the *fix* attempts
	// depend on it rather than this assertion. A shell hands the terminal to a job around the time it starts
	// rather than before, so a job that prints immediately has its output read while the shell is still the
	// foreground group. A version of this test without the sleep passed on darwin, which won that handover by
	// luck, and failed in the Linux image. Keeping it means the recorded shape is the one a fix must handle:
	// a program that held the terminal for a while before it asked, which is nvim and every long-running TUI.
	c.typeLine(`sh -c 'sleep 0.3; printf "\033[c"'`)

	// The second marker proves the shell is reading again, and then the artifact is waited for rather than
	// snapshotted. Asserting at DONE-time is what an earlier version did, and it only worked because the
	// fault used to block the pump: the shell could not echo DONE until the reply write had completed, so
	// the reply was always already in the stream. Delivering late without stalling the pump breaks that
	// coupling, and the assertion started failing on a reply that did arrive a moment later.
	c.typeLine(`printf 'DON%s\n' E`)
	waitForOnPty(t, c, "DONE")

	// Matched on the answer's digits rather than the whole sequence. A shell that reads ESC as the start of a
	// key binding consumes it and echoes the rest, so `\x1b[?62;22c` never appears as a unit: the reported
	// capture showed the characters interleaved with cursor moves, and a grep for the sequence found nothing
	// while the garbage was on screen.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(c.output(), "62;22c") {
		if time.Now().After(deadline) {
			t.Fatalf("the answer did not reach the shell within 5s of its asker exiting, so this bug no "+
				"longer reproduces the way it was measured. If that is a fix, invert this assertion; if not, "+
				"the reproduction has drifted and the chain recorded in docs/architecture.md no longer "+
				"matches.\npty:\n%s", c.output())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
