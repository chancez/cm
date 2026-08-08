package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/client"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/sessionenv"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newAttachCommand(g *globals) *cobra.Command {
	var (
		own      bool
		readOnly bool
		dir      string
		setTitle bool
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
		Use:   "kill <session>...",
		Short: "Terminate sessions and their shells",
		Args:  cobra.MinimumNArgs(1),
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
	var newline bool
	cmd := &cobra.Command{
		Use:   "send <session> <text>...",
		Short: "Send input to a session without attaching",
		Args:  cobra.MinimumNArgs(2),
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
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				_, err := cl.Send(ctx, &serverv1.SendRequest{
					Session: name,
					Data:    []byte(data),
				})
				return err
			})
		},
	}
	cmd.Flags().BoolVarP(&newline, "enter", "n", false,
		"append a carriage return so the shell runs the input")
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
		Args: sessionNameArg,
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
		Args: sessionNameArg,
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
