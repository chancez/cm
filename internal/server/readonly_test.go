package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

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

	mu   sync.Mutex
	in   []*serverv1.AttachRequest
	out  []*serverv1.AttachResponse
	done bool
}

func newFakeStream(ctx context.Context, reqs ...*serverv1.AttachRequest) *fakeAttachStream {
	return &fakeAttachStream{ctx: ctx, in: reqs}
}

func (f *fakeAttachStream) Recv() (*serverv1.AttachRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.in) == 0 {
		f.done = true
		// EOF stands for the client going away, which is what happens after it has said everything
		// it intends to.
		return nil, io.EOF
	}
	req := f.in[0]
	f.in = f.in[1:]
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
	*out = *req
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
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
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
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
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
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
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
	streamCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	stream := newFakeStream(streamCtx,
		openReq(&serverv1.Open{Session: "ro-output", Rows: 24, Cols: 80, ReadOnly: true}),
	)
	_ = svc.Attach(streamCtx, stream)

	var got strings.Builder
	for _, resp := range stream.sent() {
		if o := resp.GetOutput(); o != nil {
			got.WriteString(string(o.Data))
		}
		if op := resp.GetOpened(); op != nil {
			got.WriteString(string(op.Restore))
		}
	}
	if !strings.Contains(got.String(), "WATCHED_OUTPUT") {
		t.Errorf("read-only client saw %q, want the session's output", got.String())
	}
}

// A follower must not be able to end a session by claiming ownership, or watching a build could
// destroy it.
func TestReadOnlyClientCannotOwnSession(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("ro-own", "echo READY; sleep 5"))
	rec.State = "running"
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	svc := NewService(mgr)
	// Both flags together, which is contradictory: a watcher claiming to own what it watches.
	streamCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	stream := newFakeStream(streamCtx,
		openReq(&serverv1.Open{
			Session: "ro-own", Rows: 24, Cols: 80, ReadOnly: true, Own: true,
		}),
	)
	_ = svc.Attach(streamCtx, stream)

	// The session must survive the follower going away.
	if _, live := mgr.Get("ro-own"); !live {
		t.Error("a read-only client that claimed ownership ended the session on disconnect")
	}
	if _, err := st.Get(ctx, "ro-own"); err != nil {
		t.Errorf("session record was removed: %v", err)
	}
}
