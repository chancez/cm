package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/transport"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Listen binds the server's client-facing socket and reports the inode it bound.
//
// A stale socket left by a server that died is reclaimed, but only after confirming nothing
// answers: unlinking a live server's socket would leave clients unable to find it while it
// kept running.
//
// The inode is returned rather than left for the caller to stat, so it is read while this process still holds
// the only claim on the path. A caller that stat'd it later could race a replacement and record the wrong
// one, which would make the socket check report a problem that does not exist.
func Listen(socketPath string) (net.Listener, uint64, error) {
	if err := paths.CheckSocketPath(socketPath); err != nil {
		return nil, 0, err
	}
	if conn, err := net.Dial("unix", socketPath); err == nil {
		conn.Close()
		return nil, 0, fmt.Errorf("a server is already listening on %s", socketPath)
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, fmt.Errorf("removing stale socket %s: %w", socketPath, err)
	}

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, 0, fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	// The socket grants control of every session, so restrict it to the owner rather than
	// inheriting the process umask.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		l.Close()
		return nil, 0, fmt.Errorf("restricting %s: %w", socketPath, err)
	}
	ino, err := socketInode(socketPath)
	if err != nil {
		// Not fatal. The consequence is one diagnostic check that cannot run, which is a worse outcome than
		// refusing to serve at all.
		return l, 0, nil
	}
	return l, ino, nil
}

// Serve runs the server until ctx is cancelled.
//
// Shutdown leaves shims running on purpose. That is what makes an upgrade or restart
// survivable for a shell, and the next server adopts them through Reconcile.
func Serve(ctx context.Context, l net.Listener, svc *Service) error {
	srv, err := transport.NewTTRPCServer()
	if err != nil {
		return err
	}
	// Registration stays transport-specific, since the generated code is: each plugin emits its own
	// service interface and its own registration function. The lifecycle around it is what the
	// transport package abstracts.
	serverv1.RegisterServerService(srv.Server, svc)

	// A Shutdown RPC cancels this rather than the caller's context, so the paths for "asked to
	// stop" and "signalled to stop" converge on the same orderly shutdown below.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	svc.setStop(stop)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, l) }()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		// Already normalized: the transport reports an ordinary shutdown as nil rather than its own
		// sentinel, so there is nothing transport-specific to compare against here.
		if err != nil {
			return err
		}
	}

	// A fresh context: the one above is already cancelled, and shutdown still needs to
	// record each session's resume point.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Resume points are written *before* the transport shuts down, and the order is the whole point.
	//
	// ttrpc.Shutdown closes the listeners first and only then waits for connections to go idle, so the
	// socket stops accepting at its very first step. `cm server restart` decides the old server is gone
	// by dialing (waitServerGone), so it proceeds as soon as that happens, and the new server's
	// Reconcile then reads last_seq and client_seq while this one had not written them yet. The next
	// server therefore resubscribed from a stale position, or from zero for a session whose points had
	// never been written at all.
	//
	// The symptom was not an error anywhere. Each adopted session came back with an "output gap
	// detected" repaint instead of a seamless resume, which looks like the gap detection working rather
	// than like a resume point that was never saved. Found in a live restart at 17:12:06 where sessions
	// were adopted with from_seq=0 while the store held positions in the millions, and the 26ms between
	// "shutting down on request" and "server starting" is the window.
	//
	// Closing the manager first costs nothing: it stops consuming shim output and writes each position,
	// and sessions are deliberately left running either way. An in-flight client call can still be
	// served by the transport shutdown below, and a client whose session stopped being consumed
	// reconnects to the new server, which is the same thing it does across any restart.
	closeErr := svc.mgr.Close()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return closeErr
}
