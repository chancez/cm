package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/ttrpc"

	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// attachToEndedSession drives one attach against a session whose shell has already exited, and
// returns what the server sent.
//
// The race being reproduced is real but narrow: a command fast enough to finish between Open and
// attach. Rather than trying to win a timing window, the session is ended first and then attached
// to, which puts the server in exactly the state the race produces, deterministically.
func attachToEndedSession(t *testing.T, exitCode int) []*serverv1.AttachResponse {
	t.Helper()

	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("ended", "sleep 5"))
	rec.State = "running"
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("ended")
	if !ok {
		t.Fatal("session was not adopted")
	}

	// Set the terminal state directly rather than waiting for a shell to exit with this code. The
	// code under test reads these fields, and querying the shim for a status it has not produced
	// would only test the shim.
	sess.mu.Lock()
	sess.ended = true
	sess.exitCode = exitCode
	sess.mu.Unlock()

	svc := NewService(mgr)
	stream := newFakeStream(ctx,
		openReq(&serverv1.Open{Session: "ended", Rows: 24, Cols: 80}),
	)
	if err := svc.Attach(ctx, stream); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v, want the exit reported rather than an error", err)
	}
	return stream.sent()
}

// Attaching to a session that has already exited must report the exit status, not fail.
//
// This is the bug: attach returned ErrSessionGone, which surfaced to the user as
// "rpc error: ... session has ended" with exit 1, whatever the command's real status was. It hit
// `cm run` for any command short enough to finish before the attach completed, roughly one run in
// twenty-five locally and more often under load, so a passing `cm run -- true` and a failing
// `cm run -- false` were indistinguishable from each other and from a genuine failure.
func TestAttachToEndedSessionReportsExitStatus(t *testing.T) {
	// 42 rather than 0 or 1: a wrong code has to be distinguishable from both "success" and the
	// generic failure the bug produced.
	sent := attachToEndedSession(t, 42)

	// Opened must come first, so this looks like every other attach and the client needs no special
	// case for a session that ended before it arrived.
	if len(sent) != 2 {
		t.Fatalf("server sent %d messages, want 2 (Opened then Exited): %v", len(sent), sent)
	}
	opened := sent[0].GetOpened()
	if opened == nil {
		t.Fatalf("first message = %v, want Opened", sent[0])
	}
	if opened.Session != "ended" {
		t.Errorf("Opened.Session = %q, want %q", opened.Session, "ended")
	}
	exited := sent[1].GetExited()
	if exited == nil {
		t.Fatalf("second message = %v, want Exited", sent[1])
	}
	if exited.ExitCode != 42 {
		t.Errorf("Exited.ExitCode = %d, want 42", exited.ExitCode)
	}
}

// Every status has to survive the trip, including the two that mean something else elsewhere: 0 is
// success, and -1 is how cm marks "the shim vanished" rather than a real exit.
func TestAttachToEndedSessionPreservesEveryCode(t *testing.T) {
	for _, code := range []int{0, 1, 42, 127, 255, -1} {
		sent := attachToEndedSession(t, code)
		if len(sent) != 2 {
			t.Fatalf("code %d: server sent %d messages, want 2", code, len(sent))
		}
		exited := sent[1].GetExited()
		if exited == nil {
			t.Fatalf("code %d: second message = %v, want Exited", code, sent[1])
		}
		if int(exited.ExitCode) != code {
			t.Errorf("Exited.ExitCode = %d, want %d", exited.ExitCode, code)
		}
	}
}

