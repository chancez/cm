package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// newClientCommand groups the commands that act on attached clients rather than on sessions.
//
// A parent with one subcommand today, because "upgrade" alone at the top level would read as upgrading
// cm itself, which is the package manager's job and not something cm should claim. `cm client upgrade`
// says what it acts on.
func newClientCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "client",
		Short: "Act on attached clients",
		Long: `Act on the clients attached to sessions, rather than on the sessions themselves.

A client is the process holding a terminal and streaming a session through it. It
owns nothing that cannot be rebuilt: the session lives in its shim and the screen
can be resumed from a recorded position, which is what makes a client the one part
of cm that can be replaced without anyone losing work.`,
	}
	cmd.AddCommand(newClientUpgradeCommand(g))
	return cmd
}

func newClientUpgradeCommand(g *globals) *cobra.Command {
	var (
		asJSON  bool
		all     bool
		force   bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:   "upgrade [session]...",
		Short: "Replace attached clients with the newest build, keeping sessions on screen",
		Long: `Ask attached clients to restart on the binary now installed, without losing the
session on screen.

This is the client half of an upgrade. 'cm server restart' replaces the server and
adopts every session, but it leaves the clients alone, so after installing a new
build the windows keep running whatever they started with. Until this existed the
only way to update one was to detach and reattach, or close the window, which is a
repaint at best and a lost shell at worst.

Nothing is lost because a client holds nothing. The shim owns the pty, the server
owns the bookkeeping, and the client already knows how to reattach from a recorded
position because that is what it does every time the server restarts. Upgrading
reuses that: the client re-execs itself and resumes where it stopped, so the
terminal shows the same screen it did a moment earlier.

  cm client upgrade          # the session I am in
  cm client upgrade work     # a specific one
  cm client upgrade --all    # every client the server has

Shims are deliberately not upgraded, and cannot be. A shim holds the pty, so
replacing one means ending the shell in it. 'cm doctor' reports how many builds the
running shims span, and the only way to change that is to end a session and start a
new one, which is a trade rather than a repair.

Clients already running the server's build are skipped, so this is safe to run
twice. --force asks them anyway, which is how to restart clients after a config
change rather than a new build.

A session with nothing attached is not an error. It reports 0 clients, because
there is nothing to upgrade, which is what makes this safe to call over a whole
server without checking first.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if all || len(tagArgs) > 0 {
				if len(args) > 0 {
					return errors.New("--all and --tag take no session names")
				}
			}
			return nil
		},
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(tagArgs) > 0 {
				// Refused rather than resolved, matching kill and detach: --all means every session and
				// --tag means a subset, so one of the two was a mistake.
				return errors.New("--all and --tag cannot be combined; --all is already every session")
			}
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}

			// The calling session is the default target, like `cm detach`. A bare `cm client upgrade` from
			// inside a session upgrades that session's client, which is the shape a keybinding wants.
			if len(args) == 0 && !all && len(tagArgs) == 0 {
				name, err := sessionTarget(args, "upgrade the client of")
				if err != nil {
					return err
				}
				args = []string{name}
			}
			for _, name := range args {
				if err := paths.ValidateSessionName(name); err != nil {
					return err
				}
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Deliberately not ensureServer, matching detach: upgrading a client implies clients exist,
			// and starting a server here would create one that has never heard of the session and then
			// report it as having nothing attached.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				names := args
				if len(tagArgs) > 0 {
					// A selector matching nothing is an error, unlike --all on an empty server: it is
					// usually a typo, and exiting 0 would let a script report success having done nothing.
					names, err = resolveSelector(ctx, cl, tagArgs)
					if err != nil {
						return err
					}
					if len(names) == 0 {
						return fmt.Errorf("no sessions match %s", describeSelectors(tagArgs))
					}
				}
				// --all sends no names at all, unlike detach, which enumerates them client-side. The server
				// already treats an empty list as every session for this RPC, and enumerating here would
				// race a session created between the list and the request: it would be missed, leaving one
				// window on the old build after a command that said it upgraded everything.
				resp, err := cl.UpgradeClients(ctx, &serverv1.UpgradeClientsRequest{
					Sessions: names,
					Force:    force,
				})
				if err != nil {
					return err
				}
				return reportUpgrade(os.Stdout, resp, asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&all, "all", false, "upgrade the clients of every session")
	f.BoolVar(&force, "force", false,
		"ask clients already running the server's build to restart anyway")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"upgrade the clients of sessions with this tag, as key or key=value (repeatable)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

// upgradeJSON is one session's result, for --json.
//
// A named type rather than an inline map so the key names are declared once and cannot drift between the
// table and the JSON, matching detachJSON.
type upgradeJSON struct {
	Session string `json:"session"`
	// Asked is how many clients were asked to upgrade. Zero means nothing was attached, or everything
	// attached was already current.
	//
	// Asked rather than upgraded, and the distinction is honest rather than pedantic: the server sends
	// the request and closes the stream, so whether a client comes back is known only to the client. One
	// too old to understand the request exits instead of returning.
	Asked uint32 `json:"asked"`
	// AlreadyCurrent is how many were left alone for already running the server's build. Always 0 with
	// --force, which skips nothing.
	AlreadyCurrent uint32 `json:"already_current"`
	// Error is why this session could not be reached, empty when it was.
	Error string `json:"error,omitempty"`
}

// reportUpgrade prints what was asked to upgrade, and fails when any session errored.
//
// Errors are reported per session rather than aborting at the first, so an upgrade over a whole server
// still reaches the rest when one record is bad. The exit status covers them, so a script does not have
// to parse the output to notice.
func reportUpgrade(w io.Writer, resp *serverv1.UpgradeClientsResponse, asJSON bool) error {
	names := make([]string, 0, len(resp.Asked)+len(resp.Errors))
	for name := range resp.Asked {
		names = append(names, name)
	}
	for name := range resp.Errors {
		names = append(names, name)
	}
	// Sorted so output is stable between runs rather than following Go's map ordering, which a script
	// diffing output would read as change.
	sort.Strings(names)

	out := make([]upgradeJSON, 0, len(names))
	for _, name := range names {
		out = append(out, upgradeJSON{
			Session:        name,
			Asked:          resp.Asked[name],
			AlreadyCurrent: resp.AlreadyCurrent[name],
			Error:          resp.Errors[name],
		})
	}

	if asJSON {
		if err := writeJSON(w, out); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SESSION\tASKED\tRESULT")
		for _, r := range out {
			// The result column carries the one thing worth saying about each row, rather than a column
			// per outcome that is empty on almost every line. Same shape as reportDetach.
			result := ""
			switch {
			case r.Error != "":
				result = r.Error
			case r.AlreadyCurrent > 0:
				// Stated rather than shown as a bare "asked 0", since "already on this build" is the useful
				// statement and is not a failure. This is the ordinary result of running the command twice,
				// and it is worth saying even when some clients were also asked.
				result = fmt.Sprintf("%d already current", r.AlreadyCurrent)
			case r.Asked == 0:
				result = "no clients attached"
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\n", r.Session, r.Asked, result)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	if len(resp.Errors) == 0 {
		return nil
	}
	if asJSON {
		// The detail is already in the payload, so the error only sets the exit status rather than
		// duplicating it onto stderr. Same convention as reportDetach and reportKill.
		return errAlreadyReported
	}
	// Named per session rather than counted, so a caller reading only stderr still gets the detail.
	failed := make([]string, 0, len(resp.Errors))
	for _, r := range out {
		if r.Error != "" {
			failed = append(failed, fmt.Sprintf("%s: %s", r.Session, r.Error))
		}
	}
	return fmt.Errorf("%s", strings.Join(failed, "; "))
}
