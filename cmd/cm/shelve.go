package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newShelveCommand(g *globals) *cobra.Command {
	var (
		expire  string
		asJSON  bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:   "shelve <session>...",
		Short: "End sessions, keeping what is needed to restore them as they were",
		Long: `End a session's programs, keeping everything needed to bring it back as it was.

Its record, names, tags, saved output and the resume command a program declared
with 'cm restore-with' are all kept, and the session is listed as shelved:

  cm shelve work
  cm attach work           # restores it and attaches

This is not SIGTSTP, which is what "suspend" means in a shell (ctrl-z): nothing
is stopped to be continued. The shell and its programs are ended with SIGHUP,
waited for, and killed if they outlive the wait. Restoring starts a new shell
with the saved output replayed, and the declared resume command is handled by
on_restore as on any revive.

Only a session that saves its output can be shelved, since that is what a
restore comes back to: one started with --persist or matching persist.sessions. A
session that has already ended is marked shelved as it stands, and one that
already is shelved is reported and left alone.

A shelved session does not expire. --expire-after gives it a retention, a Go
duration counted from now, after which it is removed like any ended session.
'cm forget' and 'cm kill' remove one at once; 'cm forget --ended' leaves it.

--tag shelves the sessions carrying a tag, and a selector that matches
nothing is an error.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(tagArgs) > 0 {
				if len(args) > 0 {
					return errors.New("--tag takes no session names")
				}
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("expire-after") {
				if _, err := store.ParseExpiry(expire); err != nil {
					return fmt.Errorf("--expire-after: %w", err)
				}
			}
			if err := validateRefs(args, tagArgs); err != nil {
				return err
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireServerCapability(serverCapabilities(ctx, cl), capability.Shelve,
					"shelve a session"); err != nil {
					return err
				}
				names, err := selectedSessions(ctx, cl, args, tagArgs)
				if err != nil {
					return err
				}
				resp, err := cl.Shelve(ctx, &serverv1.ShelveRequest{Sessions: names, ExpireAfter: expire})
				if err != nil {
					return err
				}
				return reportOutcomes(os.Stdout, outcomes{
					done: resp.Shelved, doneKey: "shelved", doneWord: "shelved",
					noop: resp.AlreadyShelved, noopKey: "already_shelved", noopWord: "already shelved",
					errors: resp.Errors,
				}, asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&expire, "expire-after", "",
		"remove the shelved session after this long, a Go duration or never (default never)")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"shelve the sessions with this tag, as key or key=value (repeatable, all must match)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// validateRefs checks the session references and selectors a shelve was given, before anything reaches a
// server.
func validateRefs(args, tagArgs []string) error {
	if err := validateSelectors(tagArgs); err != nil {
		return err
	}
	for _, name := range args {
		if err := paths.ValidateSessionRef(name); err != nil {
			return err
		}
	}
	return nil
}

// selectedSessions resolves what a shelve acts on: the names given, or what a selector matches.
//
// A selector matching nothing is an error, as it is for kill, since it is usually a typo and exiting 0 would
// let a script report success having done nothing.
func selectedSessions(ctx context.Context, cl serverv1.ServerClient, args, tagArgs []string) ([]string, error) {
	if len(tagArgs) == 0 {
		return args, nil
	}
	names, err := resolveSelector(ctx, cl, tagArgs)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no sessions match %s", describeSelectors(tagArgs))
	}
	return names, nil
}

// outcomes is what a shelve reports: what it did, what needed nothing, and what failed, with the
// JSON key and the text wording for each of the first two.
type outcomes struct {
	done     []string
	doneKey  string
	doneWord string
	noop     []string
	noopKey  string
	noopWord string
	errors   map[string]string
}

// reportOutcomes prints what a shelve did, and fails if any session failed.
//
// Text is one line per session, the ones acted on first. JSON is an object with every list spelled out, empty
// rather than null, so a script can iterate each unconditionally, the shape kill and forget print.
func reportOutcomes(w io.Writer, o outcomes, asJSON bool) error {
	done, noop, errs := o.done, o.noop, o.errors
	if done == nil {
		done = []string{}
	}
	if noop == nil {
		noop = []string{}
	}
	if errs == nil {
		errs = map[string]string{}
	}
	if asJSON {
		if err := writeJSON(w, map[string]any{o.doneKey: done, o.noopKey: noop, "errors": errs}); err != nil {
			return err
		}
	} else {
		for _, name := range done {
			fmt.Fprintf(w, "%s %s\n", o.doneWord, name)
		}
		for _, name := range noop {
			fmt.Fprintf(w, "%s is %s\n", name, o.noopWord)
		}
	}
	return reportedErrors(errs, asJSON)
}
