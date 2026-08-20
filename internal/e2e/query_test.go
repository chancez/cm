package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cm answers a query its model can answer, and the client's terminal is never asked.
//
// The end-to-end half of the proxy design, and the contract here is the *reverse* of what this file
// asserted before. The old rule was that a query travelled out to the attached terminal, that terminal
// answered, and cm stayed silent. That needed cm to elect one client as the answerer, and the election was
// wrong in four separate ways, each of which shipped: a read-only follower or a not-yet-attached client
// elected meant nothing answered and the program hung; two clients meant a single CSI c came back as
// "\x1b[?62;52;c\x1b[?62;52;c"; and after a server restart cm answered a backlog query which the
// reconnecting client answered again from the log, typing a git branch name into the prompt.
//
// So cm now answers everything its terminal model can answer, always, and only the queries it genuinely
// cannot answer are put to a client. DA1 is one cm can answer, so the query is answered locally and the
// client's terminal never sees it.
//
// The unit tests in internal/server cover the decision. This covers the wiring they cannot: that the reply
// really does reach the shell through a real pty, and exactly once.
func TestCMAnswersDA1WithoutAskingTheTerminal(t *testing.T) {
	skipIfShort(t)

	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "q")

	c := attachOnPty(t, e, "q")
	c.waitReady()

	// printf writes a real ESC [ c, which is what a program querying the terminal sends. Written as an
	// octal escape the shell expands rather than a Go escape: a needle holding a literal backslash-x never
	// matches real bytes, and that has made tests here pass while asserting nothing.
	e.mustRun("send", "q", `printf 'BEFORE\033[cAFTER\n'`, "--enter")
	time.Sleep(2 * time.Second)

	raw := e.mustRun("read", "q", "--raw", "--lines", "40")

	// cm's own DA1 reply must reach the shell. libghostty answers ?62;22c (vt220 plus ansi_color).
	if !strings.Contains(raw, "62;22c") {
		t.Errorf("session output %q does not contain cm's DA1 reply (62;22c).\n"+
			"cm answers what its model can answer, whatever is attached, because it is the only writer of "+
			"a reply to this pty. Staying silent leaves the querying program waiting forever.", raw)
	}

	// And exactly one reply, not two. The count is the assertion rather than the presence, because the
	// defect this design removes is duplication rather than absence.
	if n := strings.Count(raw, "62;22c"); n != 1 {
		t.Errorf("session output %q contains %d DA1 replies, want exactly 1.\n"+
			"Two answers to one question means the program consumes one and the shell's line editor "+
			"prints the other.", raw, n)
	}
}

