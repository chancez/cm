package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// errNoActiveClient is returned when no client of the session can be named as the active one.
//
// Its own error rather than an empty report, because every caller wants to distinguish it. A keybinding
// acting on "my window" has nothing to act on, and a script reading --field would otherwise get a blank
// line it could mistake for a legitimate empty value.
var errNoActiveClient = errors.New(
	"no active client: nothing has been typed in this session since a client attached")

func newClientsCurrentCommand(g *globals) *cobra.Command {
	var (
		field  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "current [session]",
		Short: "Print the client someone is using",
		Long: `Print the one client someone is using, rather than every client attached.

With no session, the one this command is running in is used, from CM_SESSION, so a
keybinding or a shell function needs no plumbing.

  cm clients current              # the session I am in
  cm clients current work         # a specific one
  cm clients current --field pid  # one value, for a script

The active client is the one that typed most recently. That is not a guess standing
in for something better: it is the only signal that can identify one client out of
several, and it is causally right for a command typed at a prompt, since the
keystrokes that ran it arrived on exactly that client's connection.

Nothing else works, which is worth knowing before reaching for it. A session's pty
fans out to every attached client, so an escape sequence asking "which client are
you" is broadcast to all of them and answered by whichever replies first. A command
running inside the session cannot see its own client either, because its stdout is
the session's pty rather than any one terminal, and the client is not among its
parent processes. Focus would need cm to enable focus reporting on a terminal that
never asked for it, and reports nothing while a window sits at a shell prompt.

So this fails rather than guessing when nothing has been typed yet, which is the
state a freshly attached session is in. A read-only follower is never the answer:
its input is dropped, so it never types.`,
		// At most one name. Unlike the rest of the clients commands this has a single subject, so there is
		// no --tag: "the active client of each of these sessions" is a listing, which `cm clients list`
		// already is.
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := sessionTarget(args, "report the client of")
			if err != nil {
				return err
			}
			if err := paths.ValidateSessionRef(name); err != nil {
				return err
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Deliberately not ensureServer, matching the rest of this group: a server that was just
			// started has never heard of the session and would report it as having nothing attached,
			// which reads as an answer rather than as the absence of one.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.List(ctx, &serverv1.ListRequest{})
				if err != nil {
					return err
				}
				var serverVersion string
				if st, err := cl.Status(ctx, &serverv1.StatusRequest{}); err == nil {
					serverVersion = st.Version
				}

				// The whole session list is filtered client-side for the same reason `cm clients list`
				// does it: the List RPC takes a prefix rather than a name, and a prefix would also match
				// "work2" when asked about "work".
				var found *serverv1.Session
				for _, s := range resp.Sessions {
					if s.Name == name {
						found = s
						break
					}
				}
				if found == nil {
					return fmt.Errorf("session %q not found", name)
				}

				rows := clientRows([]*serverv1.Session{found}, nil, serverVersion, false)
				var active *clientRowJSON
				for i := range rows {
					if rows[i].Active {
						active = &rows[i]
						break
					}
				}
				if active == nil {
					return errNoActiveClient
				}

				if asJSON {
					return writeJSON(os.Stdout, *active)
				}
				return printClientDetail(os.Stdout, *active, field)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&field, "field", "",
		"print only this field: "+strings.Join(ClientFieldNames(), ", "))
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

// clientFields renders one client's values in display order.
//
// Built from clientRowJSON rather than from the wire message, so the field names here and the JSON keys
// come from one place and a rename cannot leave them disagreeing.
func clientFields(r clientRowJSON) []struct {
	name  string
	value string
} {
	kind := "terminal"
	if r.ReadOnly {
		kind = "follower"
	}
	build := r.Version
	if build == "" {
		build = "unknown"
	}
	lastInput := r.LastInputAt
	if lastInput == "" {
		// Not reachable through `cm clients current`, since a client with no input time is never the
		// active one, but this renders any row and a bare empty value would read as a formatting bug.
		lastInput = "never"
	}
	attached := r.AttachedAt
	if attached == "" {
		attached = "unknown"
	}
	return []struct {
		name  string
		value string
	}{
		{"session", r.Session},
		{"pid", fmt.Sprint(r.PID)},
		{"kind", kind},
		// The bare version, without the "(stale)" the table appends. A --field is read by a script, and
		// stale is its own field for exactly that reason.
		{"build", build},
		{"stale", fmt.Sprint(r.Stale)},
		{"attached_at", attached},
		{"last_input_at", lastInput},
	}
}

// ClientFieldNames lists the fields `cm clients current --field` accepts, for the flag's help.
func ClientFieldNames() []string {
	fields := clientFields(clientRowJSON{})
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.name)
	}
	return names
}

// printClientDetail prints one client as a field-per-line report, or one bare value with a field named.
//
// Vertical rather than the one-row table `cm clients list` prints, matching `cm info`: a single subject
// with seven values reads better down the page than across it, and it does not have to be re-aligned when
// a value is long.
func printClientDetail(w io.Writer, r clientRowJSON, field string) error {
	fields := clientFields(r)

	if field != "" {
		for _, f := range fields {
			if f.name == field {
				_, err := fmt.Fprintln(w, f.value)
				return err
			}
		}
		return fmt.Errorf("unknown field %q, want one of %v", field, ClientFieldNames())
	}

	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, f := range fields {
		fmt.Fprintf(tw, "%s\t%s\n", f.name, f.value)
	}
	return tw.Flush()
}
