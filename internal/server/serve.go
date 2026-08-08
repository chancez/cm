package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/containerd/ttrpc"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Listen binds the server's client-facing socket.
//
// A stale socket left by a server that died is reclaimed, but only after confirming nothing
// answers: unlinking a live server's socket would leave clients unable to find it while it
// kept running.
func Listen(socketPath string) (net.Listener, error) {
	if err := paths.CheckSocketPath(socketPath); err != nil {
		return nil, err
	}
	if conn, err := net.Dial("unix", socketPath); err == nil {
		conn.Close()
		return nil, fmt.Errorf("a server is already listening on %s", socketPath)
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("removing stale socket %s: %w", socketPath, err)
	}

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", socketPath, err)
	}
	// The socket grants control of every session, so restrict it to the owner rather than
	// inheriting the process umask.
	if err := os.Chmod(socketPath, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("restricting %s: %w", socketPath, err)
	}
	return l, nil
}

// Serve runs the server until ctx is cancelled.
//
// Shutdown leaves shims running on purpose. That is what makes an upgrade or restart
// survivable for a shell, and the next server adopts them through Reconcile.
func Serve(ctx context.Context, l net.Listener, svc *Service) error {
	srv, err := ttrpc.NewServer()
	if err != nil {
		return fmt.Errorf("creating ttrpc server: %w", err)
	}
	serverv1.RegisterServerService(srv, svc)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, l) }()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if err != nil && !errors.Is(err, ttrpc.ErrServerClosed) {
			return err
		}
	}

	// A fresh context: the one above is already cancelled, and shutdown still needs to
	// record each session's resume point.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return svc.mgr.Close()
}