// The sizing failure must be tolerated when the session ends during the attach, and reported when it
// does not.
//
// Driven by a fake shim rather than a real one, because the window is between attach and the resize
// and there is no seam there to hook from outside. The fake's Resize marks the session ended and then
// fails, which is precisely what the race produces: attach succeeded against a live session, and by
// the time the resize was issued the shell was gone.
//
// Both directions are tested, since a fix that ignored every sizing error would pass the tolerant
// case while hiding a real failure on a session that is still running.
func TestSizingFailureIsFatalOnlyWhenTheSessionLives(t *testing.T) {
	for _, tc := range []struct {
		name string
		// endOnResize marks the session ended from inside the failing Resize, standing in for the
		// shell exiting mid-attach.
		endOnResize bool
		wantSizeErr bool
	}{
		// The race: the session ended, so the exit status is what the client needs.
		{name: "ended during attach reports the exit", endOnResize: true, wantSizeErr: false},
		// Not the race: a live session that cannot be sized is a real problem, because the client
		// would be looking at a shell wrapped for someone else's width.
		{name: "live session reports the failure", endOnResize: false, wantSizeErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, _, _ := newTestManager(t, nil)
			ctx := context.Background()

			fake := &resizeFailShim{}
			socket := serveFakeShim(t, fake)

			// Built directly rather than through the manager, so the fake shim is what the session
			// talks to. 24x80 here against 40x100 below, so the resize actually runs.
			sess, err := newSession(store.Session{
				Name: "size-race", ShimSocket: socket, Rows: 24, Cols: 80,
			}, nil, 0)
			if err != nil {
				t.Fatalf("newSession() error = %v", err)
			}
			t.Cleanup(sess.Close)
			// Registered by hand rather than via Open, which would dial the real shim binary.
			mgr.mu.Lock()
			mgr.sessions[sess.name] = sess
			mgr.mu.Unlock()

			if tc.endOnResize {
				fake.onResize = func() {
					sess.mu.Lock()
					sess.ended = true
					sess.exitCode = 7
					sess.mu.Unlock()
				}
			}

			svc := NewService(mgr)
			streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			stream := newHeldFakeStream(streamCtx,
				openReq(&serverv1.Open{Session: "size-race", Rows: 40, Cols: 100}),
			)
			done := make(chan error, 1)
			go func() { done <- svc.Attach(streamCtx, stream) }()

			// Wait for the resize to have been attempted rather than for the attach to return. In the
			// tolerated case the attach correctly keeps streaming, so it does not return until the
			// stream is closed, and waiting on it first would just burn the timeout every run.
			deadline := time.Now().Add(5 * time.Second)
			for !fake.resized.Load() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			stream.closeStream()

			var attachErr error
			select {
			case attachErr = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Attach() did not return after the stream closed")
			}

			if !fake.resized.Load() {
				t.Fatal("the shim was never asked to resize, so the path under test did not run")
			}
			gotSizeErr := attachErr != nil && strings.Contains(attachErr.Error(), "sizing session")
			if gotSizeErr != tc.wantSizeErr {
				t.Errorf("Attach() error = %v, want a sizing failure = %v", attachErr, tc.wantSizeErr)
			}
		})
	}
}

// resizeFailShim is a shim whose Resize always fails, optionally running a hook first.
//
// Only Resize and the calls an attach makes on the way there are implemented; the rest return errors,
// since reaching them would mean the test is exercising something other than what it claims.
type resizeFailShim struct {
	// onResize runs before Resize fails, so a test can change the session's state inside the window
	// between attach and sizing.
	onResize func()
	resized  atomic.Bool
}

func (f *resizeFailShim) Resize(context.Context, *shimv1.ResizeRequest) (*shimv1.ResizeResponse, error) {
	f.resized.Store(true)
	if f.onResize != nil {
		f.onResize()
	}
	// The error a shim whose process is gone actually produces.
	return nil, errors.New("ttrpc: closed")
}

func (f *resizeFailShim) State(context.Context, *shimv1.StateRequest) (*shimv1.StateResponse, error) {
	return &shimv1.StateResponse{Rows: 24, Cols: 80}, nil
}

// Subscribe blocks rather than returning, since a shim that ended its output stream would end the
// session and defeat the point of the test.
func (f *resizeFailShim) Subscribe(
	ctx context.Context, _ *shimv1.SubscribeRequest, _ shimv1.Shim_SubscribeServer,
) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *resizeFailShim) Write(context.Context, *shimv1.WriteRequest) (*shimv1.WriteResponse, error) {
	return &shimv1.WriteResponse{}, nil
}

func (f *resizeFailShim) Signal(context.Context, *shimv1.SignalRequest) (*shimv1.SignalResponse, error) {
	return &shimv1.SignalResponse{}, nil
}

func (f *resizeFailShim) Shutdown(
	context.Context, *shimv1.ShutdownRequest,
) (*shimv1.ShutdownResponse, error) {
	return &shimv1.ShutdownResponse{}, nil
}

