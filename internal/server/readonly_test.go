package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// fakeAttachStream stands in for a client's attach stream, so the service can be driven without a
// socket.
//
// The point of testing at this level rather than end to end: read-only has to be enforced by the
// server, not by the client choosing not to send. A client is untrusted, so a test that only drives
// the client would pass even if the server ignored the flag entirely.
type fakeAttachStream struct {
	ctx context.Context
	// hold keeps the stream open after the scripted requests run out, instead of reporting EOF.
	//
	// Needed for any test that asserts on what the server *sent*. An immediate EOF makes the
	// service's receive-error case ready at the same moment as the output it is about to deliver,
	// and a select among ready cases picks arbitrarily, so the attach can return before sending
	// anything. That is not the server misbehaving: a real client holds its stream open, so the
	// case never arises outside the fake. It showed up as a one-run-in-many failure claiming a
	// follower saw no output.
	hold chan struct{}

	mu   sync.Mutex
	in   []*serverv1.AttachRequest
	out  []*serverv1.AttachResponse
	done bool
}

func newFakeStream(ctx context.Context, reqs ...*serverv1.AttachRequest) *fakeAttachStream {
	return &fakeAttachStream{ctx: ctx, in: reqs}
}

// newHeldFakeStream returns a stream that stays open until closeStream is called, for tests that
// assert on sent messages rather than on the attach returning.
func newHeldFakeStream(ctx context.Context, reqs ...*serverv1.AttachRequest) *fakeAttachStream {
	return &fakeAttachStream{ctx: ctx, in: reqs, hold: make(chan struct{})}
}

// closeStream releases a held stream, standing in for the client going away.
func (f *fakeAttachStream) closeStream() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hold != nil {
		select {
		case <-f.hold:
		default:
			close(f.hold)
		}
	}
}

func (f *fakeAttachStream) Recv() (*serverv1.AttachRequest, error) {
	f.mu.Lock()
	if len(f.in) == 0 {
		f.done = true
		hold := f.hold
		f.mu.Unlock()
		if hold == nil {
			// EOF stands for the client going away, which is what happens after it has said
			// everything it intends to.
			return nil, io.EOF
		}
		// Unlocked while blocking, so closeStream and Send are not deadlocked behind this.
		select {
		case <-hold:
			return nil, io.EOF
		case <-f.ctx.Done():
			return nil, f.ctx.Err()
		}
	}
	req := f.in[0]
	f.in = f.in[1:]
	f.mu.Unlock()
	return req, nil
}

func (f *fakeAttachStream) Send(resp *serverv1.AttachResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out = append(f.out, resp)
	return nil
}

// SendMsg and RecvMsg satisfy ttrpc.StreamServer, which the generated interface embeds. cm's service
// only ever uses the typed Send and Recv above, so these exist to make the fake assignable rather
// than to be called.
func (f *fakeAttachStream) SendMsg(m any) error {
	resp, ok := m.(*serverv1.AttachResponse)
	if !ok {
		return errors.New("unexpected message type")
	}
	return f.Send(resp)
}

func (f *fakeAttachStream) RecvMsg(m any) error {
	req, err := f.Recv()
	if err != nil {
		return err
	}
	out, ok := m.(*serverv1.AttachRequest)
	if !ok {
		return errors.New("unexpected message type")
	}
	// proto.Merge rather than assigning the struct: a protobuf message embeds a mutex, so copying
	// one by value is a race waiting to happen and go vet rejects it.
	proto.Reset(out)
	proto.Merge(out, req)
	return nil
}

func (f *fakeAttachStream) sent() []*serverv1.AttachResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*serverv1.AttachResponse, len(f.out))
	copy(out, f.out)
	return out
}

func openReq(o *serverv1.Open) *serverv1.AttachRequest {
	return &serverv1.AttachRequest{Event: &serverv1.AttachRequest_Open{Open: o}}
}

func inputReq(data string) *serverv1.AttachRequest {
	return &serverv1.AttachRequest{
		Event: &serverv1.AttachRequest_Input{Input: &serverv1.Input{Data: []byte(data)}},
	}
}

func resizeReq(rows, cols uint32) *serverv1.AttachRequest {
	return &serverv1.AttachRequest{
		Event: &serverv1.AttachRequest_Resize{Resize: &serverv1.Resize{Rows: rows, Cols: cols}},
	}
}

