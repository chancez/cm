package client

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/graphics"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// The terminal's yes reaches the server as an event, and does not reach the session as input.
//
// Both halves are the point. The server withholds images until a client says its terminal can draw them, so
// without the event no client ever sees a picture; and the answer is a reply to a question cm asked, so
// forwarding it would type `_Gi=...;OK` into whatever program is running.
//
// A unit test rather than only end to end because the answer arrives on the same channel as keystrokes, and
// the request it turns into is not observable from outside except by the images that follow.
func TestAGraphicsAnswerIsReportedToTheServer(t *testing.T) {
	h := newHarness(t)
	// A probe that has asked, which is the state the loop finds after Attach wrote the question.
	h.gfx = &graphicsProbe{}
	h.gfx.ask(newScreen(h.out, false, probeLog()), probeLog())

	h.stream.opened("test", 1, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := h.runAsync(ctx)

	h.input <- []byte(okAnswer() + "x")
	// Open, the answer and the keystroke: three requests, and waiting for all of them is what makes the
	// absence of one an assertion rather than a race.
	h.stream.waitForRequests(t, 3)

	var reported *serverv1.TerminalGraphics
	var typed []byte
	for _, req := range h.stream.requests() {
		if g := req.GetTerminalGraphics(); g != nil {
			reported = g
		}
		if in := req.GetInput(); in != nil {
			typed = append(typed, in.Data...)
		}
	}
	if reported == nil || !reported.DrawsImages {
		t.Errorf("TerminalGraphics = %v, want one reporting draws_images: without it the server sends no "+
			"images to any client", reported)
	}
	if got, want := string(typed), "x"; got != want {
		t.Errorf("input forwarded = %q, want %q: the answer belongs to cm, the keystroke to the program",
			got, want)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSession did not return")
	}
}

// A read-only client reports nothing, because it has no input channel to the session and no images are sent
// to it either: the whole point of one is that it cannot affect the session.
func TestAReadOnlyClientDoesNotReportGraphics(t *testing.T) {
	h := newHarness(t)
	h.opts.ReadOnly = true
	h.gfx = &graphicsProbe{}
	h.gfx.ask(newScreen(h.out, false, probeLog()), probeLog())
	h.stream.opened("test", 1, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := h.runAsync(ctx)

	h.input <- []byte(okAnswer())
	h.waitForInputConsumed(t)
	for _, req := range h.stream.requests() {
		if g := req.GetTerminalGraphics(); g != nil {
			t.Errorf("a read-only client reported %v, want nothing: it is sent no images either", g)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSession did not return")
	}
}

// Images pushed by the server reach the terminal, which is the other end of the same exchange.
func TestPushedImagesAreWrittenToTheTerminal(t *testing.T) {
	h := newHarness(t)
	// A painting screen, because a pipe-backed TTY is not a terminal and a screen over one drops every
	// injection: without this the test passes for an implementation that writes nothing at all.
	h.opts.screen = newScreen(h.out, true, probeLog())
	h.stream.opened("test", 1, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := h.runAsync(ctx)

	want := graphics.Encode("a=t,i=7,f=24,s=2,v=2", []byte("AQIDAQIDAQIDAQID"))
	h.stream.images(want)
	if got := h.terminalOutputWithin(5 * time.Second); !strings.Contains(got, string(want)) {
		t.Errorf("pushed images never reached the terminal; wrote %q", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runSession did not return")
	}
}

// A client sent fewer bytes than the log holds takes its position from the server, not from arithmetic.
//
// This is the resume half of stripping images out of a live stream. A client adds up what it received to know
// where it is, which is right until cm removes bytes on purpose: for a terminal that cannot draw images, the
// graphics commands never arrive. Adding up what did arrive leaves the position short of the truth, and the
// reconnect that follows replays the image this client was spared, one chunk at a time, forever.
func TestAStrippedChunkTakesItsPositionFromTheServer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nextSeq uint64
		want    uint64
	}{
		{"stated by the server", 900, 900},
		{"derived when absent", 0, 102},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.stream.opened("test", 100, nil)
			h.stream.recv <- recvResult{resp: &serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Output{
					Output: &serverv1.Output{Seq: 100, Data: []byte("ab"), NextSeq: tc.nextSeq},
				},
			}}
			h.stream.exited(0)

			if _, err := h.run(context.Background()); err != nil {
				t.Fatalf("runSession() error = %v", err)
			}
			if h.resumeFrom == nil {
				t.Fatal("no resume position was recorded")
			}
			if got := *h.resumeFrom; got != tc.want {
				t.Errorf("resume position = %d, want %d", got, tc.want)
			}
		})
	}
}