// serveFakeShim serves a fake shim implementation on a fresh socket and returns its path.
//
// Registered against ttrpc directly rather than through shim.Serve, which takes the concrete shim
// service and so cannot host a fake.
func serveFakeShim(t *testing.T, svc shimv1.ShimService) string {
	t.Helper()

	socket := filepath.Join(shortTempDir(t), "s.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	srv, err := ttrpc.NewServer()
	if err != nil {
		l.Close()
		t.Fatalf("ttrpc.NewServer() error = %v", err)
	}
	shimv1.RegisterShimService(srv, svc)

	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(context.Background(), l)
	}()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		select {
		case <-served:
		case <-time.After(5 * time.Second):
		}
	})
	waitSocket(t, socket)
	return socket
}

// The server must acknowledge a detach that asked for one.
//
// No client asks any more: the acknowledgement existed so an owned session was not reaped when its
// Detach was discarded with the connection, and ownership is gone. Kept as a protocol test rather than
// deleted with the feature, because `no_ack` is a field a client can set either way and the false branch
// would otherwise be unexercised -- a server that stopped replying entirely would look correct.
func TestDetachIsAcknowledged(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("ackdetach", "sleep 5"))
	rec.State = "running"
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	svc := NewService(mgr)
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream := newFakeStream(streamCtx,
		openReq(&serverv1.Open{Session: "ackdetach", Rows: 24, Cols: 80}),
		&serverv1.AttachRequest{Event: &serverv1.AttachRequest_Detach{Detach: &serverv1.Detach{}}},
	)
	if err := svc.Attach(streamCtx, stream); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}

	var acked bool
	for _, resp := range stream.sent() {
		if resp.GetDetached() != nil {
			acked = true
		}
	}
	if !acked {
		t.Errorf("server sent %d messages and none was Detached, want the detach acknowledged",
			len(stream.sent()))
	}

	// And the session survived, which is what any detach must leave alone.
	if sess, ok := mgr.Get("ackdetach"); !ok {
		t.Error("the session was destroyed by a deliberate detach")
	} else if ended, _ := sess.Ended(); ended {
		t.Error("the session ended after a deliberate detach, want it still running")
	}
}

// A client that says it will not wait gets no acknowledgement, and no warning is logged.
//
// The complement of TestDetachIsAcknowledged, and the deterministic form of a real complaint. Three clients --
// `cm run -d`, `cm attach --no-attach`, and an interrupted follower -- detach as their last act and exit
// without reading the reply. The server's send then lost a race about 40% of the time, measured at 8 of 20
// runs, and each failure logged a warning for behavior that was entirely intended. Worse, it made `cm doctor`
// report log-warnings on an installation where nothing was wrong.
//
// Asserted here rather than only end to end because the bug was probabilistic: the e2e test needs several
// rounds to be likely to catch it, while this is exact. Both exist, since the e2e test is what proves the noise
// is actually gone from ordinary use.
func TestDetachWithNoAckIsNotAcknowledged(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("noack", "sleep 5"))
	rec.State = "running"
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// The server's own log, so the absence of a warning can be asserted rather than assumed.
	var logged bytes.Buffer
	mgr.SetLogger(slog.New(slog.NewTextHandler(&logged, nil)))

	svc := NewService(mgr)
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream := newFakeStream(streamCtx,
		openReq(&serverv1.Open{Session: "noack", Rows: 24, Cols: 80}),
		&serverv1.AttachRequest{
			Event: &serverv1.AttachRequest_Detach{Detach: &serverv1.Detach{NoAck: true}},
		},
	)
	if err := svc.Attach(streamCtx, stream); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}

	for _, resp := range stream.sent() {
		if resp.GetDetached() != nil {
			t.Error("server acknowledged a detach that asked for no acknowledgement")
		}
	}
	// And nothing was warned about, which is the point: the log is quiet because nothing failed rather than
	// because a real failure was downgraded.
	if strings.Contains(logged.String(), "acknowledging a detach failed") {
		t.Errorf("server warned about a detach that asked for no acknowledgement:\n%s", logged.String())
	}

	// The session still survives, which no longer depends on the reply at all.
	if sess, ok := mgr.Get("noack"); !ok {
		t.Error("the session was destroyed by a detach that wanted no acknowledgement")
	} else if ended, _ := sess.Ended(); ended {
		t.Error("the session ended after a deliberate detach, want it still running")
	}
}
