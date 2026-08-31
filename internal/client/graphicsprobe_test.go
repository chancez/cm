package client

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/graphics"
)

func probeLog() *slog.Logger { return slog.New(discardLogHandler{}) }

// asked returns a probe that has already sent its question, with the screen it wrote to.
func asked(t *testing.T) (*graphicsProbe, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	scr := newScreen(&buf, true, probeLog())
	p := &graphicsProbe{}
	p.ask(scr, probeLog())
	return p, &buf
}

// okAnswer is the graphics half of what a terminal that can draw images replies.
func okAnswer() string {
	return "\x1b_Gi=" + strconv.FormatUint(uint64(graphics.ProbeImageID), 10) + ";OK\x1b\\"
}

// deviceAttributes is the other half, which every terminal sends because cm asks for it.
const deviceAttributes = "\x1b[?62;c"

// The question has to be asked, and paired with a device attributes query: that pairing is what makes a
// silent terminal answerable without waiting, since every terminal answers primary DA.
func TestGraphicsProbeAsksAPairedQuestion(t *testing.T) {
	_, out := asked(t)
	got := out.String()
	if !strings.Contains(got, "a=q") {
		t.Errorf("no graphics query was written; wrote %q", got)
	}
	if !strings.Contains(got, "\x1b[c") {
		t.Errorf("no primary DA was written, so a silent terminal could never be told apart from a slow "+
			"one; wrote %q", got)
	}
}

// A yes is recognised and consumed.
func TestGraphicsProbeTakesAnOK(t *testing.T) {
	p, _ := asked(t)
	rest, answered, draws := p.take([]byte(okAnswer()), time.Now())
	if !answered || !draws {
		t.Errorf("answered=%v draws=%v, want both true", answered, draws)
	}
	if len(rest) != 0 {
		t.Errorf("rest = %q, want nothing: the answer is cm's to consume", rest)
	}
}

// The device attributes reply alone is the no, which is what keeps this off a timeout.
func TestGraphicsProbeTakesDeviceAttributesAsNo(t *testing.T) {
	p, _ := asked(t)
	rest, answered, draws := p.take([]byte("\x1b[?62;c"), time.Now())
	if !answered || draws {
		t.Errorf("answered=%v draws=%v, want answered with draws false", answered, draws)
	}
	if len(rest) != 0 {
		t.Errorf("rest = %q, want nothing", rest)
	}
}

// Keystrokes around the answer are kept. Dropping them loses what the user typed.
func TestGraphicsProbeKeepsTyping(t *testing.T) {
	p, _ := asked(t)
	rest, answered, draws := p.take([]byte("ab"+okAnswer()+"cd"), time.Now())
	if !answered || !draws {
		t.Errorf("answered=%v draws=%v, want both true", answered, draws)
	}
	if got, want := string(rest), "abcd"; got != want {
		t.Errorf("rest = %q, want %q", got, want)
	}
}

// A reply split across reads still counts, which a pty guarantees: reads cap at 1022 bytes and nothing
// aligns them to a sequence.
func TestGraphicsProbeTakesASplitAnswer(t *testing.T) {
	answer := okAnswer()
	for cut := 1; cut < len(answer); cut++ {
		p, _ := asked(t)
		now := time.Now()
		_, answered, _ := p.take([]byte(answer[:cut]), now)
		if answered {
			t.Fatalf("answered on the first half of a split at %d", cut)
		}
		_, answered, draws := p.take([]byte(answer[cut:]), now)
		if !answered || !draws {
			t.Errorf("split at %d: answered=%v draws=%v, want both true", cut, answered, draws)
		}
	}
}

