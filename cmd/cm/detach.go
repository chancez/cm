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

func newDetachCommand(g *globals) *cobra.Command {
	var (
		asJSON  bool
		all     bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:   "detach [session]...",
		Short: "Disconnect a session's clients, leaving it running",
		Long: `Disconnect a session's clients without ending the session.

The shell keeps running, exactly as it does when you press the detach key. This is
the same operation, reachable without a keyboard.

With no arguments the session this command is running in is used, from CM_SESSION,
so 'cm detach' inside a session detaches that session's own clients:

  cm detach          # let go of the session I am in
  cm detach inner    # let go of a specific one
  cm detach --all    # clear every client the server has

Two cases the detach key cannot reach.

A nested attach is the first. The key is delivered to whichever client owns the
real terminal, which is the outermost one, so pressing it inside a nested session
detaches the parent rather than the child. Naming the session says which one you
meant:

  cm detach inner

cm deliberately does not guess. The key would have to target a session chosen from
state you cannot see, and guessing wrong against an owned session ends a shell
rather than releasing it.

The second is anything without a keyboard. A script or an agent that started a
client had no way to disconnect it, since every other route to detaching is a
keypress.

Detaching a session nobody is attached to is not an error. It reports 0 clients,
because the session is already in the state you asked for, which is what makes
this safe to call without checking first.`,
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
				// Refused rather than resolved, matching kill: --all means every session and --tag means a
				// subset, so one of the two was a mistake.
				return errors.New("--all and --tag cannot be combined; --all is already every session")
			}
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}

			// The calling session is the default target, but only when nothing else selected one. A bare
			// `cm detach` inside a session is the shape a keybinding or a shell alias wants, and it does
			// not retarget anything: it names the session whose clients to release, which is the session
			// the caller is already in.
			if len(args) == 0 && !all && len(tagArgs) == 0 {
				name, err := sessionTarget(args, "detach")
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
			// Deliberately not ensureServer: detaching implies clients exist, and starting a server here
			// would create one that has never heard of the session and report it detached.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				names := args
				switch {
				case all:
					// Enumerated client-side, like kill --all, so the server keeps one meaning for a detach
					// request. An empty server is a satisfied request rather than an error.
					listed, err := cl.List(ctx, &serverv1.ListRequest{})
					if err != nil {
						return err
					}
					names = make([]string, 0, len(listed.Sessions))
					for _, s := range listed.Sessions {
						names = append(names, s.Name)
					}
					if len(names) == 0 {
						return nil
					}
				case len(tagArgs) > 0:
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
				resp, err := cl.Detach(ctx, &serverv1.DetachRequest{Sessions: names})
				if err != nil {
					return err
				}
				return reportDetach(os.Stdout, resp, asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&all, "all", false, "detach the clients of every session")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"detach the clients of sessions with this tag, as key or key=value (repeatable)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

// detachJSON is one session's result, for --json.
//
// A named type rather than an inline map so the key names are declared once and cannot drift between
// the table and the JSON.
type detachJSON struct {
	Session string `json:"session"`
	// Clients is how many were disconnected. Zero means nothing was attached.
	Clients uint32 `json:"clients"`
	// Error is why this session could not be detached, empty when it was.
	Error string `json:"error,omitempty"`
}

// reportDetach prints what was detached, and fails when any session errored.
//
// Errors are reported per session rather than aborting at the first, so `cm detach --all` against a
// server holding one bad record still detaches the rest. The exit status covers them, so a script does
// not have to parse the output to notice.
func reportDetach(w io.Writer, resp *serverv1.DetachResponse, asJSON bool) error {
	names := make([]string, 0, len(resp.Detached)+len(resp.Errors))
	for name := range resp.Detached {
		names = append(names, name)
	}
	for name := range resp.Errors {
		names = append(names, name)
	}
	// Sorted so output is stable between runs rather than following Go's map ordering, which a script
	// diffing output would see as spurious change.
	sort.Strings(names)

	out := make([]detachJSON, 0, len(names))
	for _, name := range names {
		out = append(out, detachJSON{
			Session: name,
			Clients: resp.Detached[name],
			Error:   resp.Errors[name],
		})
	}

	if asJSON {
		if err := writeJSON(w, out); err != nil {
			return err
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "SESSION\tCLIENTS\tRESULT")
		for _, d := range out {
			result := "detached"
			switch {
			case d.Error != "":
				result = d.Error
			case d.Clients == 0:
				// Named rather than shown as "detached 0", since "nothing was attached" is the useful
				// statement and it is not a failure.
				result = "no clients attached"
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\n", d.Session, d.Clients, result)
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
		// duplicating it onto stderr. Same convention as reportKill.
		return errAlreadyReported
	}
	// Named per session rather than counted, so the message says which ones and why. The table above
	// already showed them, but a caller reading only stderr still gets the detail.
	failed := make([]string, 0, len(resp.Errors))
	for _, d := range out {
		if d.Error != "" {
			failed = append(failed, fmt.Sprintf("%s: %s", d.Session, d.Error))
		}
	}
	return fmt.Errorf("%s", strings.Join(failed, "; "))
}
