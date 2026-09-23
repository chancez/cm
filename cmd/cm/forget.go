package main

import (
	"context"
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newForgetCommand(g *globals) *cobra.Command {
	var (
		ended  bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "forget <session>...",
		Short: "Remove what cm remembers about sessions that have ended",
		Long: `Remove what cm remembers about a session that has ended.

Its record, the names pointing at it and any output it had saved all go, so it
leaves the session list and stops holding disk. This is what a session's records
do on their own eventually, on the schedule expire_after and
forget_unpersisted_after set; this is how to do it now.

A session that is still running is refused, and there is no override:

  cm forget work
  cm: work: work is still running; use 'cm kill' to stop it first

That refusal is why this is a separate command rather than a flag on kill. Kill
is asked to end a shell, so "it is still running" cannot be a reason to decline,
and killing forgets the session as part of ending it. Asking to clear an exited
row meant sending a request to kill something already dead. For a shim that
cannot be reached at all, 'cm kill --force' is the escape hatch: it says what it
does, which is to stop trying and accept the orphaned process.

--ended forgets every session that has ended, which is the bulk case and the
reason this exists at all:

  cm forget --ended

Spelled differently from 'kill --all' because it means something narrower, every
*ended* session rather than every session. A running session is not touched or
reported, so this is safe to run against a server holding live work.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if ended {
				if len(args) > 0 {
					return errors.New("--ended takes no session names")
				}
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		ValidArgsFunction: completeEndedSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, name := range args {
				if err := paths.ValidateSessionRef(name); err != nil {
					return err
				}
			}
			// Deliberately not ensureServer, matching detach: a server started here has never heard of the
			// session, and would report a record it cannot see as already gone.
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.Forget(ctx, &serverv1.ForgetRequest{
					Sessions: args,
					Ended:    ended,
				})
				if err != nil {
					return err
				}
				return reportForget(os.Stdout, resp, asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&ended, "ended", false, "forget every session that has ended")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}