// A follower's input must not reach the shell. The server enforces this, because a client is not
// trusted to withhold it.
func TestReadOnlyClientCannotSendInput(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	// `cat` echoes whatever it receives, so any input that got through would come back as output.
	rec := startShimFor(t, shimConfigFor("ro-input", "echo READY; cat"))
	rec.State = "running"
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, ok := mgr.Get("ro-input")
	if !ok {
		t.Fatal("session was not adopted")
	}

	// Watch the session independently of the follower, so what the shell produced is observable.
	watch := sess.recent.Subscribe(0)
	defer watch.Close()
	readUntil(t, watch, "READY")

	svc := NewService(mgr)
	stream := newFakeStream(ctx,
		openReq(&serverv1.Open{Session: "ro-input", Rows: 24, Cols: 80, ReadOnly: true}),
		inputReq("INJECTED_BY_FOLLOWER\n"),
	)
	if err := svc.Attach(ctx, stream); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}

	// Give the shell a moment to echo anything that reached it.
	deadline, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	var seen strings.Builder
	for {
		c, err := watch.Next(deadline)
		if err != nil {
			break
		}
		seen.WriteString(string(c.Data))
	}
	if strings.Contains(seen.String(), "INJECTED_BY_FOLLOWER") {
		t.Errorf("a read-only client's input reached the shell: %q", seen.String())
	}
}

// A follower must not resize the session, or watching a build would reflow the window of whoever is
// working in it.
func TestReadOnlyClientCannotResize(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("ro-resize", "echo READY; sleep 5"))
	rec.State = "running"
	rec.Rows, rec.Cols = 24, 80
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, ok := mgr.Get("ro-resize")
	if !ok {
		t.Fatal("session was not adopted")
	}

	before, err := sess.State(ctx)
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}

	svc := NewService(mgr)
	// Both an opening size and an explicit resize, since either could have taken effect.
	stream := newFakeStream(ctx,
		openReq(&serverv1.Open{Session: "ro-resize", Rows: 60, Cols: 200, ReadOnly: true}),
		resizeReq(70, 210),
	)
	if err := svc.Attach(ctx, stream); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}

	after, err := sess.State(ctx)
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if after.Rows != before.Rows || after.Cols != before.Cols {
		t.Errorf("a read-only client resized the session from %dx%d to %dx%d",
			before.Rows, before.Cols, after.Rows, after.Cols)
	}
}

// A follower still receives output, since watching is the entire point.
func TestReadOnlyClientReceivesOutput(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("ro-output", "echo WATCHED_OUTPUT; sleep 5"))
	rec.State = "running"
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// Let the output land before the follower attaches, so it must arrive via the replayed window
	// rather than by luck of timing.
	sess, _ := mgr.Get("ro-output")
	warm := sess.recent.Subscribe(0)
	readUntil(t, warm, "WATCHED_OUTPUT")
	warm.Close()

	svc := NewService(mgr)
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Held open, because this asserts on what the server sent. A stream that EOFs immediately lets
	// the attach return before delivering anything.
	stream := newHeldFakeStream(streamCtx,
		openReq(&serverv1.Open{Session: "ro-output", Rows: 24, Cols: 80, ReadOnly: true}),
	)
	attachDone := make(chan struct{})
	go func() {
		defer close(attachDone)
		_ = svc.Attach(streamCtx, stream)
	}()

	// Poll rather than sleeping a fixed amount: the output has to arrive, and how long that takes
	// is not something to encode as a constant.
	var got string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got = followerOutput(stream)
		if strings.Contains(got, "WATCHED_OUTPUT") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stream.closeStream()
	<-attachDone

	if !strings.Contains(got, "WATCHED_OUTPUT") {
		t.Errorf("read-only client saw %q, want the session's output", got)
	}
}

// followerOutput collects everything a client was shown, from both the replayed screen and the live
// stream, since which one carries a given byte depends on when the client attached.
func followerOutput(stream *fakeAttachStream) string {
	var got strings.Builder
	for _, resp := range stream.sent() {
		if o := resp.GetOutput(); o != nil {
			got.WriteString(string(o.Data))
		}
		if op := resp.GetOpened(); op != nil {
			got.WriteString(string(op.Restore))
		}
	}
	return got.String()
}