// Once the question is answered, cm claims nothing more. A second reply belongs to whoever asked next, and
// eating it truncates that conversation.
func TestGraphicsProbeStopsClaimingRepliesOnceAnswered(t *testing.T) {
	p, _ := asked(t)
	now := time.Now()
	p.take([]byte(okAnswer()), now)

	// A cursor position report, which a program inside the session asks for constantly.
	rest, answered, _ := p.take([]byte("\x1b[5;7R"), now)
	if answered {
		t.Error("answered twice, so a program's reply was claimed as cm's")
	}
	if got, want := string(rest), "\x1b[5;7R"; got != want {
		t.Errorf("rest = %q, want %q forwarded to the program", got, want)
	}
}

// And once the window has passed, nothing is claimed either. This is the property that makes a generous
// window safe: it bounds how long cm can mistake a program's reply for its own.
func TestGraphicsProbeStopsClaimingRepliesAfterTheWindow(t *testing.T) {
	p, _ := asked(t)
	late := p.asked.Add(probeGraphicsWindow + time.Second)

	rest, answered, _ := p.take([]byte(okAnswer()), late)
	if answered {
		t.Error("claimed an answer after the window closed")
	}
	if got, want := string(rest), okAnswer(); got != want {
		t.Errorf("rest = %q, want the reply forwarded untouched", got)
	}
}

// A reply that is not this probe's answer is forwarded even while the question is outstanding, because a
// program may already be asking its own questions.
func TestGraphicsProbeForwardsSomeoneElsesReply(t *testing.T) {
	p, _ := asked(t)
	// A graphics response for another image, which is what a program's own icat probe looks like.
	other := "\x1b_Gi=31;OK\x1b\\"

	rest, answered, _ := p.take([]byte(other), time.Now())
	if answered {
		t.Error("claimed another image's response as this probe's answer")
	}
	if got := string(rest); got != other {
		t.Errorf("rest = %q, want %q forwarded", got, other)
	}
}

// A probe that never asked claims nothing, which is a resume or a client with no terminal.
func TestGraphicsProbeThatNeverAskedClaimsNothing(t *testing.T) {
	var p graphicsProbe
	rest, answered, _ := p.take([]byte(okAnswer()), time.Now())
	if answered {
		t.Error("answered without having asked")
	}
	if got := string(rest); got != okAnswer() {
		t.Errorf("rest = %q, want the input untouched", got)
	}
}

// A terminal that can draw images sends both replies, and the second must not revise the first.
//
// This is the whole exchange as a real terminal performs it: the graphics response, then the device
// attributes reply, back to back and in one read. Reading the DA reply as "no graphics answer came" turned
// every yes into a no, so no client was ever sent an image and the negative test still passed. The unit tests
// missed it by feeding the graphics response alone, which no terminal sends.
func TestGraphicsProbeIsNotRevisedByTheDeviceAttributesReply(t *testing.T) {
	for _, tc := range []struct {
		name string
		feed []string
	}{
		{"one read", []string{okAnswer() + deviceAttributes}},
		{"two reads", []string{okAnswer(), deviceAttributes}},
		{"split mid-DA", []string{okAnswer() + deviceAttributes[:3], deviceAttributes[3:]}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := asked(t)
			now := time.Now()

			var sawAnswer, sawDraws bool
			var rest []byte
			for _, chunk := range tc.feed {
				r, answered, draws := p.take([]byte(chunk), now)
				rest = append(rest, r...)
				if answered {
					sawAnswer, sawDraws = true, draws
				}
			}
			if !sawAnswer || !sawDraws {
				t.Errorf("answered=%v draws=%v, want both true: the terminal said OK, and the device "+
					"attributes reply that follows it is not a second answer", sawAnswer, sawDraws)
			}
			if len(rest) != 0 {
				t.Errorf("rest = %q, want nothing forwarded: cm asked both questions", rest)
			}
		})
	}
}

