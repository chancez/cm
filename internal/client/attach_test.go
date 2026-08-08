package client

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chancez/cm/internal/transport"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Attach wraps runSession with the dial-and-retry loop: it decides whether an unreachable server is a hard
// failure or an outage to wait out, and it holds the resume point and pending input across connections.
//
// Tested against a real socket rather than a fake, because dialing is the part under test. The service on the
// other end is a stub that scripts one attachment, so the arrangement stays about connection lifecycle rather
// than session behavior, which session_test.go covers.

// stubService is a ServerService that scripts the Attach stream.
type stubService struct {
	// handle runs one attachment. Called once per connection, with the connection's ordinal.
	handle func(n int, srv serverv1.Server_AttachServer) error

	mu sync.Mutex
	// opens records the Open message from each connection, so a test can assert what the client asked for
	// the second time around.
	opens []*serverv1.Open
	// conns counts connections, which is how "did it reconnect?" is answered.
	conns int
}

func (s *stubService) Attach(_ context.Context, srv serverv1.Server_AttachServer) error {
	s.mu.Lock()
	s.conns++
	n := s.conns
	s.mu.Unlock()

	// Every attachment begins with the client's Open.
	req, err := srv.Recv()
	if err != nil {
		return err
	}
	open := req.GetOpen()
	if open == nil {
		return errors.New("first message was not an Open")
	}
	s.mu.Lock()
	s.opens = append(s.opens, open)
	s.mu.Unlock()

	return s.handle(n, srv)
}

func (s *stubService) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func (s *stubService) openMessages() []*serverv1.Open {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*serverv1.Open(nil), s.opens...)
}

// Unused by these tests: Attach is the only method the client calls here.
func (s *stubService) List(context.Context, *serverv1.ListRequest) (*serverv1.ListResponse, error) {
	panic("unused")
}
func (s *stubService) Kill(context.Context, *serverv1.KillRequest) (*serverv1.KillResponse, error) {
	panic("unused")
}
func (s *stubService) Send(context.Context, *serverv1.SendRequest) (*serverv1.SendResponse, error) {
	panic("unused")
}
func (s *stubService) History(
	context.Context, *serverv1.HistoryRequest,
) (*serverv1.HistoryResponse, error) {
	panic("unused")
}
func (s *stubService) GetEnv(
	context.Context, *serverv1.GetEnvRequest,
) (*serverv1.GetEnvResponse, error) {
	panic("unused")
}
func (s *stubService) Report(
	context.Context, *serverv1.ReportRequest,
) (*serverv1.ReportResponse, error) {
	panic("unused")
}
func (s *stubService) Read(context.Context, *serverv1.ReadRequest) (*serverv1.ReadResponse, error) {
	panic("unused")
}
func (s *stubService) Wait(context.Context, *serverv1.WaitRequest) (*serverv1.WaitResponse, error) {
	panic("unused")
}
func (s *stubService) Doctor(
	context.Context, *serverv1.DoctorRequest,
) (*serverv1.DoctorResponse, error) {
	panic("unused")
}
func (s *stubService) Status(
	context.Context, *serverv1.StatusRequest,
) (*serverv1.StatusResponse, error) {
	panic("unused")
}
func (s *stubService) Shutdown(
	context.Context, *serverv1.ShutdownRequest,
) (*serverv1.ShutdownResponse, error) {
	panic("unused")
}

var _ serverv1.ServerService = (*stubService)(nil)

// serveStub starts a server on a fresh socket and returns its path.
func serveStub(t *testing.T, svc *stubService) string {
	t.Helper()

	// os.MkdirTemp with a short prefix rather than t.TempDir(), which embeds the test name and can exceed the
	// 104-byte sockaddr_un limit. That failure arrives as a bare EINVAL.
	dir, err := os.MkdirTemp("", "cmc")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	serveStubOn(t, socket, svc)
	return socket
}

// serveStubOn serves on a given socket path and returns a function that stops it.
//
// Separate from serveStub so a test can take the server away mid-run, which is the only way to reach the
// dial-retry loop: a server that keeps accepting sends the client down the reconnect path instead.
func serveStubOn(t *testing.T, socket string, svc *stubService) func() {
	t.Helper()

	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	srv, err := transport.NewTTRPCServer()
	if err != nil {
		t.Fatalf("NewTTRPCServer() error = %v", err)
	}
	serverv1.RegisterServerService(srv.Server, svc)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(ctx, l)
	}()

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
			defer done()
			_ = srv.Shutdown(shutdownCtx)
			<-served
			// Unlinked explicitly, so a later dial fails rather than finding a socket nothing answers on.
			_ = os.Remove(socket)
		})
	}
	t.Cleanup(stop)
	return stop
}

