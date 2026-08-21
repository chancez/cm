package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/chancez/cm/internal/client"
)

// resumeFromFlag is the hidden flag one client uses to hand its position to its replacement.
//
// Named here rather than repeated as a literal in the flag definition and in the argv below, which is
// the sort of duplication that turns "the upgrade repainted the screen" into a bug with no obvious
// cause: the flag would simply not be recognised and the replacement would do a fresh attach.
const resumeFromFlag = "resume-from-seq"

// reexecForUpgrade replaces this process with the binary currently on disk, resuming where it left off.
//
// syscall.Exec rather than spawning a child and exiting, and that is the point of the whole approach.
// exec keeps the process id, the open descriptors, and the terminal exactly as they are: the terminal
// stays in raw mode, the screen keeps showing the session, and the shell that started this client keeps
// waiting on the same pid. A child would mean this process exits, its shell prints a prompt over the
// session, and the terminal is restored and re-rawed, all of which is visible.
//
// The terminal is deliberately *not* restored before this. See runAttach: restoring writes a reset that
// clears the session off the screen, which is exactly the seam upgrading in place exists to avoid.
//
// Returns only on failure. A successful exec never comes back, so a caller reaching the next line knows
// it is still holding a raw terminal and has to put it back.
func reexecForUpgrade(argv []string) error {
	// Resolved through the kernel rather than from argv[0], so this works when cm was invoked by a
	// relative path or through a shell function, and picks up a binary replaced at the same path since
	// this process started. That replacement is the ordinary case here: `mise run install` renames a new
	// binary over the old one, which is the thing being upgraded to.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving the cm binary: %w", err)
	}

	// Nothing is buffered on this path, which exec would otherwise discard: the client writes to the
	// terminal unbuffered, and the log is closed by the caller's defer.
	if err := syscall.Exec(exe, argv, os.Environ()); err != nil {
		return fmt.Errorf("re-executing %s: %w", exe, err)
	}
	// Unreachable: a successful Exec does not return.
	return nil
}

// upgradeArgv builds the argv for the process replacing this one.
//
// Reconstructed from the flags cobra parsed rather than by editing the original argv, which was the
// first approach and was wrong. Editing means guessing which bare word is the session name, and
// `--dir /tmp` puts a bare word right after a flag: the guess took /tmp for the session name. Cobra
// already knows which strings were flags and which were values, so asking it removes the guess.
//
// Only flags the user actually set are emitted, which keeps the argv close to what was typed and means
// a default that changes in the new build takes effect rather than being pinned to the old one's value.
//
// The session name is forced to the resolved one rather than whatever was typed. A client started with
// no name had one allocated by the server, and re-execing with no name would allocate a *second*
// session and orphan the first with the user's shell still in it, which is the worst thing this feature
// could do.
func upgradeArgv(cmd *cobra.Command, args []string, res client.Result) []string {
	argv0 := "cm"
	if len(os.Args) > 0 {
		// Kept as invoked so `ps` shows what the user launched. The kernel takes the path to execute from
		// Exec's first argument, not from argv[0].
		argv0 = os.Args[0]
	}
	argv := []string{argv0, cmd.Name()}

	// The resolved name, always, and before any "--" so it is not read as part of the command.
	if res.Session != "" {
		argv = append(argv, res.Session)
	}

	cmd.Flags().Visit(func(f *pflag.Flag) {
		// The old position is dropped rather than carried: this process's own is appended below, and two
		// copies would leave the wrong one in play depending on parse order.
		if f.Name == resumeFromFlag {
			return
		}
		// Repeatable flags carry several values, each of which has to be emitted separately. A
		// StringArray renders as "[a,b]" through Value.String(), which would be passed through as one
		// literal value containing brackets.
		if sv, ok := f.Value.(interface{ GetSlice() []string }); ok {
			for _, v := range sv.GetSlice() {
				argv = append(argv, "--"+f.Name+"="+v)
			}
			return
		}
		argv = append(argv, "--"+f.Name+"="+f.Value.String())
	})

	// Appended after the flags and only for a real position. Zero would ask for the whole retained log
	// rather than resuming from here.
	if res.ResumeFrom != nil && *res.ResumeFrom > 0 {
		argv = append(argv, "--"+resumeFromFlag+"="+strconv.FormatUint(*res.ResumeFrom, 10))
	}

	// The command to run, which only matters when this attach creates the session. Preserved because a
	// replacement that reattaches never uses it, and one that finds the session gone recreates it the
	// same way the original was asked to.
	if n := cmd.ArgsLenAtDash(); n >= 0 && n <= len(args) {
		argv = append(argv, "--")
		argv = append(argv, args[n:]...)
	}
	return argv
}
