package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/profiling"
)

// serveDiagnostics starts serving this process's profiles and flight recorder, whichever `[debug]` turns
// on, and returns what stops it. A no-op when neither is on, which is every process on an install that
// has not asked.
//
// A failure is logged rather than returned: diagnostics are for diagnosing a session, and a session that
// refused to start because its profiler could not would be the worse outcome.
func serveDiagnostics(ctx context.Context, dirs paths.Dirs, cfg *config.Config, role string, log *slog.Logger) func() {
	if cfg == nil {
		return func() {}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	window, err := cfg.FlightWindow()
	if err != nil {
		log.Warn("not keeping a flight recorder", "error", err)
		window = 0
	}
	opts := profiling.Options{Profiles: cfg.Debug.Pprof, FlightWindow: window}
	if opts == (profiling.Options{}) {
		return func() {}
	}
	socket := dirs.DiagnosticsSocket(role)
	if err := paths.CheckSocketPath(socket); err != nil {
		log.Warn("not serving diagnostics", "error", err)
		return func() {}
	}
	stop, err := profiling.Serve(ctx, socket, opts, log)
	if err != nil {
		log.Warn("not serving diagnostics", "error", err)
		return func() {}
	}
	return stop
}

func newDebugCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Diagnose cm itself",
	}
	cmd.AddCommand(newDebugPprofCommand(g), newDebugFlightCommand(g))
	return cmd
}

// debugTarget is the process a debug command reads: a session's shim, the server, or a client.
type debugTarget struct {
	server bool
	client int
}

func (t *debugTarget) addFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&t.server, "server", false, "read the server")
	cmd.Flags().IntVar(&t.client, "client", 0, "read the attached client with this pid")
}

func (t *debugTarget) args(_ *cobra.Command, args []string) error {
	targets := len(args)
	if t.server {
		targets++
	}
	if t.client != 0 {
		targets++
	}
	if targets != 1 {
		return errors.New("name exactly one of a session, --server, or --client PID")
	}
	return nil
}

// role names the target's socket: "server", "client-<pid>", or the session's ID.
func (t *debugTarget) role(ctx context.Context, dirs paths.Dirs, args []string) (string, error) {
	switch {
	case t.client != 0:
		return "client-" + strconv.Itoa(t.client), nil
	case len(args) == 1:
		if err := paths.ValidateSessionRef(args[0]); err != nil {
			return "", err
		}
		return resolveSessionID(ctx, dirs, args[0])
	}
	return "server", nil
}

// fetchToFile writes what the target serves under name to output, and removes the file if that fails: a
// file holding a partial or empty profile would be read later as a real one.
func fetchToFile(ctx context.Context, socket, name string, seconds int, output string) error {
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	fetchErr := profiling.Fetch(ctx, socket, name, seconds, f)
	closeErr := f.Close()
	if fetchErr != nil {
		_ = os.Remove(output)
		return fetchErr
	}
	if closeErr != nil {
		return closeErr
	}
	tool := "pprof"
	if name == "trace" || name == profiling.Flight {
		tool = "trace"
	}
	fmt.Fprintf(os.Stdout, "%s\n", output)
	fmt.Fprintf(os.Stderr, "read it with: go tool %s %s\n", tool, output)
	return nil
}

func defaultOutput(role, name string) string {
	ext := ".pprof"
	if name == "trace" || name == profiling.Flight {
		ext = ".trace"
	}
	return fmt.Sprintf("cm-%s-%s-%s%s", role, name, time.Now().Format("20060102-150405"), ext)
}

func newDebugPprofCommand(g *globals) *cobra.Command {
	var (
		target  debugTarget
		profile string
		seconds int
		output  string
		flight  bool
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

--flight also writes the process's flight recorder once the profile is done, so
the trace covers the same stretch of time, up to flight_recorder_window. It goes
beside --output as <name>-flight.trace. That needs flight_recorder on as well;
see 'cm debug flight'.

Local only: a --remote server's processes are not reachable from here.`,
		Args:              target.args,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			role, err := target.role(cmd.Context(), dirs, args)
			if err != nil {
				return err
			}
			wait := 0
			if slices.Contains(profiling.Sampled, profile) {
				wait = seconds
			}
			flightOutput := defaultOutput(role, profiling.Flight)
			if output == "" {
				output = defaultOutput(role, profile)
			} else {
				// Beside the profile, so a caller who chose where files go gets both there.
				flightOutput = strings.TrimSuffix(output, filepath.Ext(output)) + "-flight.trace"
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(wait)*time.Second+30*time.Second)
			defer cancel()
			if wait > 0 {
				fmt.Fprintf(os.Stderr, "recording %s for %ds\n", profile, wait)
			}
			socket := dirs.DiagnosticsSocket(role)
			if err := fetchToFile(ctx, socket, profile, wait, output); err != nil {
				return err
			}
			if !flight {
				return nil
			}
			// After the profile rather than before, since the recorder holds what came before it is asked.
			return fetchToFile(ctx, socket, profiling.Flight, 0, flightOutput)
		},
	}
	target.addFlags(cmd)
	f := cmd.Flags()
	f.StringVar(&profile, "profile", "cpu", "cpu, trace, heap, goroutine, allocs, block, mutex, or threadcreate")
	f.IntVar(&seconds, "seconds", 10, "how long to record cpu, trace, block and mutex")
	f.StringVarP(&output, "output", "o", "", "file to write (default cm-<process>-<profile>-<time>.pprof)")
	f.BoolVar(&flight, "flight", false, "also write the flight recorder once the profile is done")
	return cmd
}

func newDebugFlightCommand(g *globals) *cobra.Command {
	var (
		target debugTarget
		output string
	)
	cmd := &cobra.Command{
		Use:   "flight [session]",
		Short: "Write what the server, a session's shim, or a client just did, as a trace",
		Long: `Write the last few seconds of one cm process's execution trace to a file.

For a stall that has already happened: run this right after noticing it. Each
process keeps the trace in memory, so nothing has to be started beforehand
except the setting, read when each process starts:

  [debug]
  flight_recorder = true
  flight_recorder_window = "10s"   # the default

  cm debug flight work                     # the shim behind session "work"
  cm debug flight --server
  cm debug flight --client 4242            # an attached client, by pid

Read the file with 'go tool trace <file>'. Independent of [debug] pprof: either
works without the other.

Costs about 1.5ms of CPU per second in each idle process and nothing
measurable under load; see docs/config.md.

Local only: a --remote server's processes are not reachable from here.`,
		Args:              target.args,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			role, err := target.role(cmd.Context(), dirs, args)
			if err != nil {
				return err
			}
			if output == "" {
				output = defaultOutput(role, profiling.Flight)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			return fetchToFile(ctx, dirs.DiagnosticsSocket(role), profiling.Flight, 0, output)
		},
	}
	target.addFlags(cmd)
	cmd.Flags().StringVarP(&output, "output", "o", "", "file to write (default cm-<process>-flight-<time>.trace)")
	return cmd
}