// attachOpts returns Options pointing at a socket, with a pipe-backed TTY.
func attachOpts(t *testing.T, socket string) (*TTY, Options) {
	t.Helper()

	// A pipe whose write end is closed, so input is exhausted immediately. On a non-terminal that does not end
	// the attachment, which is what lets these tests be driven entirely by the server's messages.
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	inW.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	// Drained, or a write from the client blocks once the pipe buffer fills.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := outR.Read(buf); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		inR.Close()
		outR.Close()
		outW.Close()
	})

	tty, err := OpenTTY(inR, outW)
	if err != nil {
		t.Fatalf("OpenTTY() error = %v", err)
	}
	return tty, Options{Session: "test", SocketPath: socket}
}

// sendOpened is the server's first reply, which every attachment needs.
func sendOpened(srv serverv1.Server_AttachServer, session string, nextSeq uint64) error {
	return srv.Send(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Opened{Opened: &serverv1.Opened{
			Session: session, NextSeq: nextSeq,
		}},
	})
}

// An unreachable server on the first attempt is a hard failure, not a wait.
//
// The distinction is the whole point of the deadline being zero until something has connected. A first attempt
// that retried for thirty seconds would make a typo in a socket path look like a hang, and `cm attach` against
// a stopped server would appear to freeze rather than report the problem.
func TestAttachFailsImmediatelyWhenTheServerWasNeverReachable(t *testing.T) {
	tty, opts := attachOpts(t, filepath.Join(t.TempDir(), "nope.sock"))

	start := time.Now()
	_, err := Attach(context.Background(), tty, opts)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Attach() error = nil, want a failure for an unreachable server")
	}
	// Well under reconnectTimeout, which is what shows it did not enter the retry loop. Generous, since this
	// only has to distinguish "returned promptly" from "waited out a 30s budget".
	if elapsed > 5*time.Second {
		t.Errorf("Attach() took %s to fail, want a prompt failure rather than the %s retry budget",
			elapsed, reconnectTimeout)
	}
}

// A session that ends is reported with its exit status, over a real connection.
//
// The end-to-end path through dial, so a break in transport.DialServer or in how Attach hands off to runSession
// fails here rather than only in the e2e suite.
func TestAttachReportsExitOverARealSocket(t *testing.T) {
	svc := &stubService{handle: func(_ int, srv serverv1.Server_AttachServer) error {
		if err := sendOpened(srv, "test", 0); err != nil {
			return err
		}
		return srv.Send(&serverv1.AttachResponse{
			Event: &serverv1.AttachResponse_Exited{Exited: &serverv1.Exited{ExitCode: 3}},
		})
	}}
	socket := serveStub(t, svc)
	tty, opts := attachOpts(t, socket)

	result, err := Attach(context.Background(), tty, opts)
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	if !result.Exited || result.ExitCode != 3 {
		t.Errorf("result = %+v, want Exited with code 3", result)
	}
	if n := svc.connections(); n != 1 {
		t.Errorf("made %d connections, want 1: a finished session must not be reconnected to", n)
	}
}

