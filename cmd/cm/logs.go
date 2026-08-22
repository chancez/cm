package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// logsFlags are the options every logs subcommand shares.
//
// One struct because the three subcommands differ only in which file they name: sharing the flags means
// `logs server -f` and `logs shim NAME -f` cannot drift, and adding an option touches one place.
type logsFlags struct {
	follow bool
	lines  int
	all    bool
	clear  bool
}

func (f *logsFlags) bind(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.BoolVarP(&f.follow, "follow", "f", false, "keep reading as the log grows")
	fl.IntVarP(&f.lines, "lines", "n", 200, "print only the last N lines (0 for all)")
	fl.BoolVar(&f.all, "all", false, "include the rotated previous log")
	fl.BoolVar(&f.clear, "clear", false, "empty the log instead of printing it")
}

func newLogsCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Print cm's diagnostic logs",
		Long: `Print cm's diagnostic logs.

Three kinds of process keep one, and each has a subcommand:

  logs server         the server's, of which there is one
  logs client         every client's, shared, with pid and boot as fields
  logs shim <name>    one session's shim

The server and shims run detached with their stdio discarded, so these are the
only record of what they did. Several errors are deliberately swallowed to keep a
session alive when something advisory fails, and those are logged rather than
shown: a session that quietly stopped persisting or lost its title says so here.

Clients share one file rather than having one each. A client is short-lived and
there can be one per attached window, so a file each would accumulate for
diagnostics that are read only when something is wrong. Which client wrote a line
is in its pid and boot fields instead.

These are diagnostic logs, not session output. Use 'cm history' or 'cm read' for
what the shell printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Bare `cm logs` prints help rather than guessing which log was meant. It used to print the
			// server's, and keeping that would make the subcommands optional in a way that reads as
			// inconsistent once there are three.
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newLogsServerCommand(g),
		newLogsClientCommand(g),
		newLogsShimCommand(g),
	)
	return cmd
}

func newLogsServerCommand(g *globals) *cobra.Command {
	var f logsFlags
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Print the server's diagnostic log",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return runLogs(cmd.Context(), dirs.ServerLog(), f)
		},
	}
	f.bind(cmd)
	return cmd
}

func newLogsClientCommand(g *globals) *cobra.Command {
	var f logsFlags
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Print the shared client diagnostic log",
		Long: `Print the diagnostic log every client writes to.

One file for all of them, with pid and boot recorded as fields on each line, so
filtering to one client is a grep rather than finding the right file. boot
distinguishes a reused pid from the same pid in this boot, which matters because
the log outlives a reboot.

Clients record what nothing else can see: how often they reconnected, where they
resumed from, and input held across an outage.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return runLogs(cmd.Context(), dirs.ClientLog(), f)
		},
	}
	f.bind(cmd)
	return cmd
}

func newLogsShimCommand(g *globals) *cobra.Command {
	var f logsFlags
	cmd := &cobra.Command{
		Use:   "shim <session>",
		Short: "Print one session's shim diagnostic log",
		Long: `Print the diagnostic log for one session's shim.

The shim owns the session's pty and shell, so its log is where a pty that could
not be resized or output that could not be persisted is recorded.

Separate from the session's output: this is what the shim did, 'cm history' is
what the shell printed.`,
		Args:              sessionNameArg,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// The log is named after the session's ID, so a name has to be resolved first. Read from
			// the database rather than from the server, since the reason to read a shim log is usually
			// that something is wrong.
			id, err := sessionLogStem(cmd.Context(), dirs, args[0])
			if err != nil {
				return err
			}
			return runLogs(cmd.Context(), dirs.ShimLog(id), f)
		},
	}
	f.bind(cmd)
	return cmd
}

// runLogs prints, follows, or clears one log file.
//
// Shared by every subcommand, so the behavior of --follow, --lines, --all, and --clear cannot differ between
// them: only the path differs.
func runLogs(ctx context.Context, path string, f logsFlags) error {
	if f.clear {
		return clearLogs(path, f.all)
	}

	lines := f.lines
	if f.all {
		// The rotated generation first, so the output reads oldest to newest.
		if err := printLog(path+".1", 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		lines = 0
	}

	if err := printLog(path, lines); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no log at %s; logging may be disabled", path)
		}
		return err
	}
	if f.follow {
		return followLog(ctx, path)
	}
	return nil
}

// printLog writes a log file, optionally only its last n lines.
func printLog(path string, n int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if n <= 0 {
		_, err := io.Copy(os.Stdout, f)
		return err
	}

	// A ring of the last n lines. Reading the whole file rather than seeking from the end because a
	// rotated log is bounded to a few megabytes, and correctness with partial final lines is worth
	// more here than avoiding one pass.
	//
	// Indexed rather than resliced. The obvious `ring = ring[1:]` before each append is correct but
	// reallocates: the slice window walks forward until it reaches the end of the backing array, then append
	// grows a new one and copies. Measured on this function over a 200k-line log with n=10, that costs 5.44ms
	// and 7.79 MB against 3.51ms and 1.39 MB here. See BenchmarkPrintLogTail.
	ring := make([]string, n)
	count := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ring[count%n] = sc.Text()
		count++
	}
	if err := sc.Err(); err != nil {
		return err
	}

	// The oldest line still held. Fewer lines than asked for means starting at zero rather than wrapping.
	start := count - n
	if start < 0 {
		start = 0
	}
	for i := start; i < count; i++ {
		if _, err := fmt.Println(ring[i%n]); err != nil {
			return err
		}
	}
	return nil
}

// fraction of a second of latency does not matter and a poll needs no platform-specific watcher.
func followLog(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}

	const interval = 200 * time.Millisecond
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := os.Stdout.Write(buf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}

		// Rotation replaces the file, so a reader holding the old one would go silent. Detect it by
		// comparing the open file's size against the path's, and reopen when the path is shorter.
		if rotated, rerr := looksRotated(f, path); rerr == nil && rotated {
			f.Close()
			nf, oerr := os.Open(path)
			if oerr != nil {
				return oerr
			}
			f = nf
		}
	}
}

// clearLogs empties a log, and with all its rotated generation too.
//
// Truncates rather than removes. The server and each shim hold their log open for the life of the process, so
// unlinking it would leave them writing to a deleted inode: their output would go nowhere and nothing would
// report it. Verified rather than assumed -- after an unlink the path is gone while the writer keeps
// succeeding, where after a truncation the file is still there and the next write lands in it.
//
// The rotated generation is removed rather than truncated, since nothing holds it open: it exists only as a
// previous file, and leaving an empty one behind would be noise.
//
// A missing log is not an error. `cm logs --clear` on a fresh installation, or for a session that never
// logged, has nothing to do and asking for that is not a mistake.
func clearLogs(path string, all bool) error {
	if err := truncateIfExists(path); err != nil {
		return err
	}
	if all {
		if err := os.Remove(path + ".1"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing the rotated log: %w", err)
		}
	}
	return nil
}

// truncateIfExists empties a file, treating absence as success.
func truncateIfExists(path string) error {
	if err := os.Truncate(path, 0); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("clearing %s: %w", path, err)
	}
	return nil
}

// looksRotated reports whether the path now holds a different, shorter file than the open one.
func looksRotated(f *os.File, path string) (bool, error) {
	openInfo, err := f.Stat()
	if err != nil {
		return false, err
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if !os.SameFile(openInfo, pathInfo) {
		return true, nil
	}
	// Same file but shorter than where we are reading, which means it was truncated.
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, err
	}
	return pathInfo.Size() < pos, nil
}