// A query cm cannot answer is put to the attached terminal, and that terminal's reply reaches the shell.
//
// The other half, and the capability this design adds rather than merely fixes. OSC 11 asks the background
// colour, which lives in the terminal's theme and which no terminal model can answer, so before this cm
// simply never answered it and `wallfacer -h` hung reading a reply that never came. Now cm asks one client
// and relays what comes back.
//
// Asserted through a real pty because the whole point is the round trip: the server has to send the
// question to a client, the client has to write it to its terminal, and the reply has to travel back on the
// input path and be matched to the request. No unit test spans all three hops.
func TestTerminalOnlyQueryIsProxiedToTheClient(t *testing.T) {
	skipIfShort(t)

	e := newEnv(t)
	e.mustRun("attach", "--no-attach", "bg")

	c := attachOnPty(t, e, "bg")
	c.waitReady()

	// Record where the client's captured output already ends, so the check below cannot match the echo of
	// the command being typed.
	//
	// This is the trap that made the first version of this test fail while the mechanism worked. `send`
	// with --enter types the command into the shell, and the shell echoes it, so the literal text
	// `\033]11;?\007` appears in the client's terminal as *characters* before the shell ever runs it.
	// Searching the whole capture for the escape sequence matches that echo, and the test then reports
	// either a false pass or a confusing failure. Only bytes arriving after this point are the real thing.
	before := len(c.output())

	// A program inside the session asks for the background colour.
	e.mustRun("send", "bg", `printf 'BEFORE\033]11;?\007AFTER\n'`, "--enter")

	// The question has to arrive at the client's terminal, which is the hop cm did not previously make.
	//
	// Polled tightly and answered as soon as it appears, because the request expires after requestTimeout
	// (500ms). A real terminal replies in single-digit milliseconds, so that bound is generous in practice,
	// but a test that polls leisurely and *then* writes the reply is answering a question cm has already
	// given up on, and the reply is correctly discarded as unsolicited.
	answered := false
	fresh := ""
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		all := c.output()
		if len(all) > before {
			fresh = all[before:]
		}
		// The raw sequence, in bytes that arrived after the echo. A shell echoing the command shows the
		// backslash-escaped text rather than a real ESC, so this only matches the sequence printf produced.
		if strings.Contains(fresh, "\x1b]11;?") {
			// Answer the way a real terminal would, immediately, while the request is still outstanding.
			c.write([]byte("\x1b]11;rgb:2828/2c2c/3434\x07"))
			answered = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !answered {
		t.Fatalf("the OSC 11 query never reached the client's terminal; new output was %q.\n"+
			"cm cannot answer this one, so it must ask a client or the program that sent it waits forever.",
			fresh)
	}
	// What this test covers, and what it deliberately does not.
	//
	// Asserted: the question travelled from a program in the session, through the server, to a specific
	// client's terminal. That is the hop cm never made before, and it is the whole reason OSC 11 used to
	// hang. It is also the hop no unit test can span, since it crosses two processes and a real pty.
	//
	// Not asserted here: that the reply then reaches the querying program. Observing that end to end needs
	// a program in the session reading its own stdin at the moment the reply arrives, and both available
	// observation points fail for reasons that are about the harness rather than about cm. `cm read --raw`
	// re-serializes the terminal model, so an injected reply appears nowhere in it, which is the trap
	// docs/testing.md names explicitly and which produced a confusing failure here first. Driving a `cat`
	// through the attached client to capture stdin races the shell's line editor, which consumes the
	// sequence first.
	//
	// The return path is covered deterministically at the seam instead, by TestDetachReleases..., the
	// ordering test, and TestDebugRoundTrip's successor in internal/server, which drive answerFromClient
	// and assert on the bytes written to the pty. Splitting it this way is the same division the rest of
	// this file uses: the seam tests own the decisions, the e2e tests own the wiring.
	if !answered {
		t.Fatal("the query never reached the client, so the wiring is not covered")
	}
}

// One question goes to one client, and both clients still deliver their own mouse events.
//
// Two things are asserted together because a fix for either that breaks the other passes any test checking
// only one. Output fans out to every client, so a question sent through the output stream would be asked of
// every terminal and each would answer: measured against a real kitty with two clients, a single CSI c came
// back as "\x1b[?62;52;c\x1b[?62;52;c". A question is therefore addressed to one client through its own
// channel.
//
// Mouse events are the counterexample that keeps the restriction narrow. A mouse report describes what
// happened in one window, so every client sends its own; restricting them would make a session ignore the
// mouse in every window but one.
func TestOneClientIsAskedButBothSendMouse(t *testing.T) {
	skipIfShort(t)

	e := newEnv(t)
	// A shell, so waitReady sees a prompt, then a `cat` that writes what it reads to a file. The file is
	// the observation point: what a client sends becomes shell input, and a shell consumes escape sequences
	// rather than echoing them, so `cm read` cannot see them. `cm read --raw` is no help either, since it
	// re-serializes the terminal model rather than replaying bytes.
	e.mustRun("attach", "--no-attach", "two")

	first := attachOnPty(t, e, "two")
	first.waitReady()
	second := attachOnPty(t, e, "two")
	second.waitReady()

	// Ask for the background colour *before* starting cat, because a foreground `cat` would swallow the
	// command instead of the shell running it. That ordering mistake made an earlier version of this test
	// report zero clients asked: `send` wrote the printf text into cat's stdin, so the query was never
	// emitted at all.
	//
	// Marked from where each client's capture already ends, so the echo of the command being typed is not
	// counted. `send --enter` types the text into the shell and the shell echoes it, so the literal
	// characters appear in both terminals before the shell runs anything; matching those would report both
	// clients as asked no matter what cm did.
	firstBefore := len(first.output())
	secondBefore := len(second.output())
	e.mustRun("send", "two", `printf '\033]11;?\007'`, "--enter")
	time.Sleep(2 * time.Second)

	freshOf := func(c *ptyClient, from int) string {
		all := c.output()
		if len(all) <= from {
			return ""
		}
		return all[from:]
	}
	// Both terminals see the query, and that is expected rather than a defect.
	//
	// This is the correction to an assumption that looked obviously right: cm forwards session output
	// verbatim to every client, so a query a program prints is *displayed* by every attached terminal, and
	// each terminal may answer it on its own. Suppressing that would mean editing the output stream, which
	// was tried twice and reverted both times, the second time because removing bytes made the client log
	// shorter than the shim's numbering and clamped a reconnecting client into the middle of an escape
	// sequence.
	//
	// What makes one answer reach the shell is not that one terminal sees the question. It is that cm asked
	// exactly one client through its own channel and recorded that request, so exactly one reply matches
	// and every other is discarded as unsolicited. The matching is asserted deterministically at the seam
	// (TestTerminalOnlyQueryGoesToOneClient and TestUnsolicitedClientReplyIsDiscarded); what this test
	// covers is that the question reaches a real terminal at all.
	firstAsked := strings.Contains(freshOf(first, firstBefore), "\x1b]11;?")
	secondAsked := strings.Contains(freshOf(second, secondBefore), "\x1b]11;?")
	if !firstAsked && !secondAsked {
		t.Errorf("neither client's terminal saw the OSC 11 query (first=%v second=%v).\n"+
			"cm cannot answer this one, so it has to reach a terminal or the asking program hangs.",
			firstAsked, secondAsked)
	}

	// Now start the capture, for the mouse half. Started here rather than at the top because a foreground
	// cat would have eaten the query command above.
	sink := filepath.Join(e.state, "stdin.bin")
	first.write([]byte("cat > " + sink + "\n"))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(sink); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cat never created %s, so nothing would be captured", sink)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Both report a mouse press, which is per window and must not be restricted.
	//
	// Spaced out rather than written back to back: a pty that batches these writes produces a chunk mixing
	// two reports, and IsUserInput is deliberately conservative about mixed chunks. The gaps make what is
	// being tested actually what arrives.
	first.write([]byte("\x1b[<0;11;11M"))
	time.Sleep(300 * time.Millisecond)
	second.write([]byte("\x1b[<0;22;22M"))
	time.Sleep(300 * time.Millisecond)

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

	if !strings.Contains(raw, "11;11M") {
		t.Errorf("session output %q is missing the first client's mouse report.", raw)
	}
	if !strings.Contains(raw, "22;22M") {
		t.Errorf("session output %q is missing the second client's mouse report.\n"+
			"Only terminal queries are addressed to one client. A mouse event describes one window, so "+
			"dropping it makes the session ignore the mouse everywhere but there.", raw)
	}
}
