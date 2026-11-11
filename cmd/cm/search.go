package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/tags"
	"github.com/chancez/cm/internal/tui"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newSearchCommand(g *globals) *cobra.Command {
	var (
		asJSON  bool
		openTUI bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:   "search <query>...",
		Short: "Fuzzy-find sessions, as the picker's / filter does",
		Long: `Print the sessions matching a fuzzy query, best match first.

The match is the picker's own "/" filter, over the same fields: every name, the
ID, the running command, the title, the directory, the reported detail, and the
tags as key=value. Several words are joined with spaces into one query.

--tui opens the picker with the query already typed, which shows the same
sessions in the same order. With no match, it still opens, so the query can be
edited; without --tui, no match exits 1 the way grep does.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := tags.ParseSelector(tagArgs); err != nil {
				return err
			}
			query := strings.Join(args, " ")
			if openTUI {
				picker, err := filteredTUICommand(g, query, tagArgs)
				if err != nil {
					return err
				}
				picker.SetContext(cmd.Context())
				return picker.RunE(picker, nil)
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.List(ctx, &serverv1.ListRequest{Tags: tagArgs})
				if err != nil {
					return err
				}
				matched := tui.Match(query, resp.Sessions)
				if asJSON {
					err = writeSessionsJSON(os.Stdout, matched)
				} else {
					err = writeSessionsTable(os.Stdout, matched, terminalWidth(os.Stdout))
				}
				if err != nil {
					return err
				}
				if len(matched) == 0 {
					// Said on stderr and exited 1, so `cm search x && ...` and an empty table cannot be confused.
					if !asJSON {
						fmt.Fprintf(os.Stderr, "no sessions match %q\n", query)
					}
					return &exitCodeError{code: 1, reported: true}
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&openTUI, "tui", false, "open the picker filtered on the query instead of printing")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"only sessions with this tag, as key or key=value (repeatable, all must match)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	cmd.MarkFlagsMutuallyExclusive("tui", "json")
	return cmd
}

// filteredTUICommand is `cm tui --filter query`, with the search's tags, ready to run in this process.
//
// The picker's own command rather than a second call into internal/tui, so the terminal check, the server
// connection, the keys and the attach child are the ones `cm tui` has. A re-exec would do the same at the
// cost of a process and of rebuilding this invocation's global flags into an argv.
func filteredTUICommand(g *globals, query string, tagArgs []string) (*cobra.Command, error) {
	picker := newTUICommand(g)
	if err := picker.Flags().Set("filter", query); err != nil {
		return nil, err
	}
	for _, t := range tagArgs {
		if err := picker.Flags().Set("tag", t); err != nil {
			return nil, err
		}
	}
	return picker, nil
}
