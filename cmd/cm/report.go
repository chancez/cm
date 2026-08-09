package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// reportStates maps the flag values to their wire enum.
var reportStates = map[string]serverv1.ReportedState{
	"idle":    serverv1.ReportedState_REPORTED_STATE_IDLE,
	"busy":    serverv1.ReportedState_REPORTED_STATE_BUSY,
	"blocked": serverv1.ReportedState_REPORTED_STATE_BLOCKED,
	"clear":   serverv1.ReportedState_REPORTED_STATE_CLEAR,
}

func newReportCommand(g *globals) *cobra.Command {
	var (
		state  string
		detail string
		source string
	)
	cmd := &cobra.Command{
		Use:   "report [session]",
		Short: "Report what a program in a session is doing",
		Long: `Report what a program in a session is doing, so others can see and wait for it.

  busy     working on something
  blocked  needs input and will not progress without it
  idle     finished, waiting on nothing
  clear    remove the report, falling back to what cm sees itself

With no session, the one this command is running in is used, from CM_SESSION.

Nothing here is specific to any program. cm does not know or care what is
reporting: a build script, a test runner, and a coding agent's hook all use the
same call, and cm makes no attempt to detect what is running.

Blocked is the state cm cannot work out for itself, and the reason this exists.
A shell reports a command as running whether it is computing or sitting at a
prompt of its own, so only the program knows which. Once reported, it is visible
in 'cm list' and can be waited for:

  cm wait reviewer --until blocked

A report takes precedence over what cm derives from the shell, because a program
describing itself is better evidence than a shell marker. It lasts until changed
or cleared, and is not persisted: it describes a running program, so a stored
value would come back after a restart describing one that has since finished.

For a coding agent, wire this to whatever hook the agent already has for
"finished" and "needs input". Nothing needs to be added to cm per agent.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			want, ok := reportStates[state]
			if !ok {
				return fmt.Errorf(
					"unknown state %q, want one of busy, blocked, idle, clear", state)
			}

			name, err := reportTarget(args)
			if err != nil {
				return err
			}
			if err := paths.ValidateSessionName(name); err != nil {
				return err
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Deliberately not ensureServer: reporting about a session implies one exists, and starting a
			// server here would create one that has never heard of it.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				_, err := cl.Report(ctx, &serverv1.ReportRequest{
					Session: name,
					State:   want,
					Detail:  detail,
					Source:  source,
				})
				return err
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&state, "state", "", "busy, blocked, idle, or clear")
	f.StringVar(&detail, "detail", "", "a short note shown with the state")
	f.StringVar(&source, "source", "", "who is reporting, for display")
	// Required, because there is no sensible default: every value means something different and guessing
	// would report something the caller did not say.
	_ = cmd.MarkFlagRequired("state")
	return cmd
}

// reportTarget resolves which session a report is about.
func reportTarget(args []string) (string, error) {
	return sessionTarget(args, "report about")
}

// sessionTarget resolves an optional session argument, defaulting to the calling session.
//
// Falls back to CM_SESSION so a hook running inside a session needs no plumbing: the variable is already
// exported into every session's shell, and a hook that had to be told its own session name would need the
// caller to thread it through.
//
// This is the one place cm reads CM_SESSION, and it does not weaken the rule it looks like it breaks.
// That rule is about `attach`: zmx treats the variable as a request to *switch* the parent terminal's
// session, so attaching from inside one hijacks the window it was run from. Using it as the default
// target of a report or a tag does not move or retarget anything, and an explicit name always overrides
// it.
//
// verb names what the caller wanted to do, so the error says which command had nothing to act on.
func sessionTarget(args []string, verb string) (string, error) {
	if len(args) > 0 && args[0] != "" {
		return args[0], nil
	}
	if name := os.Getenv(paths.SessionEnv()); name != "" {
		return name, nil
	}
	return "", fmt.Errorf(
		"no session given and %s is not set, so there is no session to %s",
		paths.SessionEnv(), verb)
}
