package main

import (
	"context"
	"errors"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newRestoreWithCommand(g *globals) *cobra.Command {
	var clear bool
	cmd := &cobra.Command{
		Use:   "restore-with [session] -- command [args...]",
		Short: "Declare the command a session should be restored with",
		Long: `Declare the command a session should be restored with after its processes are
gone, for a program whose useful state is not in the command line it was started
with: claude restarted as "claude" is a new conversation, and
"claude --resume <id>" is the old one.

With no session, the one this command is running in is used, from CM_SESSION.

The declaration lasts as long as the program that made it. It binds to the
command the shell is running when it arrives, and ends when the shell is next at
its prompt, so a program that was quit is not brought back. --clear withdraws it
sooner, which a wrapper for something that must never be re-run can use.

Whether a restore runs the command, puts it on the prompt unrun, or ignores it is
on_restore's decision, never the program's. See docs/restore-command.md.

For Claude Code, from a SessionStart hook, which also runs after /clear with the
new conversation's id:

  cm restore-with -- claude --resume "$(jq -r .session_id)"`,
		Args:              cobra.ArbitraryArgs,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			argv := argsAfterDash(cmd, args)
			before := args
			if n := cmd.ArgsLenAtDash(); n >= 0 {
				before = args[:n]
			}
			if len(before) > 1 {
				return errors.New("at most one session, and the command after --")
			}
			if clear && len(argv) > 0 {
				return errors.New("--clear takes no command")
			}
			if !clear && len(argv) == 0 {
				return errors.New("a command is required after --")
			}

			name, err := sessionTarget(before, "declare a restore command for")
			if err != nil {
				return err
			}
			if err := paths.ValidateSessionRef(name); err != nil {
				return err
			}
			// Not ensureServer, for the reason cm report gives: declaring about a session implies a server
			// that knows it.
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				_, err := cl.SetResume(ctx, &serverv1.SetResumeRequest{Session: name, Argv: argv, Clear: clear})
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&clear, "clear", false, "withdraw the declaration")
	return cmd
}
