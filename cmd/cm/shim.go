package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
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
		session    string
		dir        string
		rows, cols uint16
		logBytes   int
	)
	cmd := &cobra.Command{
		Use:                "shim",
		Short:              "Internal: hold a session's pty",
		Hidden:             true,
		DisableSuggestions: true,
		// Everything after the flags is the command to run, so args are not restricted.
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := paths.ValidateSessionName(session); err != nil {
				return err
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			if err := dirs.Ensure(); err != nil {
				return err
			}
			return runShim(cmd.Context(), dirs, shim.Config{
				Session:  session,
				Command:  args,
				Dir:      dir,
				Rows:     rows,
				Cols:     cols,
				LogBytes: logBytes,
			})
		},
	}
	cmd.Flags().SetInterspersed(false)
	f := cmd.Flags()
	f.StringVar(&session, "session", "", "session name this shim serves")
	f.StringVar(&dir, "dir", "", "working directory for the shell")
	f.Uint16Var(&rows, "rows", 24, "initial window rows")
	f.Uint16Var(&cols, "cols", 80, "initial window columns")
	f.IntVar(&logBytes, "log-bytes", 0, "bytes of output to retain (0 for the default)")
	return cmd
}

// runShim binds the socket, starts the session, and serves until the session ends.
//
// Binding before starting the shell means a shim that cannot claim its socket, because
// another already holds it, fails without having spawned anything.
func runShim(ctx context.Context, dirs paths.Dirs, cfg shim.Config) error {
	socket := dirs.ShimSocket(cfg.Session)
	l, err := shim.Listen(socket)
	if err != nil {
		return err
	}
	// Serve closes the listener, which unlinks nothing, so remove the socket here.
	// Leaving it behind would make the session look alive to anyone scanning the
	// runtime directory.
	defer removeSocket(socket)

	session, err := shim.Start(cfg)
	if err != nil {
		l.Close()
		return fmt.Errorf("starting session %s: %w", cfg.Session, err)
	}

	return shim.Serve(ctx, l, shim.NewService(session))
}

// removeSocket unlinks the shim socket, ignoring the case where it is already gone.
func removeSocket(path string) {
	_ = os.Remove(path)
}
