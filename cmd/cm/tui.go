package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/tui"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newTUICommand(g *globals) *cobra.Command {
	var (
		readOnly bool
		tagArgs  []string
	)
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Pick a session to attach to, and come back here on detach",
		Long: `Show the sessions and attach to one, returning to the list when it detaches.

For a window that has no session yet. 'cm ls' says what exists and 'cm attach'
goes somewhere; this is the part in between, which is looking at what is running
and deciding.

Detaching returns to the list rather than to the shell, so a window can spend its
life here: pick a session, work, detach, pick another. Quitting with q leaves the
list and the shell that ran it.

The list refreshes about once a second, "/" filters it by name, directory, running
command, or tag, and "?" lists every key.

Running this inside a session nests: the detach key belongs to the outermost
client, so detaching from a session picked here detaches the window instead.
'cm switch' is the command for moving a window that is already on a session.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}
			// Checked before a server is started, since a picker with nothing to draw on has nothing to
			// do, and bubbletea's own failure here names an ioctl rather than the mistake.
			if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
				return fmt.Errorf("%s tui needs a terminal; use %s ls to list sessions", paths.Name, paths.Name)
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}

			// One connection for the whole life of the picker, unlike every other command's withServer. A
			// refresh a second through a fresh connection would pay for the dial each time, and this is the
			// one caller that outlives its first request.
			conn, cl, err := connectServer(cmd.Context(), dirs)
			if err != nil {
				return err
			}
			defer conn.Close()

			// Stated once at startup rather than refused, because a nested attach works and is
			// occasionally what someone wants: it is the detach key that goes to the wrong client, and
			// saying so is more use than declining.
			var notice string
			if inside := insideCmSession(); inside != "" {
				notice = fmt.Sprintf(
					"running inside %s: attaching from here nests, and the detach key will detach this window", inside)
			}

			return tui.Run(cmd.Context(), tui.Options{
				Sessions: cl,
				Tags:     tagArgs,
				Notice:   notice,
				Attach:   attachFromPicker(attachArgv(g, readOnly), runAttachChild),
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&readOnly, "read-only", false,
		"follow sessions without sending input")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"only list sessions with this tag, as key or key=value (repeatable, all must match)")
	return cmd
}

// attachArgv builds the argv for attaching to ref, for the picker to run as a child.
//
// The directories and the config file are passed on rather than left to the child's own defaults, so it
// talks to the same server this picker is listing. A sandboxed picker whose child read the real
// configuration would attach to the developer's own sessions, which is the failure AGENTS.md's
// isolation rule exists to prevent, and it would look like the sandbox working.
//
// Only the flags this process was actually given are forwarded. Passing the resolved values instead
// would write a default into the child's argv as though it had been asked for, and that argv is what
// `ps` reports for as long as the attachment lasts.
//
// A closure over the flags rather than a slice built once, because ref changes per attachment and
// nothing else does.
func attachArgv(g *globals, readOnly bool) func(ref string) []string {
	return func(ref string) []string {
		var argv []string
		if g.runtimeDir != "" {
			argv = append(argv, "--runtime-dir", g.runtimeDir)
		}
		if g.stateDir != "" {
			argv = append(argv, "--state-dir", g.stateDir)
		}
		if g.configPath != "" {
			argv = append(argv, "--config", g.configPath)
		}
		argv = append(argv, "attach")
		if readOnly {
			argv = append(argv, "--read-only")
		}
		// Last, and only when there is one: `cm attach` with no name asks the server to allocate one,
		// which is what the picker's "new session" means.
		if ref != "" {
			argv = append(argv, ref)
		}
		return argv
	}
}

// attachFromPicker builds the picker's way of attaching.
//
// Deliberately a child process rather than a call into internal/client, which is what this did first.
// That package's input reader is left blocked in the kernel on purpose, because a blocked read cannot
// be cancelled, and `cm attach` gets away with it by exiting immediately afterwards. A picker does not
// exit, so the leftover reader sat on the terminal and stole exactly one keystroke per attachment:
// attach, detach, then a single "/" did nothing while every key after it worked. Closing the descriptor
// did not help, because Go defers the real close until the outstanding read finishes. A child takes its
// leftovers with it. See docs/tui.md.
//
// It also gets the upgrade path for nothing, which the in-process version could not have had: an
// upgrade re-execs the client, and a client that is its own process can be replaced without touching
// the picker.
//
// Both halves are parameters so a test can check what argv was built and what the picker does with what
// the child printed, without a terminal or a server.
func attachFromPicker(
	argv func(ref string) []string,
	run func(ctx context.Context, argv []string) (string, error),
) tui.AttachFunc {
	return func(ctx context.Context, ref string) (tui.Attachment, error) {
		note, err := run(ctx, argv(ref))
		if err != nil {
			return tui.Attachment{}, err
		}
		return tui.Attachment{Note: note}, nil
	}
}

// runAttachChild runs one attachment as a child process and returns what it printed.
//
// stdin and stdout are this process's own, which is what makes the child a real client: bubbletea has
// released the terminal by now, so the child finds it exactly as a shell would hand it over, and takes
// it over completely.
//
// stderr is captured instead. It is where `cm attach` reports how the attachment ended, and those bytes
// would otherwise land on a screen bubbletea is about to repaint from its own model, which either
// erases the message or splices it into the frame. Captured, the same text becomes the status line.
func runAttachChild(ctx context.Context, argv []string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolving own path to attach: %w", err)
	}

	// Deliberately not exec.CommandContext, whose cancellation kills the child. A client killed
	// mid-attachment leaves the terminal in raw mode with a session's screen on it, and the context is
	// cancelled exactly when the picker is being asked to stop, which is when a clean detach matters
	// most. A signal reaches the child through the process group anyway, and it detaches on that.
	_ = ctx
	child := exec.Command(exe, argv...)
	child.Stdin, child.Stdout = os.Stdin, os.Stdout
	var said bytes.Buffer
	child.Stderr = &said

	// A failure is reported as the text rather than the status, since `cm attach` says why it could not
	// attach on stderr while the exit status says only that it did not.
	if err := child.Run(); err != nil {
		if note := strings.TrimSpace(said.String()); note != "" {
			return "", errors.New(note)
		}
		return "", err
	}
	return strings.TrimSpace(said.String()), nil
}

// The generated client is what the picker talks to, and tui.Sessions names the four RPCs it uses. This
// is where a change to one of their signatures fails, with the interface named, rather than inside a
// call in another package.
var _ tui.Sessions = serverv1.ServerClient(nil)
