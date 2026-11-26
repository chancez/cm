package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/remote"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// infoHost is the host cm info names for sessions it found on another one, remoteArg as --remote was set by
// a qualifier or a followed pointer: a name in [remote.hosts], as a listing of every host names one. Empty
// for this machine, and for a remote given as an address, which is no name to reach it by again.
func infoHost(remoteArg string) string {
	if remoteArg == "" || remote.ValidateName(remoteArg) != nil {
		return ""
	}
	return remoteArg
}

func newInfoCommand(g *globals) *cobra.Command {
	var (
		field     string
		asJSON    bool
		omitEmpty bool
		tagArgs   []string
	)
	cmd := &cobra.Command{
		Use:   "info [session]",
		Short: "Print one session's details",
		Long: `Print details for a single session, the one this command runs in when none
is named.

--field prints one value alone, which is what a script wants: a terminal emulator
opening a new window in a session's directory needs the path with no header,
padding, or parsing.

cwd is empty when the session has reported a directory on another host, since
acting on a remote path locally would be wrong.

--tag prints every session carrying a tag. With --field the values print one per
line, so a selector plus a field is a list of that field across the group:

  cm info --tag run=abc123 --field cwd`,
		// At most one name, since --tag supplies the rest.
		Args:              sessionOrTagArg,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}
			// The calling session when nothing is named, as bind, tag and report default: `cm info` inside a
			// session describes that one, which is what the overlay's i runs. Here rather than in
			// sessionTargets, whose other callers include wait and read, for which a session waiting on or
			// following itself is not a default anybody wants.
			if len(args) == 0 && len(tagArgs) == 0 {
				if self := os.Getenv(paths.SessionEnv()); self != "" {
					args = []string{self}
				}
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				names, fromSelector, err := sessionTargets(ctx, cl, args, tagArgs)
				if err != nil {
					return err
				}
				resp, err := cl.List(ctx, &serverv1.ListRequest{})
				if err != nil {
					return err
				}
				// Indexed rather than scanned per reference, so N sessions cost one pass instead of N.
				//
				// Every way a session can be referred to goes in: each of its names, and its ID with the
				// sigil. Only the displayed name was indexed before, so a session was findable by
				// whichever name a listing happened to show and by nothing else, which made `cm info
				// @<id>` report a session that was right there as not found.
				byRef := make(map[string]*serverv1.Session, len(resp.Sessions))
				for _, s := range resp.Sessions {
					byRef[s.Name] = s
					byRef[paths.FormatSessionID(s.Id)] = s
					for _, name := range s.Names {
						byRef[name] = s
					}
				}

				found := make([]*serverv1.Session, 0, len(names))
				for _, name := range names {
					s, ok := byRef[name]
					if !ok {
						// Only reachable for a named session: a selector's names came from this same
						// server, though a session could still end in between.
						return fmt.Errorf("session %q not found", name)
					}
					found = append(found, s)
				}
				// The host a session is on when this asked another one, by a qualified reference or a name here
				// that points there: the far server answers as if the session were local to it, and a caller
				// opening a window beside this one, as a terminal's launch wrapper does, has to know where.
				if host := infoHost(g.remote); host != "" {
					for _, s := range found {
						s.Host = host
					}
				}

				if asJSON {
					// An array for a selector and a bare object for one named session, so an existing
					// `cm info NAME --json | jq .cwd` keeps working while a selector composes with `.[]`.
					if fromSelector {
						out := make([]sessionJSON, 0, len(found))
						for _, s := range found {
							out = append(out, toSessionJSON(s))
						}
						return writeJSON(os.Stdout, out)
					}
					return writeJSON(os.Stdout, toSessionJSON(found[0]))
				}

				for i, s := range found {
					// A field prints bare even from a selector, so `--tag ... --field cwd` is a list of
					// paths rather than a headed report a caller would have to strip.
					if fromSelector && field == "" {
						if err := writeSessionHeader(os.Stdout, s.Name, i == 0); err != nil {
							return err
						}
					}
					print := func() error { return printSessionInfo(os.Stdout, s, field) }
					if omitEmpty && field == "" {
						print = func() error { return printNonEmptySessionInfo(os.Stdout, s) }
					}
					if err := print(); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&field, "field", "",
		"print only this field: "+strings.Join(SessionFieldNames(), ", "))
	f.StringArrayVar(&tagArgs, "tag", nil,
		"print the sessions with this tag, as key or key=value (repeatable, all must match)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	// For the overlay, whose block is capped at 12 rows: half the 27 fields are usually empty. Hidden because
	// a script wants every field in a fixed place, which is what the table is for.
	f.BoolVar(&omitEmpty, "omit-empty", false, "leave out fields with no value")
	_ = f.MarkHidden("omit-empty")
	return cmd
}
