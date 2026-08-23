package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newSwitchCommand(g *globals) *cobra.Command {
	return newMoveCommand(g, false)
}

func newRebindCommand(g *globals) *cobra.Command {
	return newMoveCommand(g, true)
}

// newMoveCommand builds `cm switch` or `cm rebind`, which differ in what they move.
//
// Two verbs rather than one with a flag, because they act on different layers and a flag made the more
// common one the qualified one. A switch moves this *client*: the name stays where it is, so a restarted
// terminal comes back to the session it always named. A rebind moves the *name*, and the client follows,
// so everything afterwards resolves to the target.
//
// Which also puts the awkward case where it belongs. A session with no name cannot be rebound, since there
// is nothing to move, and that is every `cm run -d` and every `cm attach` with no argument. As a flag on
// switch, the failure landed on whichever form was the default; as its own verb it is a property of
// `cm rebind` alone, and `cm switch` always works.
func newMoveCommand(g *globals, bind bool) *cobra.Command {
	var (
		from       string
		allClients bool
		asJSON     bool
	)

	use, short, long := switchHelp()
	if bind {
		use, short, long = rebindHelp()
	}

	cmd := &cobra.Command{
		Use:               use,
		Short:             short,
		Long:              long,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			session, err := sessionTarget(nil, "switch")
			if err != nil {
				return err
			}
			if from != "" {
				session = from
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Deliberately not ensureServer: switching implies a window is attached to something, and a
			// server started here holds nothing to switch.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.Switch(ctx, &serverv1.SwitchRequest{
					Session:    session,
					Target:     target,
					Bind:       bind,
					AllClients: allClients,
				})
				if err != nil {
					return err
				}
				if asJSON {
					return writeJSON(os.Stdout, map[string]any{
						"session":     session,
						"target":      target,
						"target_id":   resp.TargetId,
						"switched_to": resp.SwitchedTo,
						"bound_name":  resp.BoundName,
						"asked":       resp.Asked,
					})
				}
				target := paths.FormatSessionID(resp.TargetId)
				// A rebind reports the name first, because the name moving is the durable half and the
				// window following is a consequence. A switch has only the window to report.
				if resp.BoundName != "" {
					if resp.Asked == 0 {
						// The name moved even though no window did, which is a coherent outcome worth
						// stating rather than hiding: the next attach by that name lands on the target.
						_, err = fmt.Printf("%s now names %s, and no window was using it\n",
							resp.BoundName, target)
						return err
					}
					_, err = fmt.Printf("%s now names %s, and moved %s\n",
						resp.BoundName, target, plural(int(resp.Asked), "window"))
					return err
				}
				if resp.Asked == 0 {
					// Its own message rather than a bare zero, because the cause is specific and fixable:
					// the active client is the one that typed most recently, and a session nothing has been
					// typed in cannot name one. Not an error, since nothing went wrong and nothing moved.
					_, err = fmt.Fprintf(os.Stderr,
						"%s: no window is using %s, so nothing moved\n", paths.Name, session)
					return err
				}
				_, err = fmt.Printf("moved %s to %s\n", plural(int(resp.Asked), "window"), target)
				return err
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "",
		"the session to act on (default the one this runs in)")
	_ = cmd.RegisterFlagCompletionFunc("from", completeSessionNames(g))
	f.BoolVar(&allClients, "all-clients", false,
		"move every window showing this session, not only the one in use")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// switchHelp is the client-level form.
func switchHelp() (use, short, long string) {
	return "switch <session>",
		"Show a different session in this window, until this client goes away",
		`Point this window at another session, without closing it and without moving
anything else.

  cm switch work

Only this client moves. The name this window was started with keeps pointing where it
did, so a restarted terminal comes back to the session it always named, and the way
back is 'cm switch' to that name. 'cm rebind' is the other one: it moves the name too,
which is what you want when a window should stay on the session for good.

The client detaches and reattaches without leaving its own process, so the window never
resets: the same terminal, the same process id, the same command in 'ps'. Only the
session behind it changes, which is the same thing that happens whenever a client
reconnects after a server restart.

Which window moves is the one that typed the command. A session's output fans out to
every client attached to it, so there is no way to ask a terminal "are you the one",
and the keystrokes that ran this command arrived on exactly one client's connection,
which makes that the right answer rather than a guess. --all-clients moves every window
showing this session instead.

The session left behind keeps running, and is reachable by its ID and by every name
bound to it. Nothing has to be killed to switch away from it.

With no --from, the session this command is running in is used, from CM_SESSION, so a
keybinding needs no plumbing.`
}

// rebindHelp is the binding-level form.
func rebindHelp() (use, short, long string) {
	return "rebind <session>",
		"Point this window's name at another session, and follow it there",
		`Move this window's name to another session, and bring this window along.

  cm rebind work

This is the one that lasts. A terminal emulator fixes a window's launch command when
the window is created and saves it verbatim, so on restore the window re-runs that
command; nothing outside cm can update it. Moving the name in it is what makes the
restored window land where you put it rather than back on the session it first made.

'cm switch' is the other one: it moves this client only and leaves the name alone, so a
restart returns to the original session.

Everything that names this session by the moved name follows it, which is the point: a
window-close watcher that kills by that name, and a listing that shows it, both now
describe the session you are looking at.

The moved name is marked borrowed, so killing by it releases the name and leaves the
session running rather than ending a shell that lives elsewhere. That is what closing
this window should do to a session it borrowed.

A session with no name cannot be rebound, since there is nothing to move, and this says
so rather than quietly switching for this client alone. 'cm bind' gives it a name
first, or 'cm switch' moves the client without one.

With no --from, the session this command is running in is used, from CM_SESSION.`
}
