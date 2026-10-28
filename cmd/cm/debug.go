package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/profiling"
)

// serveProfiles starts serving this process's profiles when `[debug] pprof` is on, and returns what stops
// it. A no-op otherwise, which is every process on an install that has not asked.
//
// A failure is logged rather than returned: profiling is for diagnosing a session, and a session that
// refused to start because its profiler could not would be the worse outcome.
func serveProfiles(ctx context.Context, dirs paths.Dirs, cfg *config.Config, role string, log *slog.Logger) func() {
	if cfg == nil || !cfg.Debug.Pprof {
		return func() {}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	socket := dirs.ProfileSocket(role)
	if err := paths.CheckSocketPath(socket); err != nil {
		log.Warn("not serving profiles", "error", err)
		return func() {}
	}
	stop, err := profiling.Serve(ctx, socket, log)
	if err != nil {
		log.Warn("not serving profiles", "error", err)
		return func() {}
	}
	return stop
}

func newDebugCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Diagnose cm itself",
	}
	cmd.AddCommand(newDebugPprofCommand(g))
	return cmd
}

func newDebugPprofCommand(g *globals) *cobra.Command {
	var (
		server  bool
		client  int
		profile string
		seconds int
		output  string
	)
	cmd := &cobra.Command{
		Use:   "pprof [session]",
		Short: "Write a runtime profile of the server, a session's shim, or a client",
		Long: `Write a runtime profile of one cm process to a file.

Needs this in the config file, read when each process starts, so restart the
server and start the session after setting it:

  [debug]
  pprof = true

Which process to profile depends on where the time goes. A session's shim owns
the pty and its persisted log, the server owns the terminal model, and a client
writes to your terminal:

  cm debug pprof work                      # the shim behind session "work"
  cm debug pprof --server
  cm debug pprof --client 4242             # an attached client, by pid

cpu, trace, block and mutex are recorded for --seconds; the rest (heap,
goroutine, allocs, threadcreate) are a snapshot. Read the file with
'go tool pprof <file>', or 'go tool trace <file>' for a trace, which is the one
that shows latency: what a goroutine waited on, and for how long.

Local only: a --remote server's processes are not reachable from here.`,
		Args: func(cmd *cobra.Command, args []string) error {
			targets := len(args)
			if server {
				targets++
			}
			if client != 0 {
				targets++
			}
			if targets != 1 {
				return errors.New("name exactly one of a session, --server, or --client PID")
			}
			return nil
		},
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			role := "server"
			switch {
			case client != 0:
				role = "client-" + strconv.Itoa(client)
			case len(args) == 1:
				if err := paths.ValidateSessionRef(args[0]); err != nil {
					return err
				}
				if role, err = resolveSessionID(cmd.Context(), dirs, args[0]); err != nil {
					return err
				}
			}

			wait := 0
			if slices.Contains(profiling.Sampled, profile) {
				wait = seconds
			}
			if output == "" {
				ext := ".pprof"
				if profile == "trace" {
					ext = ".trace"
				}
				output = fmt.Sprintf("cm-%s-%s-%s%s", role, profile, time.Now().Format("20060102-150405"), ext)
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(wait)*time.Second+30*time.Second)
			defer cancel()
			if wait > 0 {
				fmt.Fprintf(os.Stderr, "recording %s for %ds\n", profile, wait)
			}
			f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			fetchErr := profiling.Fetch(ctx, dirs.ProfileSocket(role), profile, wait, f)
			closeErr := f.Close()
			if fetchErr != nil {
				// A file holding a partial or empty profile would be read later as a real one.
				_ = os.Remove(output)
				return fetchErr
			}
			if closeErr != nil {
				return closeErr
			}
			tool := "pprof"
			if profile == "trace" {
				tool = "trace"
			}
			fmt.Fprintf(os.Stdout, "%s\n", output)
			fmt.Fprintf(os.Stderr, "read it with: go tool %s %s\n", tool, output)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&server, "server", false, "profile the server")
	f.IntVar(&client, "client", 0, "profile the attached client with this pid")
	f.StringVar(&profile, "profile", "cpu", "cpu, trace, heap, goroutine, allocs, block, mutex, or threadcreate")
	f.IntVar(&seconds, "seconds", 10, "how long to record cpu, trace, block and mutex")
	f.StringVarP(&output, "output", "o", "", "file to write (default cm-<process>-<profile>-<time>.pprof)")
	return cmd
}