// And once both replies are in, the exchange is over: the next reply belongs to whoever asked next.
func TestGraphicsProbeStopsClaimingAfterBothReplies(t *testing.T) {
	p, _ := asked(t)
	now := time.Now()
	p.take([]byte(okAnswer()+deviceAttributes), now)

	// A program's own graphics query answer, which arrives constantly once something like icat runs.
	other := "\x1b_Gi=31;OK\x1b\\"
	rest, answered, _ := p.take([]byte(other), now)
	if answered {
		t.Error("answered a third time, so a program's reply was claimed as cm's")
	}
	if got := string(rest); got != other {
		t.Errorf("rest = %q, want %q forwarded to the program", got, other)
	}
}

// The answer outlives the connection it arrived on, and a probe with no answer can be asked again.
//
// Both are what a reconnect needs, and their absence was a shipped bug: the answer was reported only on the
// connection that received it, so every Open after a server restart said "cannot draw" while the settled
// exchange meant nothing asked again. A kitty that had answered yes lost images for the rest of its life.
func TestGraphicsProbeRemembersTheAnswerAndCanBeAskedAgain(t *testing.T) {
	p, out := asked(t)
	if p.drawsImages() {
		t.Error("drawsImages() is true before any answer")
	}

	p.take([]byte(okAnswer()+deviceAttributes), time.Now())
	if !p.drawsImages() {
		t.Fatal("drawsImages() is false after the terminal said OK, so no later Open can carry the answer")
	}

	// Asking again re-arms the exchange, which is what a client with no answer does on its next attach. The
	// yes is kept, since it is a fact about the terminal rather than about the connection.
	before := out.Len()
	p.ask(newScreen(out, true, probeLog()), probeLog())
	if out.Len() == before {
		t.Error("a second ask wrote nothing, so a terminal that missed the first window is never asked again")
	}
	if !p.drawsImages() {
		t.Error("asking again forgot the answer")
	}
	// And the re-armed exchange can be answered.
	_, answered, draws := p.take([]byte(okAnswer()), time.Now())
	if !answered || !draws {
		t.Errorf("answered=%v draws=%v after a second ask, want both true", answered, draws)
	}
}

// Who gets asked, and when. The resume row is the one that shipped wrong.
//
// A resuming client is not repainted, so nothing erases the question from a terminal that renders an APC as
// text, and a resume therefore used to skip asking. That stranded a client whose process had never asked and
// which only ever reconnects by resuming: it never asks, never answers, and cm treats its terminal as unable
// for the rest of its life. Reported as no images in a plain kitty, with the client log showing no answer line
// at all for that client. So a resume asks once per process, which bounds the cost to one line of text against
// images never working.
func TestGraphicsProbeAsksOnAResumeOnlyOnce(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		draws, everAsked               bool
		isTerminal, painting, resuming bool
		want                           bool
	}{
		{"fresh attach, nothing known", false, false, true, true, false, true},
		{"fresh attach, asked before", false, true, true, true, false, true},
		{"resume, never asked", false, false, true, true, true, true},
		{"resume, already asked", false, true, true, true, true, false},
		{"already answered yes", true, true, true, true, false, false},
		{"follower with no terminal", false, false, false, true, false, false},
		{"follower not painting", false, false, true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &graphicsProbe{draws: tc.draws, everAsked: tc.everAsked}
			if got := p.shouldAsk(tc.isTerminal, tc.painting, tc.resuming); got != tc.want {
				t.Errorf("shouldAsk(%v, %v, %v) = %v, want %v",
					tc.isTerminal, tc.painting, tc.resuming, got, tc.want)
			}
		})
	}
}

// And asking records that it happened, which is what makes the resume case ask once rather than every time.
func TestGraphicsProbeRecordsThatItAsked(t *testing.T) {
	p, _ := asked(t)
	if !p.everAsked {
		t.Error("everAsked is false after asking, so a resuming client would ask on every reconnect")
	}
	if !p.shouldAsk(true, true, false) {
		t.Error("a fresh attach with no answer does not ask")
	}
	if p.shouldAsk(true, true, true) {
		t.Error("a resume asks again after this process already asked")
	}
}
