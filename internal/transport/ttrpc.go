package transport

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/containerd/ttrpc"
)

// TTRPCServer wraps a ttrpc server as a Server.
//
// The wrapper exists to normalize two things. Its clean-shutdown sentinel is ttrpc.ErrServerClosed,
// which callers should not have to know about, and its Serve returns that error rather than nil for an
// ordinary stop.
type TTRPCServer struct {
	*ttrpc.Server
}

// NewTTRPCServer returns a ttrpc server.
func NewTTRPCServer() (*TTRPCServer, error) {
	srv, err := ttrpc.NewServer()
	if err != nil {
		return nil, fmt.Errorf("creating ttrpc server: %w", err)
	}
	return &TTRPCServer{Server: srv}, nil
}

// Serve handles calls until the listener closes or ctx is cancelled.
func (s *TTRPCServer) Serve(ctx context.Context, l net.Listener) error {
	err := s.Server.Serve(ctx, l)
	if errors.Is(err, ttrpc.ErrServerClosed) {
		// An ordinary shutdown, reported as success so callers need not know this transport's sentinel.
		return nil
	}
	return err
}

// DialTTRPC connects to a unix socket and returns a ttrpc client.
//
// Returns the concrete *ttrpc.Client rather than the Conn interface, because the generated client
// constructors take that type. Callers hold it only to close it and to hand to those constructors.
func DialTTRPC(socketPath string) (*ttrpc.Client, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", socketPath, err)
	}
	return ttrpc.NewClient(conn), nil
}
