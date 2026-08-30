package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/client"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/sessionenv"
	"github.com/chancez/cm/internal/tags"
)

func newAttachCommand(g *globals) *cobra.Command {
	var (
		readOnly     bool
		dir          string
		setTitle     bool
		persist      bool
		onRestore    string
		env          []string
		detachKeyArg string
		noAttach     bool
		tagArgs      []string
		// Bound to the vestigial --resume-from-seq and never read. Named for what it is so a reader
		// looking for where the position comes from does not stop here.
		ignoredResumeFrom uint64
	)
	cmd := &cobra.Command{
		Use: "attach [session]",
		// The one command typed dozens of times a day, so it gets the shortest name that is not ambiguous.
		Aliases: []string{"a"},
		Short:   "Attach to a session, creating it if needed",
		Long: `Attach to a session, creating it if it does not exist.

Being idempotent is what lets a terminal emulator use one command for both
creating a window's session and reattaching to it after a restart.

With no name, the server allocates one and prints it, which is how a per-window
session is created without the caller inventing names.

--no-attach creates the session and prints its name without attaching, for
pre-creating one that something else will attach to later. Distinct from
'cm run -d', which needs a command and captures its output for a few minutes: this
makes an ordinary shell session with ordinary persistence.

Detaching leaves the session running. The key is ctrl-\ by default, set by
detach_key in the config file, and overridden for one attachment by --detach-key.

That override matters when something outside this client already claims the key: a
multiplexer you are attaching from within sees it first, so the inner client never
receives it and the window closes instead of detaching.`,
		// Only the args before "--" are the session name; everything after is the command
		// to run, so MaximumNArgs would wrongly reject "attach x -- sh -c ...".
		Args: func(cmd *cobra.Command, args []string) error {
			n := cmd.ArgsLenAtDash()
			if n < 0 {
				n = len(args)
			}
			if n > 1 {
				return fmt.Errorf("expected at most one session name before --, got %d", n)
			}
			return nil
		},
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			var session string
			if n := cmd.ArgsLenAtDash(); n != 0 && len(args) > 0 {
				session = args[0]
				if err := paths.ValidateSessionRef(session); err != nil {
					return err
				}
			}
			sessionTags, err := tags.ParseAll(tagArgs)
			if err != nil {
				return err
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			if dir == "" {
				// Default to the caller's cwd so a new session starts where the user is,
				// which is what a terminal emulator opening a window expects.
				dir, _ = os.Getwd()
			}
			cfg, err := g.config()
			if err != nil {
				return err
			}

			// The flag wins over the config file, as flags do. Worth having as a flag and not only a
			// setting: the case for changing it is usually one attachment rather than every one, such as
			// attaching from inside another multiplexer that claims ctrl-\ for itself.
			keySpec := cfg.DetachKey
			if detachKeyArg != "" {
				keySpec = detachKeyArg
			}
			detachKey, err := client.ParseDetachKey(keySpec)
			if err != nil {
				return err
			}

			// Taken before anything reads the environment, because Env below forwards this process's
			// whole environment to a session this call creates. See takeResumeFrom.
			resumeFrom := takeResumeFrom(os.Getpid())

			opts := client.Options{
				SocketPath: dirs.ServerSocket(),
				// Recovery for a window whose server died. Every cm command already starts one when none is
				// running, which is why the fix for a frozen window was to open a new one and run any of
				// them; this hands the same machinery to the client that noticed.
				StartServer: func(ctx context.Context) error { return ensureServer(ctx, dirs) },
				// Honored so a stop stays stopped. See paths.ServerStopped.
				ServerStopped: func() bool {
					_, err := os.Stat(dirs.ServerStopped())
					return err == nil
				},
				Session:   session,
				ReadOnly:  readOnly,
				Dir:       dir,
				Command:   argsAfterDash(cmd, args),
				DetachKey: detachKey,
				// Recorded so a shell already running in this session can refresh values that
				// describe the terminal, which may have been replaced since it started.
				ClientEnv: sessionenv.Capture(os.Environ(), cfg.EnvMatcher()),
				Persist:   persist,
				OnRestore: onRestore,
				Tags:      sessionTags,
				// Set on the session rather than inherited, because the shim is spawned by the server:
				// whatever this process exported is not in the server's environment and so never
				// reaches the shell. Only applies when this call creates the session.
				//
				// Carries this client's whole environment as well as --env, so a session resembles
				// the thing that created it rather than the shell that happened to start the server,
				// which may be days old. See sessionEnv.
				Env: sessionEnv(env),
				// Tells the server this attach is nested, so the session it is running inside stops
				// reading the bytes passing through it as reports about itself.
				InsideSession: insideCmSession(),
			}
			// Nil unless this process replaced one that was already attached, so an ordinary attach
			// still repaints.
			opts.ResumeFrom = resumeFrom
			logger, closeLog := newClientLogger(dirs, cfg)
			if closeLog != nil {
				defer closeLog.Close()
			}
			opts.Log = logger

			// Checked before anything that assumes a terminal, since this path has none.
			if noAttach {
				return createWithoutAttaching(cmd.Context(), dirs, opts)
			}

			// Forward the session's title to the outer terminal.
			//
			// A flag rather than a callback that writes the sequence here, which is what this was and
			// where it was a bug: writing to os.Stdout bypassed the terminal internal/client owns, so
			// the title landed inside whatever escape sequence the session was halfway through and the
			// remainder printed as text on screen. The command layer states the policy; the client owns
			// every byte that reaches a terminal. See internal/client.screen.
			opts.SetTitle = setTitle
			return runAttach(cmd.Context(), dirs, opts, func(res client.Result) []string {
				return upgradeArgv(cmd, args, res)
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&readOnly, "read-only", false,
		"follow the session without sending input")
	f.StringVar(&dir, "dir", "", "working directory for a newly created session")
	f.BoolVar(&setTitle, "set-title", true,
		"forward the session's title to the terminal")
	f.BoolVar(&persist, "persist", false,
		"keep this session's content across a reboot")
	f.StringVar(&onRestore, "on-restore", "",
		"what to do when reviving this session: shell, none, or command")
	f.StringArrayVar(&env, "env", nil,
		"set a KEY=VALUE in the new session's environment (repeatable, ignored when reattaching)")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"label the new session, as key or key=value (repeatable, ignored when reattaching)")
	f.StringVar(&detachKeyArg, "detach-key", "",
		`key that detaches this client: "ctrl-<key>" or "none" (default from config)`)
	f.BoolVar(&noAttach, "no-attach", false,
		"create the session and print its name without attaching")
	// Accepted and ignored, and both halves are deliberate.
	//
	// Honoring it is the bug: a position from a stream this process never saw suppresses the screen
	// repaint, so the window comes back blank. See resumeEnvVar, which is where a position travels now.
	//
	// Still parsed because an older build puts it in the argv it re-execs, so the first upgrade after
	// this change hands it to a binary that has to accept it. `unknown flag` there means the client
	// exits instead of attaching and the window is left holding a dead terminal, which is worse than the
	// bug, and it would happen to every attached client at once. It also covers a command line recorded
	// by something that watched a live process. Once one upgrade has been through, the flag is dead
	// weight and can go; see docs/ideas.md.
	//
	// The cost of ignoring it is that that first upgrade repaints once, since the old process hands over
	// in the argv and the new one only reads the environment. One clear plus a snapshot, which is what a
	// fresh attach does anyway.
	f.Uint64Var(&ignoredResumeFrom, resumeFromFlag, 0,
		"ignored; kept so an argv recorded by an older build still parses")
	_ = f.MarkHidden(resumeFromFlag)
	return cmd
}

// runAttach attaches a terminal to a session and reports how it ended.
//
// upgradeArgv builds the argv for a replacement process when the server asks this client to upgrade, or
// is nil for a caller that cannot be upgraded. Passed in rather than built here because it depends on
// the parsed command, which belongs to the caller: only it knows which flags were actually set.
func runAttach(
	ctx context.Context, dirs paths.Dirs, opts client.Options,
	upgradeArgv func(client.Result) []string,
) error {
	// Attaching must work whether or not a server happens to be running, which is what
	// lets the user never think about one.
	if err := ensureServer(ctx, dirs); err != nil {
		return err
	}

	tty, err := client.OpenTTY(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}

	res, attachErr := client.Attach(ctx, tty, opts)

	// An upgrade replaces this process before the terminal is restored, deliberately.
	//
	// tty.Close writes a full reset and puts the terminal back the way it was found, which is right for
	// a client that is finishing and wrong for one that is about to be replaced: the reset clears the
	// session off the screen, and restoring cooked mode means the replacement has to enter raw mode
	// again. Both are visible, and avoiding them is the whole point of upgrading in place rather than
	// telling someone to close the window. exec keeps the same process, the same descriptors, and the
	// terminal exactly as it is, so the replacement resumes into a screen that never changed.
	//
	// Errors fall through to the ordinary teardown below. A failed exec means this process is still
	// here holding a terminal in raw mode, so it has to restore it and report, rather than leaving a
	// window that looks alive and answers nothing.
	if res.Upgrade && attachErr == nil && upgradeArgv != nil {
		if err := reexecForUpgrade(upgradeArgv(res), res.ResumeFrom); err != nil {
			// Restored first, for the reason above: the message must be readable and the shell that
			// regains this terminal must not inherit raw mode.
			_ = tty.Close()
			return fmt.Errorf("upgrading the client for %s: %w", res.Session, err)
		}
	}

	// Restore the terminal before printing anything, so the message is not erased by the
	// reset sequence. Close is idempotent, but calling it once keeps the reset from being
	// emitted twice, which would leave a stray character on screen.
	closeErr := tty.Close()

	if attachErr != nil {
		return attachErr
	}
	switch {
	case res.Exited && res.ExitCode < 0:
		// A negative code means the shim became unreachable rather than the shell reporting a
		// status, so there is no exit code worth showing.
		fmt.Fprintf(os.Stderr, "session %s ended unexpectedly\n", res.Session)
	case res.Exited:
		fmt.Fprintf(os.Stderr, "session %s ended (exit %d)\n", res.Session, res.ExitCode)
	case res.Detached:
		fmt.Fprintf(os.Stderr, "detached from %s\n", res.Session)
	}
	return closeErr
}
