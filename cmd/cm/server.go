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

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/server"
	"github.com/chancez/cm/internal/store"
	"github.com/chancez/cm/internal/transport"
	"github.com/chancez/cm/internal/vt"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// serverStartTimeout bounds how long a client waits for a server it just started.
const serverStartTimeout = 10 * time.Second

func newServerCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
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
	cmd.AddCommand(newServerStopCommand(g))
	cmd.AddCommand(newServerRestartCommand(g))
	return cmd
}

func newServerRestartCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Replace the running server, leaving sessions running",
		Long: `Stop the server and start a new one. Sessions keep running.

This is what an upgrade looks like. Each session's shim owns its pty, so the shell
is untouched: the new server adopts every session, and an attached client
reconnects on its own without anything being typed.

No process hand-off is involved, and none is needed. Stopping and starting takes a
few tens of milliseconds, well inside the window a client will wait through, and
the shim holding the pty means there are no file descriptors to pass.

What this adds over running stop and then a command yourself is waiting for the old
server to release its socket. Without the wait a restart still works, measured
repeatedly and even with shutdown deliberately slowed, because a new server refuses
to bind while something answers and the start path retries for ten seconds. The wait
makes that deterministic rather than dependent on a retry loop absorbing it.

Starts a server even if none was running, since the caller wants one running
afterwards either way.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}

			// Stop only if one is there. A restart with nothing running is a start, not an error.
			if conn, cl, cerr := connectServer(cmd.Context(), dirs); cerr == nil {
				_, serr := cl.Shutdown(cmd.Context(), &serverv1.ShutdownRequest{})
				conn.Close()
				if serr != nil {
					return fmt.Errorf("stopping the running server: %w", serr)
				}
				// Waited on so the start below is not racing the shutdown.
				//
				// Not strictly required, and the comment said it was until it was measured: without this a
				// restart still succeeded 15 times out of 15, and still succeeded with shutdown slowed by
				// 300ms on purpose. The reason is that a new server refuses to bind while something answers
				// on the socket, and ensureServer retries for ten seconds, so a lost race is absorbed rather
				// than reported.
				//
				// Kept because absorbing a race is not the same as not having one: the recovery costs a
				// startup attempt and depends on a timeout being generous. Waiting is a few milliseconds and
				// makes the ordering explicit.
				if err := waitServerGone(cmd.Context(), dirs); err != nil {
					return err
				}
			}

			// ensureServer re-execs this binary, which reads the config itself, so nothing has to be
			// passed through here.
			if err := ensureServer(cmd.Context(), dirs); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, "server restarted")
			return nil
		},
	}
}

// serverGoneTimeout bounds how long a restart waits for the old server to release its socket.
//
// Generous relative to the few tens of milliseconds a shutdown takes, because the cost of being wrong is
// asymmetric: waiting a moment longer is invisible, while giving up early leaves no server running at all.
const serverGoneTimeout = 10 * time.Second

// waitServerGone blocks until nothing answers on the server socket.
//
// Dialing rather than watching for the file to disappear. A socket file can outlive the process that bound it,
// so its presence says nothing; whether a connection is accepted is the only reliable signal that the old
// server has actually let go.
func waitServerGone(ctx context.Context, dirs paths.Dirs) error {
	deadline := time.Now().Add(serverGoneTimeout)
	for {
		conn, err := net.Dial("unix", dirs.ServerSocket())
		if err != nil {
			return nil
		}
		conn.Close()

		if time.Now().After(deadline) {
			return fmt.Errorf("the running server did not stop within %s", serverGoneTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func newServerStopCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the server, leaving sessions running",
		Long: `Stop the server. Sessions keep running.

Each session's shim owns its pty, so a server exiting leaves every session intact
and the next server adopts them. That is how an upgrade works: stop the old server,
and the next command starts a new one that picks the sessions back up.

