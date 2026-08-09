package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// waitStates maps the flag values to their wire enum.
//
// A table rather than a switch so the accepted values and the error message listing them cannot drift
// apart.
var waitStates = map[string]serverv1.WaitState{
	"idle":    serverv1.WaitState_WAIT_STATE_IDLE,
	"busy":    serverv1.WaitState_WAIT_STATE_BUSY,
	"exited":  serverv1.WaitState_WAIT_STATE_EXITED,
	"blocked": serverv1.WaitState_WAIT_STATE_BLOCKED,
}

// waitStateNames lists the states a wait accepts, in a stable order for help text and error messages.
//
// Sorted rather than in map order, so the help does not shuffle between builds.
func waitStateNames() []string {
	names := make([]string, 0, len(waitStates))
	for name := range waitStates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func newWaitCommand(g *globals) *cobra.Command {
	var (
		until   string
		timeout time.Duration
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "wait <session>",
		Short: "Wait until a session reaches a state",
		Long: `Wait until a session reaches a state.

  idle     the shell is at a prompt with nothing running
  busy     a command is running
  blocked  a program reported that it needs input
  exited   the session has ended

Idle and busy come from what the shell reports via OSC 133, so they need a shell
with terminal integration loaded. A session whose shell reports nothing never
becomes busy, and 'cm info <session> --field busy' shows what cm can see.

Blocked only exists when a program reports it, because it cannot be derived: a
shell marks a command as running whether it is computing or waiting at a prompt of
its own. Report it with 'cm report', or with the cm_report function that
'cm shell-init' provides for a shell. A report also takes precedence over the
derived state for idle and busy, since a program describing itself is better
evidence.

Exits 0 when the state is reached and 1 on timeout, so it composes with && and ||:

  cm send build 'make\n' && cm wait build --until idle && cm read build

Waiting is done by the server from the session's own output, so this costs one
request rather than polling, and cannot miss a transition the way sampling
'cm list' can.`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := paths.ValidateSessionName(name); err != nil {
				return err
			}
			state, ok := waitStates[until]
			if !ok {
				return fmt.Errorf("unknown state %q, want one of %s", until, strings.Join(waitStateNames(), ", "))
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// withServer starts a server if none is running, which is right rather than a shortcut: a
			// session outlives its server, so a wait issued while the server is down should adopt the
			// session and answer, not fail. That is the same path an upgrade takes.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				// No client-side deadline on the context. The server owns the timeout and returns a
				// result describing the session either way; cancelling the request instead would lose
				// that and report a context error rather than "not yet".
				resp, err := cl.Wait(ctx, &serverv1.WaitRequest{
					Session:   name,
					Until:     state,
					TimeoutMs: uint64(timeout.Milliseconds()),
				})
				if err != nil {
					return err
				}
				return reportWait(os.Stdout, name, until, resp, asJSON)
			})
		},
	}
	f := cmd.Flags()
	// Derived from the table rather than written out, which is the same reason the table exists. The
	// hand-written version omitted blocked while the command accepted it, so the one state cm cannot derive
	// for itself -- and the whole reason for the report mechanism -- was undiscoverable from the help.
	f.StringVar(&until, "until", "idle", "state to wait for: "+strings.Join(waitStateNames(), ", "))
	f.DurationVar(&timeout, "timeout", 0, "give up after this long (0 waits indefinitely)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// waitJSON is the JSON shape of a wait result.
type waitJSON struct {
	Session string `json:"session"`
	// Satisfied reports whether the state was reached, as opposed to the wait timing out.
	Satisfied bool `json:"satisfied"`
	// Until echoes what was waited for, so a log line makes sense on its own.
	Until string `json:"until"`
	// State, Busy, and Command describe the session when the wait returned. Present even on a timeout,
	// which is when knowing what it was doing instead matters most.
	State   string `json:"state"`
	Busy    bool   `json:"busy"`
	Command string `json:"command"`
	// ExitCode is meaningful only when state is "exited". It is the *session's* status.
	ExitCode int32 `json:"exit_code"`
	// LastCommandExitCode is the status of the last command the shell finished, and CommandFinished
	// whether there was one.
	//
	// Carried in the same reply that says the wait was satisfied, so `cm wait --until idle` answers "did
	// it work" without a second call -- which would race the next command starting and could report the
	// wrong one.
	LastCommandExitCode int32 `json:"last_command_exit_code"`
	CommandFinished     bool  `json:"command_finished"`
}

// reportWait prints the outcome and reports failure as an exit status.
func reportWait(w *os.File, name, until string, resp *serverv1.WaitResponse, asJSON bool) error {
	state := "running"
	switch resp.State {
	case serverv1.SessionState_SESSION_STATE_EXITED:
		state = "exited"
	case serverv1.SessionState_SESSION_STATE_DEAD:
		state = "dead"
	}

	if asJSON {
		if err := writeJSON(w, waitJSON{
			Session:             name,
			Satisfied:           resp.Satisfied,
			Until:               until,
			State:               state,
			Busy:                resp.Busy,
			Command:             resp.Command,
			ExitCode:            resp.ExitCode,
			LastCommandExitCode: resp.LastCommandExitCode,
			CommandFinished:     resp.CommandFinished,
		}); err != nil {
			return err
		}
	}

	if resp.Satisfied {
		return nil
	}

	// A timeout exits non-zero so `cm wait ... && next` does the right thing, and the detail goes to
	// stderr so it does not pollute a pipeline reading stdout.
	if !asJSON {
		// Phrased from the state rather than concatenated around it, since "running running sleep 30"
		// is what building one sentence out of both fields produces.
		var doing string
		switch {
		case state != "running":
			doing = state
		case resp.Command != "":
			doing = "running " + resp.Command
		case resp.Busy:
			doing = "running something it did not name"
		default:
			doing = "idle"
		}
		fmt.Fprintf(os.Stderr, "%s: timed out waiting for %s to be %s; it is %s\n",
			paths.Name, name, until, doing)
	}
	return &exitCodeError{code: 1, reported: true}
}
