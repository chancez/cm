package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// defaultReadLines bounds a read that did not ask for a size.
//
// A default at all, because the point of this command is a bounded view: a caller that wanted everything
// has `--lines 0` or `cm history`. Large enough to hold a test failure with its stack, small enough to
// paste somewhere or hand to a model without thinking about it.
const defaultReadLines = 100

func newReadCommand(g *globals) *cobra.Command {
	var (
		lines   int
		wrap    bool
		follow  bool
		raw     bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:   "read [session]",
		Short: "Print a session's recent output",
		Long: `Print the tail of a session's output as plain text.

Made for reading a session programmatically, where 'cm history' is for reading all
of it and for the formats that only make sense over the whole thing. Use 'cm history
--format html' when styling matters, since that is the one view neither this command
nor --raw can produce. Two differences follow from that:

Only the last lines are printed, 100 by default. Use --lines 0 for everything.

Soft-wrapped lines are rejoined, so a path or a stack frame the terminal broke to
fit its width comes back as one line. Use --keep-wrap to see the lines as the
terminal laid them out.

Works after a command has finished, which is the usual case for 'cm run', as long
as the session saved its output.

--raw prints the bytes the program emitted rather than the text they rendered to,
still bounded by --lines. That is the difference from 'cm history --format vt', which
renders the whole scrollback and cannot be limited.

--follow prints the last lines and then keeps streaming, like 'tail -f'. Escape
sequences are stripped from both halves unless --raw is given.

The two halves still differ in kind. The lines already printed are a rendered screen
with wrapping rejoined, and what follows is filtered byte by byte, because a stream
cannot re-render a screen per byte. So a program that repaints in place, like a
progress bar, comes out as every frame in turn rather than overwritten.`,
		// At most one name, since --tag supplies the rest.
		Args:              sessionOrTagArg,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines < 0 {
				lines = 0
			}
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			if raw {
				warnIfTerminal(os.Stdout)
			}

			cfg, err := g.config()
			if err != nil {
				return err
			}
			logger, closeLog := newClientLogger(dirs, cfg)
			if closeLog != nil {
				defer closeLog.Close()
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				names, fromSelector, err := sessionTargets(ctx, cl, args, tagArgs)
				if err != nil {
					return err
				}
				if follow && len(names) > 1 {
					// Refused rather than interleaved. Following is an endless stream, so N of them would
					// produce lines from several sessions mixed together with no way to tell them apart,
					// and the per-session header this command uses cannot mark a stream that never ends.
					return fmt.Errorf(
						"--follow needs one session, but --tag matched %d; follow one by name", len(names))
				}

				for i, name := range names {
					// Headed whenever a selector chose the session, including a single match: the caller did
					// not know which session it would be, so the name is part of the answer. A named session
					// prints bare, so piping one session's output is unchanged.
					if fromSelector {
						if err := writeSessionHeader(os.Stdout, name, i == 0); err != nil {
							return err
						}
					}
					resp, err := cl.Read(ctx, &serverv1.ReadRequest{
						Session: name,
						Lines:   uint32(lines),
						Unwrap:  !wrap,
						Raw:     raw,
					})
					if err != nil {
						return err
					}
					if follow {
						// The tail first, then the stream. Both go to stdout, so the caller sees one
						// continuous piece of output rather than having to stitch two commands together.
						return printTailThenFollow(ctx, dirs, name, resp.Data, raw, logger)
					}
					if _, err := os.Stdout.Write(resp.Data); err != nil {
						return err
					}
					// A trailing newline when the render lacks one, so the shell prompt does not land on the
					// same line as the last output. The formatter trims trailing whitespace, so this is the
					// common case rather than a rarity.
					if n := len(resp.Data); n > 0 && resp.Data[n-1] != '\n' {
						if _, err := os.Stdout.Write([]byte{'\n'}); err != nil {
							return err
						}
					}
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&follow, "follow", "f", false,
		"keep streaming new output after the last lines")
	f.BoolVar(&raw, "raw", false,
		"print the bytes the program emitted rather than the text they rendered to")
	f.IntVar(&lines, "lines", defaultReadLines, "how many lines from the end (0 for everything)")
	f.BoolVar(&wrap, "keep-wrap", false,
		"keep soft-wrapped lines split as the terminal laid them out")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"read the sessions with this tag, as key or key=value (repeatable, all must match)")
	return cmd
}
