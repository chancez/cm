package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"time"

	"github.com/chancez/cm/internal/paths"
)

// overlayCommandTimeout bounds a command run from the overlay.
//
// A bound at all because the overlay is a keyboard, not a terminal: a command that waits for something
// would sit there with no way to interrupt it. Ten seconds is well above every command that has anything
// to print -- `cm doctor` is the slowest at a little over a second -- and well below the point where a
// user concludes the overlay is broken. Commands that wait *by design* are refused outright below rather
// than left to this.
const overlayCommandTimeout = 10 * time.Second

// overlayRunner returns the function the overlay uses to run a cm command.
//
// A child process rather than calling into the command tree in this package. Three reasons, in order of
// how much they matter: this process is holding a terminal in raw mode with its stdout owned by
// internal/client, so a command that printed anything would write into the session's screen; cobra
// commands here are built around os.Stdout and os.Exit; and a child gets the whole CLI, including
// commands added later, for nothing. The cost is one process per command, about 23ms, which is invisible
// against a keystroke.
//
// The same measurement that made `cm tui` run attachments as children rather than calling
// client.Attach. See docs/tui.md.
func overlayRunner(dirs paths.Dirs) func(context.Context, string, []string) (string, error) {
	return func(ctx context.Context, sessionRef string, args []string) (string, error) {
		if err := refuseOverlayCommand(args); err != nil {
			return "", err
		}
		self, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("finding the cm binary: %w", err)
		}

		ctx, cancel := context.WithTimeout(ctx, overlayCommandTimeout)
		defer cancel()

		cmd := exec.CommandContext(ctx, self, args...)
		// The directories this client is using, stated rather than inherited. A client started with
		// --runtime-dir has nothing in its environment saying so, and a child that fell back to the default
		// would talk to a different server: the sandbox in .agents/skills/cm-sandbox is exactly that case,
		// and the failure would be a command that quietly did nothing to the session in front of you.
		cmd.Env = append(os.Environ(),
			"CM_RUNTIME_DIR="+dirs.Runtime,
			"CM_STATE_DIR="+dirs.State,
			// The session the command acts on, which is what makes `bind refactor` mean "name this one".
			// Overridden rather than inherited, because a client attached from inside another session has
			// that outer session's reference in its own environment, and every command would have named the
			// wrong session.
			paths.SessionEnv()+"="+sessionRef,
		)
		// Nothing to read. A command that wants input gets EOF rather than the terminal, which is being
		// used by the session, and stealing a keystroke from it is the bug internal/tui measured.
		cmd.Stdin = nil

		// Both streams together, in the order they were written. cm prints the useful part to stdout and
		// the reason to stderr, and a runner that kept only one of them showed either a bare exit status or
		// a success message for a failure.
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			return string(out), fmt.Errorf("gave up after %s", overlayCommandTimeout)
		}
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				// The output carries the reason, so the status alone is what is added. Reporting the raw
				// ExitError as well would put "exit status 1" on screen next to the message that explains it.
				return string(out), fmt.Errorf("exit %d", exit.ExitCode())
			}
			return string(out), err
		}
		return string(out), nil
	}
}

// overlayRefusals are the commands the overlay will not run, and why.
//
// Two kinds. Those that take over a terminal, which the overlay does not have to give: it is a few rows
// at the bottom of a screen a program is using. And those that wait by design, which would hold the
// overlay open until the timeout with nothing to show.
//
// A list rather than a timeout alone because the message matters. "cm attach cannot run here" tells
// someone what to do instead; a command that appears to hang for ten seconds and then reports giving up
// teaches them the overlay is unreliable.
var overlayRefusals = map[string]string{
	"attach": "attach needs a terminal of its own: use ':switch' to move this window instead",
	"a":      "attach needs a terminal of its own: use ':switch' to move this window instead",
	"tui":    "the picker needs a terminal of its own: detach first",
	"wait":   "wait blocks until something happens, which the overlay cannot show",
	"follow": "follow streams until it is stopped, which the overlay cannot show",
	"shim":   "the shim is started by the server, not by hand",
}

// refuseOverlayCommand reports why a command cannot run from the overlay, or nil.
func refuseOverlayCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("no command given")
	}
	if why, refused := overlayRefusals[args[0]]; refused {
		return errors.New(why)
	}
	// Streaming forms of commands that are otherwise fine. Checked by flag rather than by command,
	// because `cm read` is one of the most useful things to run here and only --follow makes it endless.
	if slices.Contains(args, "--follow") || slices.Contains(args, "-f") {
		return fmt.Errorf("%s --follow streams until it is stopped, which the overlay cannot show", args[0])
	}
	// `cm run` attaches unless it is told not to, and an attachment here would fight the session for the
	// terminal. With -d it is a detached session and perfectly reasonable from an overlay.
	if args[0] == "run" && !slices.Contains(args, "-d") && !slices.Contains(args, "--detach") {
		return errors.New("run attaches unless given -d: add -d to start it in the background")
	}
	// Nothing here rejects a command for being unknown. cm's own error for that is better than a guess,
	// and a list of valid commands maintained here would go stale the first time one is added.
	return nil
}