Use 'cm kill' to end sessions themselves.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Deliberately not ensureServer: starting a server in order to stop it would be absurd,
			// and "no server is running" is the state the caller asked for rather than an error.
			conn, cl, err := connectServer(cmd.Context(), dirs)
			if err != nil {
				fmt.Fprintln(os.Stderr, "no server is running")
				return nil
			}
			defer conn.Close()

			if _, err := cl.Shutdown(cmd.Context(), &serverv1.ShutdownRequest{}); err != nil {
				return err
			}
			return nil
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
	l, socketInode, err := server.Listen(dirs.ServerSocket())
	if err != nil {
		return err
	}
	// No explicit os.Remove of the socket path here.
	//
	// Go's *net.UnixListener already unlinks the path it bound when it is closed, so removing it by
	// name is redundant, and worse than redundant: it deletes whatever is at that path *now*, which
	// after a restart is the next server's socket. A client would then start a server, see it log
	// "server starting" and adopt sessions, and still fail with "server did not become ready",
	// because the socket it was waiting for had been unlinked by the server that just exited.
	//
	// Reproduced by stopping and starting a server repeatedly: it failed within a few iterations.

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
	// Recorded so the server can notice that its own socket path stops referring to it, which is what
	// happens when the runtime directory is deleted underneath it: it keeps listening on an inode nothing can
	// name while every later command starts a second server.
	mgr.SetServerSocketInode(socketInode)
	resizePolicy, err := cfg.Resize()
	if err != nil {
		l.Close()
		return err
	}
	mgr.SetResizePolicy(server.ResizePolicy(resizePolicy))

	// Always installed, even when persistence is disabled. The policy carries two separable things:
	// which sessions save output, and how long finished records are kept. Only the first is what
	// persist.enabled means, and gating the whole policy on it left the manager with no expiry at all,
	// so a default install kept every finished session forever.
	//
	// Matches already returns false for every name when persistence is off, so nothing is persisted
	// either way.
	policy, err := persistPolicy(cfg)
	if err != nil {
		l.Close()
		return err
	}
	mgr.SetPersistPolicy(policy)

	// Adopt sessions whose shims survived a previous server before accepting clients, so a
	// client that reconnects immediately finds its session already present.
	if err := mgr.Reconcile(ctx); err != nil {
		l.Close()
		return err
	}

	// Expire on startup and then periodically. Startup is when a reboot's accumulation is visible,
	// and the ticker covers a server that stays up for weeks.
	//
	// Not gated on persistence being enabled. That setting decides whether a session *saves output*,
	// which is a different question from whether finished session records are cleaned up: every
	// session that ends leaves a row regardless, so gating this meant a default install accumulated
	// them forever and `cm list` filled with every command ever run.
	if _, err := mgr.ExpireDeadSessions(ctx, time.Now()); err != nil {
		// Not fatal: failing to clean up is worth reporting but not worth refusing to serve.
		logger.Warn("expiring sessions failed", "error", err)
	}
	go expirePeriodically(ctx, mgr, logger)
	// Notice if this server's own socket path stops referring to it, and say so in the log.
	//
	// Needed because such a server cannot be asked: its socket is unlinked, so no client can name it, and
	// `cm doctor` reaches the replacement server instead and reports nothing wrong. The log is shared through
	// the state directory, which survives the deletion, so this is the one channel that still works.
	go watchOwnSocket(ctx, mgr)

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
func dialServer(dirs paths.Dirs) (transport.Conn, serverv1.ServerClient, error) {
	return transport.DialServer(dirs.ServerSocket())
}

func connectServer(ctx context.Context, dirs paths.Dirs) (transport.Conn, serverv1.ServerClient, error) {
	if conn, cl, err := dialServer(dirs); err == nil {
		return conn, cl, nil
	}

	if err := ensureServer(ctx, dirs); err != nil {
		return nil, nil, err
	}

	conn, cl, err := dialServer(dirs)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to server: %w", err)
	}
	return conn, cl, nil
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
// A minute rather than an hour. Persisted-session expiry is measured in days and would be happy with
// any interval, but sessions that saved no output are forgotten within minutes, and an hourly sweep
// would leave every short command sitting in `cm list` until the next tick. The sweep is a query
// over a small table, so this still keeps an idle server idle.
const expireInterval = time.Minute

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

// socketWatchInterval is how often a server checks that its own socket path still names it.
//
// The same order as the expiry sweep, and for the same reason: the condition can arise at any time, and the
// check is a single stat, so polling it costs an idle server nothing.
const socketWatchInterval = time.Minute

// watchOwnSocket logs when this server becomes unreachable, until the context is cancelled.
func watchOwnSocket(ctx context.Context, mgr *server.Manager) {
	interval := socketWatchInterval
	// Shortened only in a build with the test hooks compiled in. A test creates the condition deliberately and
	// then waits for it to be noticed, which a minute's interval makes impractical.
	if d, ok := paths.SocketWatchIntervalOverride(); ok {
		interval = d
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			mgr.LogIfUnreachable()
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

	forget, err := cfg.ForgetUnpersistedAfter()
	if err != nil {
		return nil, err
	}

	return &server.PersistPolicy{
		Matches:                cfg.PersistsSession,
		Limits:                 cfg.PersistLimits(),
		OnRestore:              server.RestoreAction(mode),
		CommandIsSafeToRerun:   cfg.CommandIsSafeToRerun,
		ExpireAfter:            expire,
		ForgetUnpersistedAfter: forget,
	}, nil
}

// terminalFactory builds the function the manager uses to create terminal models.
//
// This is where cgo enters the server. Passing it in means the manager, and its tests, do not
// depend on the emulator.
//
// Always returns a factory: cgo is required, so the emulator is always present. The manager still accepts a
// nil one, which is what its own tests use.
func terminalFactory(cfg *config.Config) server.NewTerminalFunc {
	scrollback := cfg.Scrollback()
	return func(rows, cols uint16) (server.Terminal, error) {
		return vt.NewSessionTerminal(rows, cols, scrollback)
	}
}
