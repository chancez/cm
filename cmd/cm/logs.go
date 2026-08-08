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

	"github.com/chancez/cm/internal/paths"
)

func newLogsCommand(g *globals) *cobra.Command {
	var (
		follow bool
		lines  int
		all    bool
		clear  bool
	)
	cmd := &cobra.Command{
		Use:   "logs [session]",
		Short: "Print cm's diagnostic log",
		Long: `Print cm's diagnostic log.

With no argument, prints the server's log. With a session name, prints that
session's shim log instead.

The server and shim run detached with their stdio discarded, so this is the only
record of what they did. Several errors are deliberately swallowed to keep a
session alive when something advisory fails, and those are logged here rather
than shown, so a session that quietly stopped persisting or lost its title says
so in the log.

This is the diagnostic log, not the session's output. Use 'cm history' for what
the shell printed.

--clear empties the log instead of printing it, which is worth having while
debugging: 'cm doctor' reports errors from the last 24 hours, so yesterday's
entries obscure whether a change helped. It truncates rather than deletes, since
the server and any shim hold the file open and writing to a deleted file would
lose their output silently.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}

			path := dirs.ServerLog()
			if len(args) == 1 {
				if err := paths.ValidateSessionName(args[0]); err != nil {
					return err
				}
				path = dirs.ShimLog(args[0])
			}

			if clear {
				return clearLogs(path, all)
			}

			if all {
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
			if follow {
				return followLog(cmd.Context(), path)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", false, "keep reading as the log grows")
	f.IntVarP(&lines, "lines", "n", 200,
		"print only the last N lines (0 for all)")
	f.BoolVar(&all, "all", false,
		"include the rotated previous log")
	f.BoolVar(&clear, "clear", false,
		"empty the log instead of printing it")
	return cmd
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

// followLog keeps printing a log as it grows.
//
// Polls rather than watching the filesystem: this is a diagnostic aid used interactively, so a
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
