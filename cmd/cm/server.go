package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/containerd/ttrpc"
	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/server"
	"github.com/chancez/cm/internal/store"
	"github.com/chancez/cm/internal/vt"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// serverStartTimeout bounds how long a client waits for a server it just started.
const serverStartTimeout = 10 * time.Second

func newServerCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Run the server in the foreground",
		Long: `Run the server in the foreground.

Normally there is no need to run this: a client starts a server automatically if
one is not already running. Running it explicitly is useful for watching logs and
for development.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return runServer(cmd.Context(), dirs)
		},
	}
}

func runServer(ctx context.Context, dirs paths.Dirs) error {
	if err := dirs.Ensure(); err != nil {
		return err
	}

	// Bind before opening the database so a second server exits immediately rather than
	// touching shared state.
	l, err := server.Listen(dirs.ServerSocket())
	if err != nil {
		return err
	}
	defer os.Remove(dirs.ServerSocket())

	st, err := store.Open(ctx, dirs.Database())
	if err != nil {
		l.Close()
		return err
	}
	defer st.Close()

	mgr, err := server.NewManager(dirs, st, newTerminal)
	if err != nil {
		l.Close()
		return err
	}

	// Adopt sessions whose shims survived a previous server before accepting clients, so a
	// client that reconnects immediately finds its session already present.
	if err := mgr.Reconcile(ctx); err != nil {
		l.Close()
		return err
	}

	return server.Serve(ctx, l, server.NewService(mgr))
}

// withServer runs fn against a server, starting one if needed.
//
// Auto-starting is what lets the user never think about a server: attaching to a session
// works whether or not one is running. The started server is detached from this process so
// it outlives the command that spawned it.
func withServer(
	ctx context.Context,
	dirs paths.Dirs,
	fn func(context.Context, serverv1.ServerClient) error,
) error {
	conn, cl, err := connectServer(ctx, dirs)
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(ctx, cl)
}

func connectServer(ctx context.Context, dirs paths.Dirs) (*ttrpc.Client, serverv1.ServerClient, error) {
	if conn, err := net.Dial("unix", dirs.ServerSocket()); err == nil {
		cl := ttrpc.NewClient(conn)
		return cl, serverv1.NewServerClient(cl), nil
	}

	if err := ensureServer(ctx, dirs); err != nil {
		return nil, nil, err
	}

	conn, err := net.Dial("unix", dirs.ServerSocket())
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to server: %w", err)
	}
	cl := ttrpc.NewClient(conn)
	return cl, serverv1.NewServerClient(cl), nil
}

// ensureServer starts a server and waits for it to accept connections.
//
// Losing the race to start one is not an error: whoever won is serving, which is all the
// caller needs. That is why readiness is a poll on the socket rather than a check on the
// spawned process.
func ensureServer(ctx context.Context, dirs paths.Dirs) error {
	if err := dirs.Ensure(); err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving own path to start a server: %w", err)
	}

	cmd := exec.Command(exe,
		"--runtime-dir", dirs.Runtime,
		"--state-dir", dirs.State,
		"server",
	)
	cmd.SysProcAttr = newDetachedSysProcAttr()
	// Detach stdio: inheriting the client's terminal would tie the server's lifetime to a
	// window, and its output would scribble over the session.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting server: %w", err)
	}
	// Release rather than wait: the server outlives this command by design.
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(serverStartTimeout)
	for {
		if conn, err := net.Dial("unix", dirs.ServerSocket()); err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("server did not become ready within " + serverStartTimeout.String())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// defaultScrollbackLines bounds retained scrollback per session.
//
// Matches tmux's and zmx's default. libghostty prunes at page granularity, so the effective
// limit is somewhat higher.
const defaultScrollbackLines = 2000

// newTerminal builds the terminal model for a session.
//
// This is where cgo enters the server. Keeping it a function passed to the manager means the
// manager, and its tests, do not depend on the emulator.
func newTerminal(rows, cols uint16) (server.Terminal, error) {
	return vt.NewSessionTerminal(rows, cols, defaultScrollbackLines)
}
