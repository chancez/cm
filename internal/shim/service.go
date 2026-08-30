package shim

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/fault"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/transport"
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
		Session:  s.session.cfg.Session,
		ShimPid:  int32(os.Getpid()),
		ShellPid: int32(s.session.ShellPID()),
		// The wire carries a plain uint64, so the space is stated here and nowhere else. Both of these
		// are the shim's own numbering: it is the thing doing the numbering. See internal/seq.
		NextSeq:   uint64(next),
		OldestSeq: uint64(oldest),
		Exited:    exited,
		ExitCode:  int32(code),
		Rows:      uint32(rows),
		Cols:      uint32(cols),
		// The build this shim is running, which is not necessarily the server's: a shim keeps its pty
		// across restarts and upgrades, so it outlives the binary that spawned it by design.
		Version: paths.Version(),
		// What this build's shim can do, so the server does not have to infer it from Version above. A
		// version tells it two builds differ; this tells it what differs.
		Capabilities: capability.Shim().Strings(),
	}, nil
}

// Subscribe streams output from a sequence number and then follows live output.
//
// It returns nil rather than an error when the log closes: the shell exiting is a normal
// end to the stream, and the caller learns the exit status from State.
func (s *Service) Subscribe(ctx context.Context, req *shimv1.SubscribeRequest, srv shimv1.Shim_SubscribeServer) error {
	// A resubscribe names a position in the shim's numbering, which is the only space this side knows.
	r := s.session.Log().Subscribe(seq.Shim(req.FromSeq))
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
				Seq:  uint64(c.Seq) + uint64(off),
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
	// An explicit signal wins over what force selected. Checked after force rather than before so a
	// request carrying both is unambiguous, and zero keeps meaning "not specified", which is what an
	// older server sends.
	if req.Signal > 0 {
		sig = syscall.Signal(req.Signal)
	}
	// Signalled through the checking form so a process that declines to leave is reported rather than
	// left as a silent leak. This is the only place that still knows the pty and the process group: the
	// server deletes the session record immediately afterwards, and a stray process then cannot be
	// attributed to cm at all.
	//
	// A shell that has already exited is not an error here: the caller wants the session gone, and it
	// is. Both errors mean that, since Signal reports it one way and the pty guards report it another.
	// Logged before signalling, and unconditionally.
	//
	// A session lost to a signal used to be unattributable. The shim recorded only "shim exiting" with
	// exit_code=-1, which says the shell died by *some* signal and nothing about which, or who asked.
	// `cm kill`, `cm doctor --repair`, and an external `kill` from outside cm all left byte-identical
	// traces, so the first question after losing real work had no answer in any log.
	//
	// Before rather than after, because this is the record that survives the shim dying mid-shutdown, and
	// at Info rather than Debug: it is one line per session lifetime and it is the line someone reads
	// after losing a shell.
	s.session.log.Info("shutdown requested",
		"session", s.session.cfg.Session, "signal", sig, "force", req.Force,
		"explicit_signal", req.Signal > 0)

	pgid, surviving, err := s.session.SignalAndCheck(sig, shutdownGrace)
	if err != nil && !errors.Is(err, seqlog.ErrClosed) && !errors.Is(err, ErrSessionOver) {
		return nil, err
	}
	if len(surviving) > 0 {
		// Logged here as well as returned, because the two reach different people. The reply lets `cm
		// kill` warn the caller now; the log is what is left to find afterwards, when the symptom is an
		// unrelated program failing to allocate a terminal.
		s.session.log.Warn("processes survived the shutdown signal",
			"signal", sig, "pgid", pgid, "surviving", surviving,
			"hint", "they hold a pty; use a stronger signal")
	}
	// Signalled after this handler returns, not here, so the reply is on the wire before Serve starts
	// tearing the RPC server down. Closing it inline is a race the caller loses: ttrpc recomputes a
	// connection's active/idle state only when its write loop next wakes, so a connection whose sole
	// request is still inside its handler is recorded as *idle*, and srv.Shutdown's closeIdleConns
	// closes it out from under the pending response. The caller then sees `ttrpc: closed` for a
	// shutdown that fully happened.
	//
	// That cost `cm doctor --repair` its report: Repair stopped an orphaned shim, took the transport
	// error as failure, and said it did nothing, leaving the operator to believe a shell was still
	// leaked when it had already been reaped. Surfaced as TestRepairStopsOrphansAndSparesHealthySessions
	// failing about 1 run in 8 under parallel load, and measured directly: inserting a 50ms sleep here
	// took shutdownShim from 0/30 failures to 30/30, and a probe confirmed the shim was gone every time
	// the error was returned.
	//
	// AfterFunc rather than a bare goroutine because the delay is the point: it has to outlast this
	// return and the write that follows it. A tiny delay is enough since the write is a local socket,
	// and being late costs nothing, as Serve is only waiting to exit.
	time.AfterFunc(shutdownReplyGrace, func() {
		s.shutdownOnce.Do(func() { close(s.shutdown) })
	})
	return &shimv1.ShutdownResponse{
		SurvivingPids: int32s(surviving),
		SignalledPgid: int32(pgid),
	}, nil
}

