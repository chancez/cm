package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newBindCommand(g *globals) *cobra.Command {
	var (
		move   bool
		borrow bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "bind <name> [session]",
		Short: "Point a name at a session",
		Long: `Point a name at a session, so attaching by that name reaches it.

  cm bind refactor              # name the session this is running in
  cm bind work @a7k2m9x4        # name a session that had none
  cm bind build work            # a second name for the same session
  cm bind work review --move    # point an existing name somewhere else
  cm unbind build

With no session, the one this command is running in is used, from CM_SESSION, so
naming the session in front of you is one word and needs nothing looked up.

A name is not a session's identity. A session is identified by its ID, which is
allocated when it is created and never changes, and a name is a separate thing
pointing at one. So a session can have several names or none, any name can be moved
to another session, and none of it disturbs the session: nothing restarts, no socket
moves, and a shell's CM_SESSION keeps meaning what it meant.

That is what makes this useful with a terminal emulator. A window's launch command is
fixed when the window is created and saved verbatim, so the name in it is the only
thing that survives a restart, and pointing that name at a different session is how
the window comes back to the session you want rather than the one it made.

--move is required to repoint a name that already names something. The default
refuses, because naming a session is ordinary while taking a name off the session a
window is watching sends that window somewhere its user did not put it.

--borrow changes what killing by this name means: it releases the name and leaves the
session running, which is the session equivalent of detaching. Without it, 'cm kill
<name>' kills the session, which is what it has always meant. Use it for a name whose
window borrowed a session that lives elsewhere, so closing the window lets go rather
than ending someone's shell.`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeBindArgs(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, session, err := splitBindArgs(args)
			if err != nil {
				return err
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Deliberately not ensureServer, matching `cm tag`: binding a name implies a session exists,
			// and a server started here has never heard of it.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.Bind(ctx, &serverv1.BindRequest{
					Name:    name,
					Session: session,
					Move:    move,
					Borrow:  borrow,
				})
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(os.Stdout, map[string]any{
						"name":                name,
						"session_id":          resp.SessionId,
						"previous_session_id": resp.PreviousSessionId,
					})
				}
				// What it moved from, when it moved: a caller passing --move is doing something to a
				// name that was already in use, and saying only where it landed hides half of that.
				if resp.PreviousSessionId != "" && resp.PreviousSessionId != resp.SessionId {
					_, err = fmt.Printf("%s now names %s, moved from %s\n",
						name, paths.FormatSessionID(resp.SessionId),
						paths.FormatSessionID(resp.PreviousSessionId))
					return err
				}
				_, err = fmt.Printf("%s now names %s\n", name, paths.FormatSessionID(resp.SessionId))
				return err
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&move, "move", false, "allow repointing a name that already names a session")
	f.BoolVar(&borrow, "borrow", false,
		"killing by this name releases it and leaves the session running")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

func newUnbindCommand(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "unbind <name>",
		Short: "Remove a name, leaving its session running",
		Long: `Remove a name. The session it named keeps running and is still reachable by its
ID and by any other name bound to it.

Removing a name nothing uses is not an error: the name is gone either way, so this is
safe to call without checking first.

A session whose last name is removed is not orphaned. Every session is reachable by
its ID, which is what 'cm list' shows, and that is what makes removing a name safe:
there is no way to make a shell unreachable by taking a name away from it.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeBoundNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.Unbind(ctx, &serverv1.UnbindRequest{Name: name})
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(os.Stdout, map[string]any{
						"name":       name,
						"removed":    resp.Removed,
						"session_id": resp.SessionId,
					})
				}
				if !resp.Removed {
					// Said plainly rather than silently succeeding, so a typo is visible. Still exit 0:
					// the name is not bound, which is what was asked for.
					_, err = fmt.Printf("%s named nothing\n", name)
					return err
				}
				_, err = fmt.Printf("%s no longer names %s, which is still running\n",
					name, paths.FormatSessionID(resp.SessionId))
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// splitBindArgs resolves what to bind and what to bind it to.
//
// The session defaults to the calling one, from CM_SESSION, which is what makes `cm bind refactor` the
// whole gesture for naming the window you are in. Same fallback `cm tag`, `cm report` and `cm detach`
// use, and it retargets nothing: a bind moves a name, never a client.
//
// The name is checked before the target is resolved, so what is wrong with the name is what gets
// reported. The other order answers `cm bind @a7k2m9x4` outside a session with "no session given", which
// names the argument that was omitted rather than the one that cannot mean what it says.
func splitBindArgs(args []string) (name, session string, err error) {
	name = args[0]
	// An ID cannot be bound, since an ID is what names point at. Refused here so a typo costs no server
	// round trip, and because the error can name the rule.
	if _, isID := paths.SessionRef(name); isID {
		return "", "", fmt.Errorf(
			"%q is an ID reference, which cannot be bound: bind a name to it instead", name)
	}
	if err := paths.ValidateSessionName(name); err != nil {
		return "", "", err
	}
	session, err = sessionTarget(args[1:], "bind a name to")
	if err != nil {
		return "", "", err
	}
	return name, session, nil
}

// completeBindArgs completes the session in the second position and offers nothing for the first.
//
// Nothing for the name, deliberately: the whole point of the first argument is that it does not exist
// yet, so completing it from existing names would only ever suggest values that need --move.
func completeBindArgs(g *globals) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeSessionNames(g)(cmd, args, toComplete)
	}
}

// completeBoundNames offers names that exist, which is what unbind takes.
func completeBoundNames(g *globals) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		dirs, err := g.dirs()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		conn, cl, err := dialServer(dirs)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		defer conn.Close()

		resp, err := cl.List(cmd.Context(), &serverv1.ListRequest{})
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		sortSessions(resp.Sessions)
		var out []string
		for _, s := range resp.Sessions {
			// Names only, and every one of them: unbind takes a name, and an ID reference is not
			// something it can remove.
			for _, name := range s.Names {
				out = append(out, name+"\t"+"names "+paths.FormatSessionID(s.Id))
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}
