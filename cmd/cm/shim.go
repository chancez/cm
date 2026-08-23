package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/shim"
)

// newShimCommand builds the hidden shim subcommand.
//
// The server re-execs this; a human never types it. Suggestions are disabled so a
// malformed re-exec fails loudly instead of helpfully running something adjacent, and
// interspersed args are disabled so the argv the server constructs is parsed exactly as
// written.
func newShimCommand(g *globals) *cobra.Command {
	var (
		session         string
		dir             string
		rows, cols      uint16
		xpixel, ypixel  uint16
		logBytes        int
		persistPath     string
		persistMaxLines int
		persistMaxBytes int64
	)
	cmd := &cobra.Command{
		Use:                "shim",
		Short:              "Internal: hold a session's pty",
		Hidden:             true,
		DisableSuggestions: true,
		// Everything after the flags is the command to run, so args are not restricted.
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := paths.ValidateSessionID(session); err != nil {
				return err
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			if err := dirs.Ensure(); err != nil {
				return err
			}
			cfg, err := g.config()
			if err != nil {
				return err
			}
			return runShim(cmd.Context(), dirs, cfg, shim.Config{
				Session:     session,
				Command:     args,
				Dir:         dir,
				Rows:        rows,
				Cols:        cols,
				XPixel:      xpixel,
				YPixel:      ypixel,
				LogBytes:    logBytes,
				PersistPath: persistPath,
				PersistLimits: seqlog.FileLimits{
					MaxLines: persistMaxLines,
					MaxBytes: persistMaxBytes,
				},
			})
		},
	}
	cmd.Flags().SetInterspersed(false)
	f := cmd.Flags()
	f.StringVar(&session, "session", "", "session name this shim serves")
	f.StringVar(&dir, "dir", "", "working directory for the shell")
	f.Uint16Var(&rows, "rows", 24, "initial window rows")
	f.Uint16Var(&cols, "cols", 80, "initial window columns")
	// Zero rather than a guessed default: pixels depend on the font the client renders with, so there is
	// no sensible fallback, and inventing one would tell a program the window is a size it is not.
	f.Uint16Var(&xpixel, "xpixel", 0, "initial window width in pixels (0 if unknown)")
	f.Uint16Var(&ypixel, "ypixel", 0, "initial window height in pixels (0 if unknown)")
	f.IntVar(&logBytes, "log-bytes", 0, "bytes of output to retain (0 for the default)")
	f.StringVar(&persistPath, "persist-path", "",
		"file to mirror output to, so the session survives this process")
	f.IntVar(&persistMaxLines, "persist-max-lines", 0,
		"lines to retain in the persisted log (0 for the default)")
	f.Int64Var(&persistMaxBytes, "persist-max-bytes", 0,
		"byte ceiling for the persisted log (0 for the default)")
	return cmd
}

// runShim binds the socket, starts the session, and serves until the session ends.
//
// Binding before starting the shell means a shim that cannot claim its socket, because
// another already holds it, fails without having spawned anything.
func runShim(ctx context.Context, dirs paths.Dirs, appCfg *config.Config, cfg shim.Config) error {
	level, enabled, err := appCfg.Logging()
	if err != nil {
		return err
	}
	logger, closeLog, err := cmlog.New(cmlog.Options{
		Path:    dirs.ShimLog(cfg.Session),
		Level:   level,
		Enabled: enabled,
	})
	if err != nil {
		return err
	}
	defer closeLog.Close()

	socket := dirs.ShimSocket(cfg.Session)
	l, err := shim.Listen(socket)
	if err != nil {
		logger.Error("claiming shim socket failed", "socket", socket, "error", err)
		return err
	}
	// Serve closes the listener, which unlinks nothing, so remove the socket here.
	// Leaving it behind would make the session look alive to anyone scanning the
	// runtime directory.
	defer removeSocket(socket)

	session, err := shim.Start(cfg)
	if err != nil {
		l.Close()
		logger.Error("starting session failed", "session", cfg.Session, "error", err)
		return fmt.Errorf("starting session %s: %w", cfg.Session, err)
	}
	session.SetLogger(logger)

	logger.Info("shim started",
		"session", cfg.Session, "pid", os.Getpid(), "shell_pid", session.ShellPID(),
		"persisting", cfg.PersistPath != "")

	serveErr := shim.Serve(ctx, l, shim.NewService(session))

	exited, code := session.Exited()
	logger.Info("shim exiting", "session", cfg.Session, "shell_exited", exited, "exit_code", code)
	return serveErr
}

// removeSocket unlinks the shim socket, ignoring the case where it is already gone.
func removeSocket(path string) {
	_ = os.Remove(path)
}