// A dropped connection is retried, and the retry resumes from where the client had read to.
//
// This is the mechanism behind an upgrade being invisible. The resume point has to survive the reconnect and be
// sent on the next Open, or the session repaints from the beginning of what the shim retained.
func TestAttachReconnectsAndResumes(t *testing.T) {
	svc := &stubService{handle: func(n int, srv serverv1.Server_AttachServer) error {
		if n == 1 {
			if err := sendOpened(srv, "test", 100); err != nil {
				return err
			}
			// Some output, then the stream ends as a restarting server's would.
			return srv.Send(&serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Output{Output: &serverv1.Output{
					Seq: 100, Data: []byte("before"),
				}},
			})
		}
		if err := sendOpened(srv, "test", 106); err != nil {
			return err
		}
		return srv.Send(&serverv1.AttachResponse{
			Event: &serverv1.AttachResponse_Exited{Exited: &serverv1.Exited{ExitCode: 0}},
		})
	}}
	socket := serveStub(t, svc)
	tty, opts := attachOpts(t, socket)

	result, err := Attach(context.Background(), tty, opts)
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}
	if !result.Exited {
		t.Errorf("result = %+v, want the second connection's exit observed", result)
	}
	if n := svc.connections(); n != 2 {
		t.Fatalf("made %d connections, want 2: the dropped stream must be retried", n)
	}

	opens := svc.openMessages()
	if len(opens) != 2 {
		t.Fatalf("got %d Open messages, want 2", len(opens))
	}
	// The first asks for nothing in particular; the second asks for exactly what was missed.
	if opens[0].ResumeFromSeq != nil {
		t.Errorf("first Open.ResumeFromSeq = %d, want nil on a fresh attachment", *opens[0].ResumeFromSeq)
	}
	if opens[1].ResumeFromSeq == nil {
		t.Fatal("second Open.ResumeFromSeq is nil, so the reconnect would repaint from the beginning")
	}
	// 100 + len("before") == 106.
	if *opens[1].ResumeFromSeq != 106 {
		t.Errorf("second Open.ResumeFromSeq = %d, want 106 (one past the last byte received)",
			*opens[1].ResumeFromSeq)
	}
}

// A cancelled context ends the attachment rather than dialing out the whole retry budget.
//
// Without this a client asked to stop during an outage would keep dialing for up to thirty seconds, so SIGTERM
// would appear to be ignored.
//
// The server has to actually go away, which took a correction: an earlier version of this test used a stub that
// kept accepting, so cancellation landed during a live session and correctly *detached*, returning nil. That is
// right behavior and a different path. Reaching the dial-retry loop means there is nothing to dial.
func TestAttachStopsOnContextCancelDuringAnOutage(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmc")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")

	// A server that accepts one attachment, drops it, and then stops listening. The client establishes a
	// connection, which is what starts the retry budget, and then finds nothing to dial.
	svc := &stubService{handle: func(_ int, srv serverv1.Server_AttachServer) error {
		return sendOpened(srv, "test", 0)
	}}
	stop := serveStubOn(t, socket, svc)

	tty, opts := attachOpts(t, socket)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Attach(ctx, tty, opts)
		done <- err
	}()

	// Wait for the connection, so the client is past its first dial and the deadline has started.
	deadline := time.Now().Add(5 * time.Second)
	for svc.connections() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if svc.connections() < 1 {
		t.Fatal("the client never connected, so cancellation would hit the first-attempt path instead")
	}

	// Now the server is gone, so every retry fails to dial.
	stop()

	// Long enough for at least one failed dial, and far short of the 30s budget: if cancellation were ignored,
	// the call would run for the full budget rather than returning here.
	time.Sleep(3 * reconnectInterval)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Attach() error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Errorf("Attach() did not return within 10s of cancellation, want it to stop rather than dial out "+
			"the %s budget", reconnectTimeout)
	}
}

// Options are sent on every connection, not just the first.
//
// A reconnect that dropped them would silently change the attachment: a read-only watcher would become a
// writer, and an owned session would stop being owned and outlive its client.
func TestAttachResendsOptionsOnReconnect(t *testing.T) {
	svc := &stubService{handle: func(n int, srv serverv1.Server_AttachServer) error {
		if n == 1 {
			if err := sendOpened(srv, "test", 0); err != nil {
				return err
			}
			return nil // drop, forcing a reconnect
		}
		if err := sendOpened(srv, "test", 0); err != nil {
			return err
		}
		return srv.Send(&serverv1.AttachResponse{
			Event: &serverv1.AttachResponse_Exited{Exited: &serverv1.Exited{ExitCode: 0}},
		})
	}}
	socket := serveStub(t, svc)
	tty, opts := attachOpts(t, socket)
	opts.Own = true
	opts.ReadOnly = true

	if _, err := Attach(context.Background(), tty, opts); err != nil {
		t.Fatalf("Attach() error = %v", err)
	}

	opens := svc.openMessages()
	if len(opens) != 2 {
		t.Fatalf("got %d Open messages, want 2", len(opens))
	}
	for i, open := range opens {
		if !open.Own {
			t.Errorf("Open %d has Own = false, want it preserved across the reconnect", i+1)
		}
		if !open.ReadOnly {
			t.Errorf("Open %d has ReadOnly = false, want it preserved across the reconnect", i+1)
		}
	}
}
