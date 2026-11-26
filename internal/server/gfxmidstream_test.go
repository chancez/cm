package server

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/vt"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// gatedStream is a fakeAttachStream whose requests after the first are handed over one at a time, so a test
// can put a client's request at a chosen point in the session's output.
type gatedStream struct {
	*fakeAttachStream
	// next carries each later request. Closing it ends the stream.
	next chan *serverv1.AttachRequest
	// handled receives once the service asks for the request after one handed over, which is when it has
	// finished with that one: the service reads its requests one at a time and acts on each before the next.
	handled chan struct{}
	opened  bool
	// handedOver is set once a gated request has gone to the service, so the next Recv reports it handled.
	handedOver bool
}

func (g *gatedStream) Recv() (*serverv1.AttachRequest, error) {
	if !g.opened {
		g.opened = true
		return g.fakeAttachStream.Recv()
	}
	if g.handedOver {
		g.handled <- struct{}{}
	}
	select {
	case req, ok := <-g.next:
		if !ok {
			return nil, io.EOF
		}
		g.handedOver = true
		return req, nil
	case <-g.ctx.Done():
		return nil, g.ctx.Err()
	}
}

// RecvMsg is shadowed too, so nothing reaches the embedded stream's queue behind the gate.
func (g *gatedStream) RecvMsg(m any) error {
	panic("the service reads with Recv")
}

// A terminal that says it can draw while an image is part way through arriving is still sent that image.
//
// The answer to cm's graphics probe crosses the client's link after the attach, and until it lands the
// session's output reaches that client with its images stripped. kitty's own client sends an image in 4096 byte
// chunks, so a large one is hundreds of commands, and an answer landing between two of them found the image
// incomplete in the store, so the push it triggered carried nothing, while every chunk already sent, the first
// with its format and size among them, had been stripped. The rest then went out live without its start, and
// the terminal drew nothing. Seen as TestALargeImageReachesASecondClient failing on a slow CI runner, where the
// first client answered partway through 1.9 MB of image.
func TestAnAnswerMidTransmissionStillGetsTheImage(t *testing.T) {
	mgr, st, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return vt.NewSessionTerminal(rows, cols, 0)
	})
	ctx := context.Background()
	rec := startShimFor(t, shimConfigFor("midstream", markerScript))
	rec.State = "running"
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("midstream")
	if !ok {
		t.Fatal("session was not adopted")
	}
	// Cell metrics, which a placement needs to be computed at all.
	if err := sess.Resize(ctx, 24, 80, 800, 480); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	// Each write is printed back by markerScript, which is how this test writes the session's output.
	output := func(s string) {
		t.Helper()
		want := sess.LastSeq() + seq.Shim(len(s))
		if err := sess.Write(ctx, []byte(s)); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for sess.LastSeq() < want {
			if time.Now().After(deadline) {
				t.Fatalf("the pump never consumed %q", s)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// markerReady, so the terminal is raw before anything is written to it.
	waitFor("the session to start", func() bool { return sess.LastSeq() >= seq.Shim(len(markerReady)) })

	streamCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stream := &gatedStream{
		// Saying nothing about graphics at Open, as a client does on a first attach: it asks its terminal and
		// does not wait for the answer.
		fakeAttachStream: newFakeStream(streamCtx,
			openReq(&serverv1.Open{Session: "midstream", Rows: 24, Cols: 80, XPixel: 800, YPixel: 480})),
		next:    make(chan *serverv1.AttachRequest),
		handled: make(chan struct{}, 1),
	}
	attachDone := make(chan struct{})
	go func() {
		defer close(attachDone)
		_ = NewService(mgr).Attach(streamCtx, stream)
	}()
	defer func() {
		close(stream.next)
		<-attachDone
	}()
	// The client's position in the session's output, as the service has told it.
	position := func() seq.Log {
		var at seq.Log
		for _, resp := range stream.sent() {
			if o := resp.GetOutput(); o != nil {
				end := seq.Log(o.Seq) + seq.Log(len(o.Data))
				if o.NextSeq != 0 {
					end = seq.Log(o.NextSeq)
				}
				at = max(at, end)
			}
			if op := resp.GetOpened(); op != nil {
				at = max(at, seq.Log(op.NextSeq))
			}
		}
		return at
	}
	waitFor("the attach to open", func() bool { return len(stream.sent()) > 0 })

	// The first chunk, which carries the image's format and size, reaches the client before its terminal has
	// answered, so it is stripped.
	output("\x1b_Ga=T,q=2,f=24,s=2,v=2,i=7,m=1;AQIDAQID\x1b\\")
	delivered := sess.recent.Next()
	waitFor("the first chunk to reach the client", func() bool { return position() >= delivered })

	// The answer lands between the chunks.
	stream.next <- &serverv1.AttachRequest{Event: &serverv1.AttachRequest_TerminalGraphics{
		TerminalGraphics: &serverv1.TerminalGraphics{DrawsImages: true},
	}}
	<-stream.handled

	output("\x1b_Ga=T,q=2,m=0;AQIDAQID\x1b\\")
	output("AFTER")
	delivered = sess.recent.Next()
	waitFor("the rest of the image to reach the client", func() bool { return position() >= delivered })

	// What the client's terminal received, in order, replayed into a fresh one.
	var received []byte
	for _, resp := range stream.sent() {
		switch {
		case resp.GetOpened() != nil:
			received = append(received, resp.GetOpened().Restore...)
		case resp.GetOutput() != nil:
			received = append(received, resp.GetOutput().Data...)
		case resp.GetImages() != nil:
			received = append(received, resp.GetImages().Data...)
		}
	}
	client, err := vt.NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer client.Close()
	if err := client.Resize(24, 80, 10, 20); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if err := client.Write(received); err != nil {
		t.Fatalf("replaying what the client received: %v", err)
	}
	got, err := client.Placements()
	if err != nil {
		t.Fatalf("client Placements() error = %v", err)
	}

	// Two by two at 24 bits, the twelve bytes the two chunks carry. Waited for, since the model is fed after
	// the output is delivered.
	var want []graphics.Placement
	waitFor("the session's own screen to hold the image, or the fixture tests nothing", func() bool {
		sess.termMu.Lock()
		defer sess.termMu.Unlock()
		want, err = sess.term.Placements()
		return err == nil && len(want) > 0
	})
	if !slices.Equal(got, want) {
		t.Errorf("the client's screen has placements %+v, want the session's %+v.\nreceived: %q",
			got, want, truncate(string(received)))
	}
	if !strings.Contains(string(received), "AFTER") {
		t.Errorf("the output after the image never reached the client: %q", truncate(string(received)))
	}
}
