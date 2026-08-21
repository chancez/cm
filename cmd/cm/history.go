package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newHistoryCommand(g *globals) *cobra.Command {
	var (
		format  string
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:   "history [session]",
		Short: "Print a session's contents, scrollback included",
		Long: `Print a session's contents, including scrollback.

Plain text by default, so it can be piped or paged. --format=vt keeps colors and
styling; --format=html produces styled markup.

The whole thing, always. 'cm read' is the bounded form: it takes --lines, rejoins
soft-wrapped lines, and with --raw gives the emitted bytes of just the tail, which
this cannot do. What only lives here is --format=html, since neither rendered text nor
raw bytes can carry styling as markup.

--tag prints every session carrying a tag, each under a header naming it. Not
available with --format=html, which produces a whole document per session that
cannot be concatenated into valid markup.`,
		// At most one name, since --tag supplies the rest.
		Args:              sessionOrTagArg,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			var f serverv1.HistoryFormat
			switch format {
			case "plain", "":
				f = serverv1.HistoryFormat_HISTORY_FORMAT_UNSPECIFIED
			case "vt":
				f = serverv1.HistoryFormat_HISTORY_FORMAT_VT
			case "html":
				f = serverv1.HistoryFormat_HISTORY_FORMAT_HTML
			default:
				return fmt.Errorf("unknown format %q, want plain, vt, or html", format)
			}
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
				// Refused rather than emitted, because the output would be broken in a way that is not
				// obvious: each session's HTML is a complete document with its own head and styles, so
				// several in a row is not a document at all. Checked against the count rather than the flag
				// so a selector matching exactly one still works.
				if f == serverv1.HistoryFormat_HISTORY_FORMAT_HTML && len(names) > 1 {
					return fmt.Errorf(
						"--format=html needs one session, but --tag matched %d; "+
							"each session's HTML is a whole document and they cannot be concatenated",
						len(names))
				}

				for i, name := range names {
					if fromSelector {
						if err := writeSessionHeader(os.Stdout, name, i == 0); err != nil {
							return err
						}
					}
					resp, err := cl.History(ctx, &serverv1.HistoryRequest{
						Session: name,
						Format:  f,
					})
					if err != nil {
						return err
					}
					if _, err := os.Stdout.Write(resp.Data); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&format, "format", "plain", "output format: plain, vt, or html")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"print the sessions with this tag, as key or key=value (repeatable, all must match)")
	return cmd
}
