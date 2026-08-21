package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newKillCommand(g *globals) *cobra.Command {
	var (
		force   bool
		asJSON  bool
		all     bool
		tagArgs []string
		sigSpec string
	)
	cmd := &cobra.Command{
		Use:   "kill <session>...",
		Short: "Terminate sessions and their shells",
		Long: `Terminate sessions and their shells.

The session is ended with SIGHUP by default, sent to its foreground process group
so a running job goes too. That is a request rather than a guarantee: a job that
ignores SIGHUP survives it, and this then reports the session killed while a
process keeps holding its pty.

--signal names a different one when that happens, or when a program needs a
particular signal to shut down cleanly:

  cm kill build --signal term
  cm kill build --signal kill    # what --force sends
  cm kill build --signal 9

--force means be maximally forceful, which is two things: end the session with
SIGKILL, and forget it even if its shim cannot be reached. The second is why it is
not the default -- an unreachable shim may be busy rather than dead, and discarding
the record would orphan it and its pty permanently. Reach for --signal when the
goal is only a stronger signal.

--all kills every session the server knows, which is what a test harness or a
teardown script wants: killing by name means enumerating them first, and a
missed one leaves a shell and its pty behind.

--tag kills the sessions carrying a tag, which is the safe form of --all for
anything that created its own sessions:

  cm kill --tag run=abc123

It names exactly what the selector matches, so a script tearing down its own
fan-out cannot reach sessions somebody else is using, which --all would. A
selector matching nothing is an error rather than a silent success.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if all || len(tagArgs) > 0 {
				if len(args) > 0 {
					return errors.New("--all and --tag take no session names")
				}
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			if all && len(tagArgs) > 0 {
				// Refused rather than resolved. --all means every session and --tag means a subset, so
				// one of the two was a mistake and guessing which would kill either too much or too
				// little.
				return errors.New("--all and --tag cannot be combined; --all is already every session")
			}
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}
			var sig int32
			if sigSpec != "" {
				var err error
				if sig, err = parseSignal(sigSpec); err != nil {
					return err
				}
			}
			for _, name := range args {
				if err := paths.ValidateSessionName(name); err != nil {
					return err
				}
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				names := args
				switch {
				case all:
					// Enumerated here rather than server-side, so --all is exactly "kill these names" and
					// the server keeps one meaning for a kill request. Nothing is killed if the list is
					// empty, which is the right answer for a server with no sessions.
					listed, err := cl.List(ctx, &serverv1.ListRequest{})
					if err != nil {
						return err
					}
					names = make([]string, 0, len(listed.Sessions))
					for _, s := range listed.Sessions {
						names = append(names, s.Name)
					}
					if len(names) == 0 {
						return nil
					}
				case len(tagArgs) > 0:
					// Unlike --all, a selector matching nothing is an error. --all on an empty server is a
					// satisfied request, while a selector that matched nothing is usually a typo, and
					// exiting 0 there would let a teardown script report success having killed nothing.
					names, err = resolveSelector(ctx, cl, tagArgs)
					if err != nil {
						return err
					}
					if len(names) == 0 {
						return fmt.Errorf("no sessions match %s", describeSelectors(tagArgs))
					}
				}
				resp, err := cl.Kill(ctx, &serverv1.KillRequest{
					Sessions: names,
					Force:    force,
					Signal:   sig,
				})
				if err != nil {
					return err
				}
				return reportKill(os.Stdout, resp, asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&force, "force", false,
		"end the session with SIGKILL, and forget it even if its shim cannot be reached")
	f.StringVar(&sigSpec, "signal", "",
		"signal to end the session with, by name or number (default hup, or kill with --force)")
	f.BoolVar(&all, "all", false, "kill every session")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"kill the sessions with this tag, as key or key=value (repeatable, all must match)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}
