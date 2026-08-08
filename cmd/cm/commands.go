package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/client"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/sessionenv"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newAttachCommand(g *globals) *cobra.Command {
	var (
		own       bool
		readOnly  bool
		dir       string
		setTitle  bool
		persist   bool
		onRestore string
		env       []string
	)
	cmd := &cobra.Command{
		Use:   "attach [session]",
		Short: "Attach to a session, creating it if needed",
		Long: `Attach to a session, creating it if it does not exist.

Being idempotent is what lets a terminal emulator use one command for both
creating a window's session and reattaching to it after a restart.

With no name, the server allocates one and prints it, which is how a per-window
session is created without the caller inventing names.

Detach with ctrl-\ , which leaves the session running.`,
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
				if err := paths.ValidateSessionName(session); err != nil {
					return err
				}
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

			detachKey, err := client.ParseDetachKey(cfg.DetachKey)
			if err != nil {
				return err
			}

			opts := client.Options{
				SocketPath: dirs.ServerSocket(),
				Session:    session,
				Own:        own,
				ReadOnly:   readOnly,
				Dir:        dir,
				Command:    argsAfterDash(cmd, args),
				DetachKey:  detachKey,
				// Recorded so a shell already running in this session can refresh values that
				// describe the terminal, which may have been replaced since it started.
				ClientEnv: sessionenv.Capture(os.Environ(), cfg.EnvMatcher()),
				Persist:   persist,
				OnRestore: onRestore,
				// Set on the session rather than inherited, because the shim is spawned by the server:
				// whatever this process exported is not in the server's environment and so never
				// reaches the shell. Only applies when this call creates the session.
				Env: env,
			}
			if setTitle {
				// Forward the session's title to the outer terminal.
				//
				// The shell reports its title to cm, not to the terminal, so without this a tab
				// shows the client's process name. Emitted only when output is a terminal, since
				// escape bytes in a pipe would corrupt it.
				opts.OnMetadata = func(meta client.SessionMetadata) {
					if meta.Title == "" {
						return
					}
					fmt.Fprintf(os.Stdout, "\x1b]2;%s\x07", meta.Title)
				}
			}
			return runAttach(cmd.Context(), dirs, opts)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&own, "own", false,
		"terminate the session if this client disconnects without detaching")
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
	return cmd
}

// argsAfterDash returns the command to run in a new session, which is everything after a
// literal "--". Keeping it separate from the session name means a command can contain
// anything without being mistaken for a flag.
func argsAfterDash(cmd *cobra.Command, args []string) []string {
	if n := cmd.ArgsLenAtDash(); n >= 0 && n <= len(args) {
		return args[n:]
	}
	return nil
}

