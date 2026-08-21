package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/tags"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newListCommand(g *globals) *cobra.Command {
	var (
		prefix  string
		asJSON  bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List sessions",
		Long: `List sessions.

--tag filters by the labels set with 'cm tag' or --tag at creation. Repeating it
narrows rather than widens, so asking for two tags lists the sessions that have
both:

  cm ls --tag project=cm --tag role=reviewer

A bare key matches whatever its value is, so --tag project lists everything that
belongs to some project.

Tags group sessions that a name cannot. A per-window session is named by the
server, so it is called "s17" and --prefix has nothing to match on, and a session
belongs to several groupings at once while its name only says one thing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parsed client-side too, so a typo is reported the same way whether or not a server is
			// running, and the error names the character that was wrong.
			if _, err := tags.ParseSelector(tagArgs); err != nil {
				return err
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.List(ctx, &serverv1.ListRequest{Prefix: prefix, Tags: tagArgs})
				if err != nil {
					return err
				}
				if asJSON {
					return printSessionsJSON(os.Stdout, resp.Sessions)
				}
				return printSessionsTable(os.Stdout, resp.Sessions)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&prefix, "prefix", "", "only sessions whose name starts with this")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"only sessions with this tag, as key or key=value (repeatable, all must match)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}
