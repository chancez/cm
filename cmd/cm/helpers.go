package main

// Helpers shared by more than one subcommand, and small enough that giving each its own file would
// scatter them. Anything used by a single command belongs in that command's file instead, so this
// stays a place for shared code rather than a drawer for leftovers.

import (
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
)

// argsAfterDash returns the command to run in a new session, which is everything after a
// literal "--". Keeping it separate from the session name means a command can contain
// anything without being mistaken for a flag.
func argsAfterDash(cmd *cobra.Command, args []string) []string {
	if n := cmd.ArgsLenAtDash(); n >= 0 && n <= len(args) {
		return args[n:]
	}
	return nil
}

// newClientLogger opens the shared client log, tagged with which client is writing.
//
// One file for every client, with pid and boot as fields rather than in the filename. A client is short-lived
// and there can be one per attached window, so a file each would accumulate for diagnostics that are only read
// when something is wrong. The fields keep it filterable: pid names the process, and boot distinguishes a reused
// pid from the same pid in this boot, which matters because the log outlives a reboot.
//
// Failing to open the log is not fatal. Diagnostics are advisory, and refusing to attach because a log file
// could not be written would turn a nicety into an outage.
func newClientLogger(dirs paths.Dirs, cfg *config.Config) (*slog.Logger, io.Closer) {
	level, enabled, err := cfg.Logging()
	if err != nil || !enabled {
		return nil, nil
	}
	if err := dirs.Ensure(); err != nil {
		return nil, nil
	}

	logger, closer, err := cmlog.New(cmlog.Options{
		Enabled: true,
		Level:   level,
		Path:    dirs.ClientLog(),
	})
	if err != nil {
		return nil, nil
	}
	return logger.With("pid", os.Getpid(), "boot", paths.BootID()), closer
}
