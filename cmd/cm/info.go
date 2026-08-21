package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newInfoCommand(g *globals) *cobra.Command {
	var (
		field   string
		asJSON  bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:   "info [session]",
		Short: "Print one session's details",
		Long: `Print details for a single session.

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
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				names, fromSelector, err := sessionTargets(ctx, cl, args, tagArgs)
				if err != nil {
					return err
				}
				resp, err := cl.List(ctx, &serverv1.ListRequest{})
				if err != nil {
					return err
				}
				// Indexed rather than scanned per name, so N sessions cost one pass instead of N.
				byName := make(map[string]*serverv1.Session, len(resp.Sessions))
				for _, s := range resp.Sessions {
					byName[s.Name] = s
				}

				found := make([]*serverv1.Session, 0, len(names))
				for _, name := range names {
					s, ok := byName[name]
					if !ok {
						// Only reachable for a named session: a selector's names came from this same
						// server, though a session could still end in between.
						return fmt.Errorf("session %q not found", name)
					}
					found = append(found, s)
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
					if err := printSessionInfo(os.Stdout, s, field); err != nil {
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
	return cmd
}
