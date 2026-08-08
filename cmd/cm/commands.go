package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/client"
	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newAttachCommand(g *globals) *cobra.Command {
	var (
		own      bool
		readOnly bool
		dir      string
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
			return runAttach(cmd.Context(), dirs, client.Options{
				SocketPath: dirs.ServerSocket(),
				Session:    session,
				Own:        own,
				ReadOnly:   readOnly,
				Dir:        dir,
				Command:    argsAfterDash(cmd, args),
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&own, "own", false,
		"terminate the session if this client disconnects without detaching")
	f.BoolVar(&readOnly, "read-only", false,
		"follow the session without sending input")
	f.StringVar(&dir, "dir", "", "working directory for a newly created session")
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
	case res.Exited:
		fmt.Fprintf(os.Stderr, "session %s ended (exit %d)\n", res.Session, res.ExitCode)
	case res.Detached:
		fmt.Fprintf(os.Stderr, "detached from %s\n", res.Session)
	}
	return closeErr
}

func newListCommand(g *globals) *cobra.Command {
	var prefix string
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
				return printSessions(os.Stdout, resp.Sessions)
			})
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "only sessions whose name starts with this")
	return cmd
}

func printSessions(w *os.File, sessions []*serverv1.Session) error {
	if len(sessions) == 0 {
		return nil
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].CreatedAtUnix < sessions[j].CreatedAtUnix
	})

	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPID\tCLIENTS\tSTATE\tCREATED\tCWD")
	for _, s := range sessions {
		state := "running"
		if s.Exited {
			state = fmt.Sprintf("exited(%d)", s.ExitCode)
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\n",
			s.Name, s.ShellPid, s.Clients, state,
			humanAge(time.Unix(s.CreatedAtUnix, 0)), s.Cwd)
	}
	return tw.Flush()
}

// humanAge renders an age compactly, since a full timestamp crowds out the columns that
// matter when scanning a session list.
func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func newKillCommand(g *globals) *cobra.Command {
	var force bool
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
				for _, name := range resp.Killed {
					fmt.Printf("killed %s\n", name)
				}
				if len(resp.Errors) > 0 {
					var msgs []string
					for name, msg := range resp.Errors {
						msgs = append(msgs, fmt.Sprintf("%s: %s", name, msg))
					}
					sort.Strings(msgs)
					return fmt.Errorf("%s", strings.Join(msgs, "; "))
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"forget the session even if its shim cannot be reached")
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