// shutdownReplyGrace is how long the shim waits after replying to a Shutdown before letting Serve exit.
//
// Covers one local socket write, so it is generous by orders of magnitude rather than tuned. It only
// delays a process that is already leaving, and nothing waits on it: the shell has been signalled before
// this starts, so the session is over either way. Erring long is therefore free, while erring short
// reintroduces the lost reply.
const shutdownReplyGrace = 50 * time.Millisecond

// shutdownGrace is how long the shim waits before checking what survived its signal.
//
// Short on purpose. It exists only to let a process that is going to die actually die, so it must not be
// mistaken for a shutdown timeout: nothing waits on the outcome except a warning, and the shim still exits
// either way.
const shutdownGrace = 250 * time.Millisecond

// int32s converts pids for the wire.
func int32s(pids []int) []int32 {
	if len(pids) == 0 {
		return nil
	}
	out := make([]int32, 0, len(pids))
	for _, p := range pids {
		out = append(out, int32(p))
	}
	return out
}

// socketRefusalGrace is how long a socket must refuse connections without pause before Listen
// believes nothing is serving it.
//
// A single refusal is not evidence: a unix listener refuses once its accept queue fills, with the
// same errno a socket nobody serves produces. Measured in this tree, a live listener whose queue was
// deliberately filled became dialable again about 11ms after it resumed accepting, so this is an
// order of magnitude above that. The mirror of the server's own constant of the same name, kept
// separate because these are different processes and neither should import the other.
const socketRefusalGrace = 250 * time.Millisecond

// checkSocketUnserved returns an error unless nothing is serving the path.
//
// Retries a refusal rather than trusting it, and the reason is the worst failure in this file. A
// refusal is what a live shim gives while its accept queue is full, and it is also what a socket left
// behind by a dead one gives, so a single dial cannot tell them apart. Treating the first refusal as
// "nothing there" let Listen unlink the socket of a shim that was merely busy, which orphans it with
// no way back: the shell keeps running, holding a pty, on a path nothing can name, where not even
// `cm doctor` can find it since that enumerates sockets.
//
// A sustained refusal is reclaimed rather than reported, which needs justifying because reclaiming
// wrongly is the unrecoverable direction. Two things make it the right call here.
//
// A socket that refuses without pause for the whole grace is not a busy listener. A full accept queue
// drains as soon as the process returns to accepting, measured at about 11ms, so a quarter of a second
// of unbroken refusal means nothing is accepting at all. That is what a shim killed with SIGKILL
// leaves behind, along with anything left by a reboot, and refusing to reclaim it would make the
// session name permanently unusable.
//
// The ambiguous case is also already handled upstream. A shim is spawned by the server, and the
// server calls waitForSocketFree first, which polls with the same distinction and fails the create if
// the path is still held. So a shim reaching this point has been preceded by a check with a longer
// timeout, and the single-refusal misread that motivated all of this cannot survive both.
func checkSocketUnserved(socketPath string) error {
	deadline := time.Now().Add(socketRefusalGrace)
	for {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			conn.Close()
			return fmt.Errorf("%s is already served by a live shim", socketPath)
		}
		// A path that does not exist is conclusive: nothing can be serving it, so it is safe to
		// reclaim. This is the common case, since a shim unlinks its own socket on the way out.
		//
		// Deliberately only this, matching the server's socketAbsent. ENOTSOCK is not portable
		// enough to act on: darwin gives it for a plain file while Linux gives ECONNREFUSED, which
		// is indistinguishable from a stale socket, so relying on it would make the platforms
		// disagree about whether to unlink.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
		if time.Now().After(deadline) {
			// Refused throughout, so nothing is accepting: a stale socket rather than a busy shim.
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Listen creates the shim's socket.
//
// The socket is bound before the caller reports readiness so there is no window where the
// shim is running but unreachable. A stale socket from a previous shim that died without
// cleaning up is removed first, but only after confirming nothing answers on it: removing
// a live shim's socket would orphan it with no way back.
func Listen(socketPath string) (net.Listener, error) {
	// Before the socket exists, so the server sees a shim that is starting and not yet answering. That is
	// the state its ten-second readiness timeout exists for, measured at 10.38s per attempt against 0.36s
	// when a session reference was validated as a name and the shim exited before binding.
	fault.At(fault.BeforeShimReady)

	// Check the length first: bind() reports an over-long path as a bare EINVAL, which
	// gives no hint about the actual problem.
	if err := paths.CheckSocketPath(socketPath); err != nil {
		return nil, err
	}
	if err := checkSocketUnserved(socketPath); err != nil {
		return nil, err
	}
	// Nothing answers, so the path is safe to reuse. ENOENT is the common case and removing
	// it is harmless.
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
	srv, err := transport.NewTTRPCServer()
	if err != nil {
		return err
	}
	shimv1.RegisterShimService(srv.Server, svc)

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
		if err != nil {
			return err
		}
	}

	cancel()
	return srv.Shutdown(context.Background())
}
