package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/containerd/ttrpc"
	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/config"
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
			cfg, err := g.config()
			if err != nil {
				return err
			}
			return runServer(cmd.Context(), dirs, cfg, true)
		},
	}
}

func runServer(ctx context.Context, dirs paths.Dirs, cfg *config.Config, foreground bool) error {
	if err := dirs.Ensure(); err != nil {
		return err
	}

	level, enabled, err := cfg.Logging()
	if err != nil {
		return err
	}
	logger, closeLog, err := cmlog.New(cmlog.Options{
		Path:    dirs.ServerLog(),
		Level:   level,
		Enabled: enabled,
		// A server run in the foreground is being watched, so send it to both.
		Stderr: foreground,
	})
	if err != nil {
		return err
	}
	defer closeLog.Close()

	// Bind before opening the database so a second server exits immediately rather than
	// touching shared state.
	l, err := server.Listen(dirs.ServerSocket())
	if err != nil {
		return err
	}
	defer os.Remove(dirs.ServerSocket())

	logger.Info("server starting", "pid", os.Getpid(), "socket", dirs.ServerSocket())

	st, err := store.Open(ctx, dirs.Database())
	if err != nil {
		l.Close()
		return err
	}
	defer st.Close()

	mgr, err := server.NewManager(dirs, st, terminalFactory(cfg))
	if err != nil {
		l.Close()
		return err
	}
	mgr.SetLogger(logger)

	resizePolicy, err := cfg.Resize()
	if err != nil {
		l.Close()
		return err
	}
	mgr.SetResizePolicy(server.ResizePolicy(resizePolicy))

	if cfg.Persist.Enabled {
		policy, err := persistPolicy(cfg)
		if err != nil {
			l.Close()
			return err
		}
		mgr.SetPersistPolicy(policy)
	}

	// Adopt sessions whose shims survived a previous server before accepting clients, so a
	// client that reconnects immediately finds its session already present.
	if err := mgr.Reconcile(ctx); err != nil {
		l.Close()
		return err
	}

	// Expire on startup and then periodically. Startup is when a reboot's accumulation is visible,
	// and the ticker covers a server that stays up for weeks.
	if cfg.Persist.Enabled {
		if _, err := mgr.ExpireDeadSessions(ctx, time.Now()); err != nil {
			// Not fatal: failing to clean up is worth reporting but not worth refusing to serve.
			logger.Warn("expiring sessions failed", "error", err)
		}
		go expirePeriodically(ctx, mgr, logger)
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

// dialServer connects to a running server without starting one.
//
// Used by completion, which runs on every tab press: a stray keystroke must not launch a daemon, and
// if none is running there is nothing to complete.
func dialServer(dirs paths.Dirs) (*ttrpc.Client, serverv1.ServerClient, error) {
	conn, err := net.Dial("unix", dirs.ServerSocket())
	if err != nil {
		return nil, nil, err
	}
	cl := ttrpc.NewClient(conn)
	return cl, serverv1.NewServerClient(cl), nil
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

// expireInterval is how often dead sessions are swept while the server runs.
//
// Hourly rather than more often: expiry is measured in days, so a long interval costs nothing and
// keeps a mostly-idle server mostly idle.
const expireInterval = time.Hour

// expirePeriodically sweeps dead sessions until the context is cancelled.
func expirePeriodically(ctx context.Context, mgr *server.Manager, logger *slog.Logger) {
	t := time.NewTicker(expireInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := mgr.ExpireDeadSessions(ctx, time.Now()); err != nil {
				logger.Warn("expiring sessions failed", "error", err)
			}
		}
	}
}

// persistPolicy translates configuration into the policy the manager uses.
//
// Kept here rather than in the manager so the manager, and its tests, do not depend on the config
// package or on a file existing.
func persistPolicy(cfg *config.Config) (*server.PersistPolicy, error) {
	mode, err := cfg.RestoreMode()
	if err != nil {
		return nil, err
	}
	expire, err := cfg.ExpireAfter()
	if err != nil {
		return nil, err
	}

	return &server.PersistPolicy{
		Matches:              cfg.PersistsSession,
		Limits:               cfg.PersistLimits(),
		OnRestore:            server.RestoreAction(mode),
		CommandIsSafeToRerun: cfg.CommandIsSafeToRerun,
		ExpireAfter:          expire,
	}, nil
}

// terminalFactory builds the function the manager uses to create terminal models.
//
// This is where cgo enters the server. Passing it in means the manager, and its tests, do not
// depend on the emulator.
func terminalFactory(cfg *config.Config) server.NewTerminalFunc {
	scrollback := cfg.Scrollback()
	return func(rows, cols uint16) (server.Terminal, error) {
		return vt.NewSessionTerminal(rows, cols, scrollback)
	}
}
