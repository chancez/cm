package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/tags"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newClientsListCommand(g *globals) *cobra.Command {
	var (
		asJSON  bool
		stale   bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:     "list [session]...",
		Aliases: []string{"ls"},
		Short:   "List attached clients, one row each",
		Long: `List the clients attached to sessions, one row per client.

'cm list' is session-oriented: one row per session, with a count of what is
attached. This is the other axis. A count answers "is anyone watching"; this
answers "what is watching, and what is it running", which is the question that
matters when builds differ.

  cm clients list                 # every client
  cm clients list work            # just this session's
  cm clients list --stale         # only clients not on the server's build

A '*' marks the client someone is using, which is the one that typed most recently.
Nothing is marked until something is typed, and only one client per session is ever
marked. 'cm clients current' prints that client alone, with the time it last typed.

Typing is the signal because nothing else can tell one client from another. A
session's pty fans out to every attached client, so a sequence asking "which client
are you" is answered by all of them, and a client cannot be identified from inside
the session at all: a command's stdout is the pty, not any one terminal.

Version differences are expected rather than broken. A session outlives its server
by design, so a client and server from different builds is a normal state and cm is
built for it. It is worth being able to see because the effect is silent: protobuf
reads a field a peer never sent as its zero value, so a feature one side does not
implement returns nothing rather than failing.

A client too old to report its build shows "unknown" rather than a guess, and
counts as stale, since a client that predates the field is by definition not
current.

Read-only clients are followers: something streaming the session rather than
painting a terminal. They never size the session and never answer a terminal
query.`,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parsed client-side too, so a typo reports the same way whether or not a server is running.
			if _, err := tags.ParseSelector(tagArgs); err != nil {
				return err
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Deliberately not ensureServer: listing clients of a server that is not running should say
			// there are none, not start one and then report that it has none.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.List(ctx, &serverv1.ListRequest{Tags: tagArgs})
				if err != nil {
					return err
				}
				// The server's own build, to mark each client current or stale. From Status rather than
				// this binary's version: the comparison that matters is client against the server it is
				// talking to, and this process may be a third build again.
				var serverVersion string
				if st, err := cl.Status(ctx, &serverv1.StatusRequest{}); err == nil {
					serverVersion = st.Version
				}

				rows := clientRows(resp.Sessions, args, serverVersion, stale)
				if asJSON {
					return writeJSON(os.Stdout, rows)
				}
				return printClientRows(os.Stdout, rows, serverVersion)
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&stale, "stale", false,
		"only clients whose build differs from the server's, including those that report none")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"only clients of sessions with this tag, as key or key=value (repeatable)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

// clientRowJSON is one attached client, flattened so the session is on the row.
//
// A flat shape rather than sessions containing clients, because the subject here is the client: a caller
// filtering for stale clients wants one record each, not to walk a nesting. `cm list --json` already
// provides the nested form for anything that wants it.
type clientRowJSON struct {
	// Session this client is attached to, repeated on each of its rows.
	Session string `json:"session"`
	// PID is the client process, 0 when it reported none. Meaningful only on this host.
	PID int32 `json:"pid"`
	// Version is the client's build, or empty when it reported none.
	Version string `json:"version"`
	// Stale reports that this client's build differs from the server's, or that it reported none.
	//
	// Precomputed rather than left to the caller, so a script does not have to fetch the server's version
	// separately and repeat the comparison, including the "unknown counts as stale" rule.
	Stale bool `json:"stale"`
	// ReadOnly reports a follower rather than a terminal.
	ReadOnly bool `json:"read_only"`
	// AttachedAt is RFC 3339, empty when unknown. AttachedAtUnix is the same instant, 0 when unknown.
	AttachedAt     string `json:"attached_at"`
	AttachedAtUnix int64  `json:"attached_at_unix"`
	// Active marks the client someone is using: the one that typed most recently. At most one row per
	// session has it, and no row does when nothing has typed yet.
	Active bool `json:"active"`
	// LastInputAt is when this client last sent typing, RFC 3339 and empty when it never has.
	// LastInputAtUnix is the same instant, 0 when never.
	//
	// Reported alongside Active because the mark alone cannot say how old it is, and a client that last
	// typed days ago is the active one only in the sense that nothing else has typed since.
	LastInputAt     string `json:"last_input_at"`
	LastInputAtUnix int64  `json:"last_input_at_unix"`
}

// clientRows flattens sessions into one row per attached client.
//
// names, when non-empty, keeps only those sessions. Filtered here rather than server-side because the List
// RPC takes a prefix rather than a set, and a client-side filter over a handful of sessions is cheaper than
// a new request shape.
//
// Rows come out in the order the server reported sessions, and within a session in the order its clients
// attached, so repeated calls agree and the output can be diffed. Not pid order, and not stable across an
// upgrade: a re-execed client attaches again and takes a new place in that order while keeping its pid,
// which is correct rather than surprising once you know the ordering is by attachment.
func clientRows(
	sessions []*serverv1.Session, names []string, serverVersion string, onlyStale bool,
) []clientRowJSON {
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}

	rows := make([]clientRowJSON, 0, len(sessions))
	for _, s := range sessions {
		if len(want) > 0 && !want[s.Name] {
			continue
		}
		for _, c := range s.AttachedClients {
			// Unknown counts as stale. The field exists because older clients did not send one, so a
			// client that reports nothing is more likely behind than current, and calling it current
			// would hide exactly the case this flag is for.
			isStale := c.Version != serverVersion
			if onlyStale && !isStale {
				continue
			}
			row := clientRowJSON{
				Session:         s.Name,
				PID:             c.Pid,
				Version:         c.Version,
				Stale:           isStale,
				ReadOnly:        c.ReadOnly,
				AttachedAtUnix:  c.AttachedAtUnix,
				Active:          c.Active,
				LastInputAtUnix: c.LastInputAtUnix,
			}
			// Formatted only for a real instant. Rendering zero would print 1970, which reads as a client
			// attached decades ago rather than one whose attach time is unknown.
			if c.AttachedAtUnix != 0 {
				row.AttachedAt = time.Unix(c.AttachedAtUnix, 0).Format(time.RFC3339)
			}
			if c.LastInputAtUnix != 0 {
				row.LastInputAt = time.Unix(c.LastInputAtUnix, 0).Format(time.RFC3339)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// printClientRows prints the table form.
//
// Says so explicitly when there is nothing, rather than printing a bare header. An empty listing here is
// ambiguous in a way an empty `cm list` is not: it can mean no sessions, or sessions with nothing attached,
// and with --stale it usually means the good outcome.
func printClientRows(w io.Writer, rows []clientRowJSON, serverVersion string) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no clients attached")
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// An unnamed first column for the active marker, the shape `git branch` and `tmux list-clients` use.
	// A header over it would have to be a word wider than the mark it labels, which pushes every row
	// right to caption one character; the legend under the table does that job instead.
	fmt.Fprintln(tw, "\tSESSION\tPID\tKIND\tBUILD\tATTACHED")
	for _, r := range rows {
		kind := "terminal"
		if r.ReadOnly {
			kind = "follower"
		}
		mark := ""
		if r.Active {
			mark = "*"
		}
		// The build column carries the comparison rather than only the value, since a bare hash means
		// nothing without the server's to compare against, and the whole point of this view is the
		// difference. The server's build is named under the table for the same reason.
		build := r.Version
		if build == "" {
			build = "unknown"
		}
		if r.Stale {
			build += " (stale)"
		}
		attached := r.AttachedAt
		if attached == "" {
			attached = "unknown"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", mark, r.Session, r.PID, kind, build, attached)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	stale := 0
	active := false
	for _, r := range rows {
		if r.Stale {
			stale++
		}
		if r.Active {
			active = true
		}
	}
	// Only when something is marked. Explaining a symbol that does not appear invites a hunt for it, and
	// with a single client attached the mark says nothing anyway.
	if active {
		fmt.Fprintln(w, "\n* the client last typed in")
	}
	if serverVersion != "" {
		fmt.Fprintf(w, "\nserver is %s", serverVersion)
		if stale > 0 {
			// The remedy is named because it is not obvious that this is fixable without losing anything,
			// which is the whole difference between a stale client and a stale shim.
			fmt.Fprintf(w, "; %d client(s) on another build, `cm clients upgrade --all` replaces them", stale)
		}
		fmt.Fprintln(w)
	}
	return nil
}
