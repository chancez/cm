package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/spaces"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newSpaceCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "space",
		Aliases: []string{"spaces"},
		Short:   "List and manage spaces, the groups sessions are organized into",
		Long: `List and manage spaces.

Every session is in exactly one space: the one named by --space when it was
created, else the space of the session it was created from, else "default". A
space outlives its sessions, so a session whose window was closed is found again
in the space it was left in:

  cm attach --space infra
  cm ls --space infra
  cm space move infra work

Spaces differ from tags in being one answer rather than several. A session can
carry many tags or none, so "where is it" has no single reply from them, and a
tag nothing carries does not exist. Tags still work inside a space and across
spaces.

Names are letters, digits, '-', '_' and '.', and may start with '.', as a
repository's directory such as .dotfiles does; '.' and '..' are refused.`,
	}
	cmd.AddCommand(
		newSpaceListCommand(g),
		newSpaceCreateCommand(g),
		newSpaceRemoveCommand(g),
		newSpaceMoveCommand(g),
		newSpaceMetaCommand(g),
	)
	return cmd
}

// requireSpaces refuses up front against a server that cannot hold spaces.
//
// Needed on every path that names one, because the failure without it is silent: an older server reads
// Open.space and ListRequest.space as absent, so a session asked into a space lands in none, and a listing
// asked for one space lists all of them.
func requireSpaces(ctx context.Context, cl serverv1.ServerClient) error {
	return requireServerCapability(serverCapabilities(ctx, cl), capability.Spaces, "organize sessions into spaces")
}

// validateSpaceArg checks a --space value client-side, so a typo is reported the same way whether or not a
// server is running. Empty means none was given.
func validateSpaceArg(space string) error {
	if space == "" {
		return nil
	}
	if err := spaces.ValidateName(space); err != nil {
		return fmt.Errorf("--space: %w", err)
	}
	return nil
}

// spaceFrom is the session a created session takes its space from: the one this command runs inside, unless
// the server is another machine's, which cannot resolve a local session and could match an unrelated one.
func spaceFrom(g *globals) string {
	if g.remote != "" {
		return ""
	}
	return os.Getenv(paths.SessionEnv())
}

func newSpaceListCommand(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List spaces and how many sessions each holds",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireSpaces(ctx, cl); err != nil {
					return err
				}
				resp, err := cl.Spaces(ctx, &serverv1.SpacesRequest{})
				if err != nil {
					return err
				}
				if asJSON {
					return printSpacesJSON(os.Stdout, resp.Spaces)
				}
				return printSpacesTable(os.Stdout, resp.Spaces)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

// spaceJSON is a space as `cm space list --json` prints it.
type spaceJSON struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Sessions  uint32    `json:"sessions"`
	Running   uint32    `json:"running"`
}

