package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/chancez/cm/internal/client"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/sessionenv"
	"github.com/chancez/cm/internal/tags"
	"github.com/chancez/cm/internal/tui"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newTUICommand(g *globals) *cobra.Command {
	var (
		readOnly bool
		setTitle bool
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
			if _, err := tags.ParseSelector(tagArgs); err != nil {
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
			cfg, err := g.config()
			if err != nil {
				return err
			}
			detachKey, err := client.ParseDetachKey(cfg.DetachKey)
			if err != nil {
				return err
			}

			// One connection for the whole session of the picker, unlike every other command's
			// withServer. A refresh a second through a fresh connection would pay for the dial each
			// time, and the picker is the one caller that outlives its first request.
			conn, cl, err := connectServer(cmd.Context(), dirs)
			if err != nil {
				return err
			}
			defer conn.Close()

			logger, closeLog := newClientLogger(dirs, cfg)
			if closeLog != nil {
				defer closeLog.Close()
			}

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
				Attach: attachFromPicker(dirs, cfg.EnvMatcher(), attachSettings{
					detachKey: detachKey,
					readOnly:  readOnly,
					setTitle:  setTitle,
					log:       logger,
				}),
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&readOnly, "read-only", false,
		"follow sessions without sending input")
	f.BoolVar(&setTitle, "set-title", true,
		"forward the attached session's title to the terminal")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"only list sessions with this tag, as key or key=value (repeatable, all must match)")
	return cmd
}

// attachSettings are the parts of an attachment the picker does not vary per session.
type attachSettings struct {
	detachKey client.DetachKeySpec
	readOnly  bool
	setTitle  bool
	log       *slog.Logger
}

// attachFromPicker builds the picker's way of attaching.
//
// Deliberately not runAttach, which the `cm attach` command uses. Two of the things runAttach does are
// wrong from inside a picker, and both would show as a corrupted screen rather than as an error:
//
// It prints how the attachment ended. That write lands on a terminal bubbletea is about to repaint
// from its own model, so the message is either erased or spliced into the frame. The picker puts the
// same wording in its status line instead.
//
// It re-execs the process when the server asks the client to upgrade. That is right for a command
// whose whole job was this attachment and wrong here, because the process is also the picker: exec
// would replace the list the user is about to come back to. Reported as Stale instead.
func attachFromPicker(
	dirs paths.Dirs, envMatcher *sessionenv.Matcher, settings attachSettings,
) tui.AttachFunc {
	return func(ctx context.Context, ref string) (tui.Attachment, error) {
		// The directory is taken per attachment rather than once, since a session created later should
		// start where the user is now. It only applies when this call creates the session.
		dir, _ := os.Getwd()

		opts := client.Options{
			SocketPath: dirs.ServerSocket(),
			StartServer: func(ctx context.Context) error {
				return ensureServer(ctx, dirs)
			},
			ServerStopped: func() bool {
				_, err := os.Stat(dirs.ServerStopped())
				return err == nil
			},
			Session:   ref,
			ReadOnly:  settings.readOnly,
			Dir:       dir,
			DetachKey: settings.detachKey,
			// Captured per attachment for the same reason `cm attach` captures it: these describe the
			// terminal, and a shell already running in the session may need to refresh them.
			ClientEnv:     sessionenv.Capture(os.Environ(), envMatcher),
			Env:           sessionEnv(nil),
			SetTitle:      settings.setTitle,
			InsideSession: insideCmSession(),
			Log:           settings.log,
		}

		// A fresh TTY per attachment, opened here rather than held across the picker's life. Raw mode
		// must not be on while bubbletea is running, since bubbletea sets its own mode and restores what
		// it found; a descriptor left in raw mode by the picker would be what it "found" and would be
		// restored to that on exit, leaving the shell without echo.
		tty, err := client.OpenTTY(os.Stdin, os.Stdout)
		if err != nil {
			return tui.Attachment{}, err
		}
		res, attachErr := client.Attach(ctx, tty, opts)
		// Closed before returning so the terminal is out of raw mode when bubbletea takes it back, and
		// so the full reset has already been written: the picker repaints its whole frame anyway, so the
		// repaint that reset costs is one it was going to do.
		closeErr := tty.Close()
		if attachErr != nil {
			return tui.Attachment{}, attachErr
		}

		return tui.Attachment{
			Session:  res.Session,
			Detached: res.Detached,
			Exited:   res.Exited,
			ExitCode: res.ExitCode,
			Stale:    res.Upgrade,
		}, closeErr
	}
}

// The generated client is what the picker talks to, and tui.Sessions names the four RPCs it uses. This
// is where a change to one of their signatures fails, with the interface named, rather than inside a
// call in another package.
var _ tui.Sessions = serverv1.ServerClient(nil)
