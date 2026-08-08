package shim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/ttrpc"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seqlog"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// exitGrace is how long the shim stays reachable after its shell exits.
//
// Long enough for a server that was watching to ask for the exit status, short enough that a shim
// with nobody listening does not linger.
const exitGrace = 2 * time.Second

// subscribeChunkMax bounds how much output one stream message carries.
//
// Output accumulates while nobody is subscribed, so the first message after a resume can
// cover the whole retained log. Splitting it keeps a single message from being both a
// latency spike and a large allocation on the receiver.
const subscribeChunkMax = 64 << 10

// Service adapts a Session to the shim ttrpc API.
type Service struct {
	session *Session
	// shutdown is closed once a Shutdown RPC has been handled, so Serve can return and
	// the process can exit after replying.
	shutdownOnce sync.Once
	shutdown     chan struct{}
}

// NewService wraps a session.
func NewService(s *Session) *Service {
	return &Service{session: s, shutdown: make(chan struct{})}
}

// ShutdownRequested is closed when a client has asked the shim to exit.
func (s *Service) ShutdownRequested() <-chan struct{} { return s.shutdown }

func (s *Service) State(context.Context, *shimv1.StateRequest) (*shimv1.StateResponse, error) {
	oldest, next := s.session.Log().Bounds()
	exited, code := s.session.Exited()
	rows, cols, err := s.session.Size()
	if err != nil {
		// A closed pty cannot report a size, which is expected once the shell has
		// exited. Report zeros rather than failing the whole call, since the caller is
		// usually asking precisely to discover that the session is over.
		rows, cols = 0, 0
	}
	return &shimv1.StateResponse{
		Session:   s.session.cfg.Session,
		ShimPid:   int32(os.Getpid()),
		ShellPid:  int32(s.session.ShellPID()),
		NextSeq:   next,
		OldestSeq: oldest,
		Exited:    exited,
		ExitCode:  int32(code),
		Rows:      uint32(rows),
		Cols:      uint32(cols),
	}, nil
}

// Subscribe streams output from a sequence number and then follows live output.
//
// It returns nil rather than an error when the log closes: the shell exiting is a normal
// end to the stream, and the caller learns the exit status from State.
func (s *Service) Subscribe(ctx context.Context, req *shimv1.SubscribeRequest, srv shimv1.Shim_SubscribeServer) error {
	r := s.session.Log().Subscribe(req.FromSeq)
	defer r.Close()

	for {
		c, err := r.Next(ctx)
		if err != nil {
			if errors.Is(err, seqlog.ErrClosed) {
				return nil
			}
			return err
		}

		// Carry the gap flag on the first message of the split only. The rest are
		// contiguous with it, and marking them too would make a single discontinuity
		// look like several.
		gap := c.Gap
		for off := 0; off < len(c.Data); off += subscribeChunkMax {
			end := min(off+subscribeChunkMax, len(c.Data))
			if err := srv.Send(&shimv1.Output{
				Seq:  c.Seq + uint64(off),
				Data: c.Data[off:end],
				Gap:  gap,
			}); err != nil {
				return err
			}
			gap = false
		}
	}
}

func (s *Service) Write(_ context.Context, req *shimv1.WriteRequest) (*shimv1.WriteResponse, error) {
	n, err := s.session.Write(req.Data)
	if err != nil {
		return nil, err
	}
	return &shimv1.WriteResponse{Written: uint64(n)}, nil
}

func (s *Service) Resize(_ context.Context, req *shimv1.ResizeRequest) (*shimv1.ResizeResponse, error) {
	resize := s.session.Resize
	if req.ForceSignal {
		resize = s.session.ResizeSignal
	}
	if err := resize(
		uint16(req.Rows), uint16(req.Cols),
		uint16(req.XPixel), uint16(req.YPixel),
	); err != nil {
		return nil, err
	}
	return &shimv1.ResizeResponse{}, nil
}

func (s *Service) Signal(_ context.Context, req *shimv1.SignalRequest) (*shimv1.SignalResponse, error) {
	if err := s.session.Signal(syscall.Signal(req.Signal), req.ProcessGroup); err != nil {
		return nil, err
	}
	return &shimv1.SignalResponse{}, nil
}

// Shutdown terminates the shell and asks the shim to exit.
//
// It signals the process group rather than the shell alone so a foreground job and its
// children go too, and it returns before the process exits so the caller gets a reply
// rather than a broken connection.
func (s *Service) Shutdown(_ context.Context, req *shimv1.ShutdownRequest) (*shimv1.ShutdownResponse, error) {
	sig := syscall.SIGHUP
	if req.Force {
		sig = syscall.SIGKILL
	}
	// A shell that has already exited is not an error here: the caller wants the session
	// gone, and it is.
	if err := s.session.Signal(sig, true); err != nil && !errors.Is(err, seqlog.ErrClosed) {
		return nil, err
	}
	s.shutdownOnce.Do(func() { close(s.shutdown) })
	return &shimv1.ShutdownResponse{}, nil
}

// Listen creates the shim's socket.
//
// The socket is bound before the caller reports readiness so there is no window where the
// shim is running but unreachable. A stale socket from a previous shim that died without
// cleaning up is removed first, but only after confirming nothing answers on it: removing
// a live shim's socket would orphan it with no way back.
func Listen(socketPath string) (net.Listener, error) {
	// Check the length first: bind() reports an over-long path as a bare EINVAL, which
	// gives no hint about the actual problem.
	if err := paths.CheckSocketPath(socketPath); err != nil {
		return nil, err
	}
	if conn, err := net.Dial("unix", socketPath); err == nil {
		conn.Close()
		return nil, fmt.Errorf("%s is already served by a live shim", socketPath)
	}
	// Any dial failure means nothing is listening, so the path is safe to reuse.
	// ENOENT is the common case and removing it is harmless.
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing stale socket %s: %w", socketPath, err)
	}

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	// The socket grants control of a shell, so restrict it to the owner. Listen creates
	// it with the process umask, which is typically laxer than that.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("restricting %s: %w", socketPath, err)
	}
	return l, nil
}

// Serve runs the shim until the shell exits, a client requests shutdown, or ctx is done.
func Serve(ctx context.Context, l net.Listener, svc *Service) error {
	srv, err := ttrpc.NewServer()
	if err != nil {
		return fmt.Errorf("creating ttrpc server: %w", err)
	}
	shimv1.RegisterShimService(srv, svc)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, l) }()

	// The shell exiting ends the session, so stop waiting for clients once the log
	// closes. Subscribing from the current end means this does not hold retained output
	// alive or care about history.
	_, end := svc.session.Log().Bounds()
	logDone := make(chan struct{})
	go func() {
		defer close(logDone)
		r := svc.session.Log().Subscribe(end)
		defer r.Close()
		for {
			if _, err := r.Next(ctx); err != nil {
				return
			}
		}
	}()

	select {
	case <-svc.ShutdownRequested():
	case <-logDone:
		// The shell exited. Stay reachable briefly rather than exiting at once: the shim is the only
		// thing that knows the exit status, and the server learns the session ended by this very
		// stream closing, so leaving immediately means it asks a socket that is already gone and
		// records the session as having vanished with no status.
		//
		// A grace period rather than an acknowledgement handshake, because the shim must not depend
		// on a server being there at all: a session whose server is down still has to exit cleanly.
		select {
		case <-time.After(exitGrace):
		case <-svc.ShutdownRequested():
		case <-ctx.Done():
		}
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, ttrpc.ErrServerClosed) {
			return err
		}
	}

	cancel()
	return srv.Shutdown(context.Background())
}