func runAttach(ctx context.Context, dirs paths.Dirs, opts client.Options) error {
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

func newListCommand(g *globals) *cobra.Command {
	var (
		prefix string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List sessions",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.List(ctx, &serverv1.ListRequest{Prefix: prefix})
				if err != nil {
					return err
				}
				if asJSON {
					return printSessionsJSON(os.Stdout, resp.Sessions)
				}
				return printSessionsTable(os.Stdout, resp.Sessions)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&prefix, "prefix", "", "only sessions whose name starts with this")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

func newKillCommand(g *globals) *cobra.Command {
	var (
		force  bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:               "kill <session>...",
		Short:             "Terminate sessions and their shells",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, name := range args {
				if err := paths.ValidateSessionName(name); err != nil {
					return err
				}
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.Kill(ctx, &serverv1.KillRequest{Sessions: args, Force: force})
				if err != nil {
					return err
				}
				return reportKill(os.Stdout, resp, asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&force, "force", false,
		"forget the session even if its shim cannot be reached")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

func newSendCommand(g *globals) *cobra.Command {
	var (
		newline bool
		until   string
		timeout time.Duration
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "send <session> <text>...",
		Short: "Send input to a session without attaching",
		Long: `Send input to a session without attaching.

--wait blocks until the session reaches a state after the input lands, which is
how a script or an agent runs something and then reads the result:

  cm send build 'make' --enter --wait idle && cm read build

That is one request, not a send followed by 'cm wait', and the difference is
correctness rather than efficiency. The command starts as soon as the input
arrives, so a fast one finishes before a separate wait could be issued, and that
wait would then block until its timeout having missed what it was waiting for.
The server arms the wait before writing the input.`,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := paths.ValidateSessionName(name); err != nil {
				return err
			}
			data := strings.Join(args[1:], " ")
			if newline {
				// Carriage return, not newline: a shell at its prompt has the pty in raw
				// mode, where CR is what accept-line is bound to.
				data += "\r"
			}

			var state serverv1.WaitState
			if until != "" {
				var ok bool
				state, ok = waitStates[until]
				if !ok {
					return fmt.Errorf("unknown state %q, want one of idle, busy, blocked, exited", until)
				}
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.Send(ctx, &serverv1.SendRequest{
					Session:       name,
					Data:          []byte(data),
					WaitUntil:     state,
					WaitTimeoutMs: uint64(timeout.Milliseconds()),
				})
				if err != nil {
					return err
				}
				if resp.GetWait() == nil {
					return nil
				}
				return reportWait(os.Stdout, name, until, resp.GetWait(), asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&newline, "enter", "n", false,
		"append a carriage return so the shell runs the input")
	f.StringVar(&until, "wait", "",
		"after sending, wait until the session is idle, busy, blocked, or exited")
	f.DurationVar(&timeout, "timeout", 0,
		"give up waiting after this long (0 waits indefinitely)")
	f.BoolVar(&asJSON, "json", false, "print the wait result as JSON")
	return cmd
}

func newInfoCommand(g *globals) *cobra.Command {
	var (
		field  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "info <session>",
		Short: "Print one session's details",
		Long: `Print details for a single session.

--field prints one value alone, which is what a script wants: a terminal emulator
opening a new window in a session's directory needs the path with no header,
padding, or parsing.

cwd is empty when the session has reported a directory on another host, since
acting on a remote path locally would be wrong.`,
		Args:              sessionNameArg,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.List(ctx, &serverv1.ListRequest{})
				if err != nil {
					return err
				}
				for _, s := range resp.Sessions {
					if s.Name != args[0] {
						continue
					}
					if asJSON {
						return writeJSON(os.Stdout, toSessionJSON(s))
					}
					return printSessionInfo(os.Stdout, s, field)
				}
				return fmt.Errorf("session %q not found", args[0])
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&field, "field", "",
		"print only this field: name, state, pid, clients, title, cwd, cwd_uri, cwd_is_local")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

func newHistoryCommand(g *globals) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "history <session>",
		Short: "Print a session's contents, scrollback included",
		Long: `Print a session's contents, including scrollback.

Plain text by default, so it can be piped or paged. --format=vt keeps colors and
styling; --format=html produces styled markup.`,
		Args:              sessionNameArg,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			var f serverv1.HistoryFormat
			switch format {
			case "plain", "":
				f = serverv1.HistoryFormat_HISTORY_FORMAT_UNSPECIFIED
			case "vt":
				f = serverv1.HistoryFormat_HISTORY_FORMAT_VT
			case "html":
				f = serverv1.HistoryFormat_HISTORY_FORMAT_HTML
			default:
				return fmt.Errorf("unknown format %q, want plain, vt, or html", format)
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.History(ctx, &serverv1.HistoryRequest{
					Session: args[0],
					Format:  f,
				})
				if err != nil {
					return err
				}
				_, err = os.Stdout.Write(resp.Data)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&format, "format", "plain", "output format: plain, vt, or html")
	return cmd
}

func newCompletionsCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "completions <shell>",
		Short:     "Print a shell completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell %q", args[0])
			}
		},
	}
}