func printSpacesJSON(w io.Writer, all []*serverv1.Space) error {
	out := make([]spaceJSON, 0, len(all))
	for _, sp := range all {
		out = append(out, spaceJSON{
			Name:      sp.Name,
			CreatedAt: time.Unix(sp.CreatedAtUnix, 0),
			Sessions:  sp.Sessions,
			Running:   sp.Running,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func printSpacesTable(w io.Writer, all []*serverv1.Space) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSESSIONS\tRUNNING")
	for _, sp := range all {
		fmt.Fprintf(tw, "%s\t%d\t%d\n", sp.Name, sp.Sessions, sp.Running)
	}
	return tw.Flush()
}

func newSpaceCreateCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		// A new name, so nothing to offer; and no files, which is cobra's default otherwise.
		ValidArgsFunction: cobra.NoFileCompletions,
		Use:               "create NAME",
		Short:             "Create an empty space",
		Long: `Create an empty space. Creating one that exists already succeeds.

Not needed before putting a session in a space, since --space and 'cm space move'
create the space they name. This is for a space wanted before anything is in it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := spaces.ValidateName(args[0]); err != nil {
				return err
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireSpaces(ctx, cl); err != nil {
					return err
				}
				_, err := cl.CreateSpace(ctx, &serverv1.CreateSpaceRequest{Name: args[0]})
				return err
			})
		},
	}
}

func newSpaceRemoveCommand(g *globals) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "remove NAME",
		Aliases: []string{"rm"},
		Short:   "Remove a space, and with --force every session in it",
		Long: `Remove a space that no session is in.

A space holding any session is refused, shelved and exited sessions included,
rather than emptied into default: a space is how its sessions are found, and
scattering them into default would lose that. Move or forget them first. The
default space cannot be removed.

--force ends every session in the space first, as 'cm kill' and 'cm forget'
would: running ones are killed and ended ones forgotten, their saved output with
them. A session whose shim does not answer is left, as 'cm kill' leaves one
without --force, and the space with it; the command says which.`,
		Args: cobra.ExactArgs(1),
		// With --force any space but default can go, so the offer is every one of them.
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			want := removableSpace
			if force {
				want = func(sp *serverv1.Space) bool { return sp.Name != spaces.Default }
			}
			return completeSpaceArg(g, want)(cmd, args, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := spaces.ValidateName(args[0]); err != nil {
				return err
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireSpaces(ctx, cl); err != nil {
					return err
				}
				resp, err := cl.RemoveSpace(ctx, &serverv1.RemoveSpaceRequest{Name: args[0], Force: force})
				if err != nil {
					return err
				}
				for _, ref := range resp.Ended {
					fmt.Printf("ended %s\n", ref)
				}
				if len(resp.Errors) > 0 {
					return fmt.Errorf("%s was not removed: %w", args[0], reportedErrors(resp.Errors, false))
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "end every session in the space, then remove it")
	return cmd
}

func newSpaceMoveCommand(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "move SPACE [SESSION]...",
		Aliases: []string{"mv"},
		Short:   "Move sessions to another space",
		Long: `Move sessions to another space, creating it if it does not exist.

With no session, the one this command is running in is moved, from ` + paths.SessionEnv() + `.
Ended sessions move too, since a shelved session in the wrong space is the one
that is hardest to find.`,
		Args: cobra.MinimumNArgs(1),
		// The space first, then sessions, which may name several.
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				return completeSpaceNames(g, nil)(cmd, args, toComplete)
			}
			return completeSessionNames(g)(cmd, args[1:], toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			space, refs := args[0], args[1:]
			if err := spaces.ValidateName(space); err != nil {
				return err
			}
			if len(refs) == 0 {
				self, err := resolveSession(nil)
				if err != nil {
					return err
				}
				refs = []string{self}
			}
			for _, ref := range refs {
				if err := paths.ValidateSessionRef(ref); err != nil {
					return err
				}
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireSpaces(ctx, cl); err != nil {
					return err
				}
				resp, err := cl.Move(ctx, &serverv1.MoveRequest{Sessions: refs, Space: space})
				if err != nil {
					return err
				}
				return reportMove(os.Stdout, space, resp, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// reportMove prints what moved from where, and fails if anything could not be moved.
func reportMove(w io.Writer, space string, resp *serverv1.MoveResponse, asJSON bool) error {
	if asJSON {
		out := struct {
			Space  string            `json:"space"`
			Moved  map[string]string `json:"moved"`
			Errors map[string]string `json:"errors"`
		}{Space: space, Moved: resp.Moved, Errors: resp.Errors}
		// Empty rather than null, so a script can iterate either unconditionally.
		if out.Moved == nil {
			out.Moved = map[string]string{}
		}
		if out.Errors == nil {
			out.Errors = map[string]string{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return err
		}
	} else {
		for _, ref := range sortedKeys(resp.Moved) {
			fmt.Fprintf(w, "moved %s: %s -> %s\n", ref, resp.Moved[ref], space)
		}
	}
	return reportedErrors(resp.Errors, asJSON)
}

// completeSpaceNames returns a completion function offering the server's spaces, for --space and for a
// command's first argument. want filters which are offered, nil offering all; a command whose later
// arguments are not spaces takes none past the first.
//
// Deliberately starts no server, as session completion does not: see completeSessionNames.
func completeSpaceNames(
	g *globals, want func(*serverv1.Space) bool,
) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		all, err := listSpaces(cmd.Context(), g)
		if err != nil {
			// Silence rather than an error: a completion that prints a diagnostic corrupts the command line
			// being typed. An older server answers Unimplemented, which lands here too.
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return spaceCandidates(all, toComplete, want), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeSpaceArg completes a command whose only argument is a space.
func completeSpaceArg(
	g *globals, want func(*serverv1.Space) bool,
) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	spaceNames := completeSpaceNames(g, want)
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return spaceNames(cmd, args, toComplete)
	}
}

// listSpaces asks whichever server this invocation names, bounded as every completion is. See
// globals.completionServer.
func listSpaces(ctx context.Context, g *globals) ([]*serverv1.Space, error) {
	ctx, cancel := g.completionDeadline(ctx)
	defer cancel()
	conn, cl, err := g.completionServer(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := cl.Spaces(ctx, &serverv1.SpacesRequest{})
	if err != nil {
		return nil, err
	}
	return resp.Spaces, nil
}

// spaceCandidates turns spaces into completion candidates for what has been typed so far, each described by
// what it holds. Split from listSpaces so the candidates are testable without a server.
func spaceCandidates(all []*serverv1.Space, prefix string, want func(*serverv1.Space) bool) []string {
	out := make([]string, 0, len(all))
	for _, sp := range all {
		if !strings.HasPrefix(sp.Name, prefix) || (want != nil && !want(sp)) {
			continue
		}
		out = append(out, fmt.Sprintf("%s\t%d sessions, %d running", sp.Name, sp.Sessions, sp.Running))
	}
	return out
}

// removableSpace is a space `cm space remove` accepts: empty, and not default. Offering one it refuses
// completes to a guaranteed refusal, the reason completeEndedSessionNames filters.
func removableSpace(sp *serverv1.Space) bool {
	return sp.Sessions == 0 && sp.Name != spaces.Default
}
