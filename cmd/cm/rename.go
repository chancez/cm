package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newRenameCommand(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "rename <name> [session]",
		Short: "Give a session a new name in place of its old one",
		Long: `Point a name at a session and take off the name it had.

  cm rename refactor            # rename the session this is running in
  cm rename review work         # rename the session called work

A bind and an unbind, as 'cm bind' and 'cm unbind' would do them, and the old
name is removed only when the session had exactly one: several names are
deliberate, and choosing which to drop would be a guess. A session with
several keeps them all and gains this one.

With no session, the one this command is running in is used, from CM_SESSION.

Renaming a session a terminal emulator named, kitty.325 and the like, takes
away the name that emulator asks for again when it restarts, so it makes a new
session under the old name. 'cm bind' adds a name without taking one away.`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeBindArgs(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, session, err := splitBindArgs(args, "rename")
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				r, err := renameSession(ctx, cl, name, session)
				if err != nil {
					return err
				}
				return r.write(os.Stdout, asJSON)
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// renamer is the part of the server a rename uses, so a test can drive it without one.
type renamer interface {
	List(context.Context, *serverv1.ListRequest) (*serverv1.ListResponse, error)
	Bind(context.Context, *serverv1.BindRequest) (*serverv1.BindResponse, error)
	Unbind(context.Context, *serverv1.UnbindRequest) (*serverv1.UnbindResponse, error)
}

// renamed is what a rename did.
type renamed struct {
	// label is what the session was called before, for the message.
	label     string
	name      string
	sessionID string
	// dropped is the name taken off the session, or empty when it kept every name it had.
	dropped string
}

// renameSession binds name to the session ref names and unbinds its old name when it had exactly one.
//
// The rule internal/tui's rename uses, which that package states at length: Names rather than Name, since
// Name holds an ID reference for a session nothing names. Bound by ID once found, so the unbind and the
// bind cannot land on two sessions when a name moves between the List and the Bind.
func renameSession(ctx context.Context, cl renamer, name, ref string) (renamed, error) {
	resp, err := cl.List(ctx, &serverv1.ListRequest{})
	if err != nil {
		return renamed{}, err
	}
	s := sessionNamed(resp.Sessions, ref)
	if s == nil {
		return renamed{}, fmt.Errorf("no session %s", ref)
	}
	r := renamed{label: s.Name, name: name, sessionID: s.Id}
	if _, err := cl.Bind(ctx, &serverv1.BindRequest{Name: name, Session: paths.FormatSessionID(s.Id)}); err != nil {
		return renamed{}, err
	}
	if len(s.Names) != 1 || s.Names[0] == name {
		return r, nil
	}
	if _, err := cl.Unbind(ctx, &serverv1.UnbindRequest{Name: s.Names[0]}); err != nil {
		// Two names rather than a failed rename, so said as that: the new name works and so does the old.
		return renamed{}, fmt.Errorf("%s is now %s, but %s still names it too: %w", r.label, name, s.Names[0], err)
	}
	r.dropped = s.Names[0]
	return r, nil
}

// write reports a rename in the wording internal/tui's status line uses.
func (r renamed) write(w io.Writer, asJSON bool) error {
	if asJSON {
		return writeJSON(w, map[string]any{
			"name":       r.name,
			"session_id": r.sessionID,
			"dropped":    r.dropped,
		})
	}
	msg := fmt.Sprintf("%s is now %s", r.label, r.name)
	if r.dropped != "" {
		msg += ", and no longer " + r.dropped
	}
	_, err := fmt.Fprintln(w, msg)
	return err
}
