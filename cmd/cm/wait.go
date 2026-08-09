package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
		tagArgs []string
		any     bool
	)
	cmd := &cobra.Command{
		Use:   "wait [session]",
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
'cm list' can.

--tag waits on every session carrying a tag, which is what a fan-out needs:

  cm wait --tag run=abc123 --until idle            # all of them
  cm wait --tag run=abc123 --until blocked --any   # whichever is first

The waits run concurrently, so waiting on five sessions takes as long as the
slowest rather than the sum. By default every session must reach the state and the
exit status is 0 only if all did. --any returns as soon as one does, for reacting
to whichever finishes first instead of polling for it.`,
		// At most one name, since --tag supplies the rest.
		Args:              sessionOrTagArg,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			state, ok := waitStates[until]
			if !ok {
				return fmt.Errorf("unknown state %q, want one of %s", until, strings.Join(waitStateNames(), ", "))
			}
			if any && len(tagArgs) == 0 {
				// --any describes which of several sessions, so it means nothing for one. Refused rather
				// than ignored, since a caller passing it believes it is doing something.
				return errors.New("--any only applies with --tag, which is what selects several sessions")
			}
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// withServer starts a server if none is running, which is right rather than a shortcut: a
			// session outlives its server, so a wait issued while the server is down should adopt the
			// session and answer, not fail. That is the same path an upgrade takes.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				names, _, err := sessionTargets(ctx, cl, args, tagArgs)
				if err != nil {
					return err
				}
				if len(names) == 1 {
					// One session takes the single-session path even when a selector chose it, so the
					// output and the exit status are exactly what naming it would have given.
					return waitOne(ctx, os.Stdout, os.Stderr, cl, names[0], until, state, timeout, asJSON)
				}
				return waitMany(ctx, os.Stdout, os.Stderr, cl, names, until, state, timeout, any, asJSON)
			})
		},
	}
	f := cmd.Flags()
	// Derived from the table rather than written out, which is the same reason the table exists. The
	// hand-written version omitted blocked while the command accepted it, so the one state cm cannot derive
	// for itself -- and the whole reason for the report mechanism -- was undiscoverable from the help.
	f.StringVar(&until, "until", "idle", "state to wait for: "+strings.Join(waitStateNames(), ", "))
	addTimeoutFlag(f, &timeout)
	f.StringArrayVar(&tagArgs, "tag", nil,
		"wait on the sessions with this tag, as key or key=value (repeatable, all must match)")
	f.BoolVar(&any, "any", false,
		"return as soon as one selected session reaches the state, rather than all of them")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// waitOne waits on a single session.
func waitOne(
	ctx context.Context,
	stdout, stderr io.Writer,
	cl serverv1.ServerClient,
	name, until string,
	state serverv1.WaitState,
	timeout time.Duration,
	asJSON bool,
) error {
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
	return reportWait(stdout, stderr, name, until, resp, asJSON)
}

// waitMany waits on several sessions at once, returning when all are satisfied or, with any, when the
// first is.
//
// Concurrent rather than sequential, which is the whole reason this exists rather than a shell loop.
// Waiting in sequence on five sessions each taking a minute takes five minutes, because each wait only
// starts once the previous returned: the sessions are already working in parallel and a sequential
// collector throws that away. The same trap the cm skill documents for `cm send --wait`, where the fix
// is backgrounding each one and calling wait once.
//
// One connection carries all of them. ttrpc multiplexes calls over a single connection behind its own
// send lock, and the server registers a subscription per session, so N waits need no N connections.
func waitMany(
	ctx context.Context,
	stdout, stderr io.Writer,
	cl serverv1.ServerClient,
	names []string,
	until string,
	state serverv1.WaitState,
	timeout time.Duration,
	any, asJSON bool,
) error {
	// Cancelled on return, which is what stops the siblings once --any has its answer. Without it they
	// would run to their timeouts with nothing reading the results.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type outcome struct {
		name string
		resp *serverv1.WaitResponse
		err  error
	}
	// Buffered to the full count, so a goroutine whose result is never read still exits rather than
	// blocking forever on the send. That happens on every --any run by design.
	results := make(chan outcome, len(names))

	for _, name := range names {
		go func(name string) {
			resp, err := cl.Wait(ctx, &serverv1.WaitRequest{
				Session:   name,
				Until:     state,
				TimeoutMs: uint64(timeout.Milliseconds()),
			})
			results <- outcome{name: name, resp: resp, err: err}
		}(name)
	}

	got := make(map[string]*serverv1.WaitResponse, len(names))
	var firstErr error
	for range names {
		res := <-results
		if res.err != nil {
			// Cancelling this context is how --any stops its siblings, so their cancellation is an
			// expected outcome rather than a failure worth reporting.
			if firstErr == nil && !errors.Is(res.err, context.Canceled) {
				firstErr = res.err
			}
			continue
		}
		got[res.name] = res.resp
		if any && res.resp.Satisfied {
			// The first to reach the state ends the wait, reported in the single-session form: what the
			// caller wants is which one, and what it was doing.
			return reportWait(stdout, stderr, res.name, until, res.resp, asJSON)
		}
	}
	if firstErr != nil {
		return firstErr
	}
	// Either every session was required and they have all been collected, or --any ran out of sessions
	// without one being satisfied. Both report the whole set, since the useful information on a failure
	// is which sessions did not get there.
	return reportWaitMany(stdout, stderr, names, got, until, asJSON)
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

// waitStateOf renders a session's lifecycle stage from a wait reply.
func waitStateOf(resp *serverv1.WaitResponse) string {
	switch resp.State {
	case serverv1.SessionState_SESSION_STATE_EXITED:
		return "exited"
	case serverv1.SessionState_SESSION_STATE_DEAD:
		return "dead"
	}
	return "running"
}

// waitDoing describes what a session was doing instead of reaching the state.
//
// Phrased from the state rather than concatenated around it, since "running running sleep 30" is what
// building one sentence out of both fields produces.
func waitDoing(resp *serverv1.WaitResponse) string {
	state := waitStateOf(resp)
	switch {
	case state != "running":
		return state
	case resp.Command != "":
		return "running " + resp.Command
	case resp.Busy:
		return "running something it did not name"
	default:
		return "idle"
	}
}

// toWaitJSON converts a wait reply for output.
func toWaitJSON(name, until string, resp *serverv1.WaitResponse) waitJSON {
	return waitJSON{
		Session:             name,
		Satisfied:           resp.Satisfied,
		Until:               until,
		State:               waitStateOf(resp),
		Busy:                resp.Busy,
		Command:             resp.Command,
		ExitCode:            resp.ExitCode,
		LastCommandExitCode: resp.LastCommandExitCode,
		CommandFinished:     resp.CommandFinished,
	}
}

// reportWait prints the outcome and reports failure as an exit status.
func reportWait(
	stdout, stderr io.Writer, name, until string, resp *serverv1.WaitResponse, asJSON bool,
) error {
	if asJSON {
		if err := writeJSON(stdout, toWaitJSON(name, until, resp)); err != nil {
			return err
		}
	}

	if resp.Satisfied {
		return nil
	}

	// A timeout exits non-zero so `cm wait ... && next` does the right thing, and the detail goes to
	// stderr so it does not pollute a pipeline reading stdout.
	if !asJSON {
		fmt.Fprintf(stderr, "%s: timed out waiting for %s to be %s; it is %s\n",
			paths.Name, name, until, waitDoing(resp))
	}
	return &exitCodeError{code: 1, reported: true}
}

// reportWaitMany prints the outcome for several sessions and reports failure as an exit status.
//
// Every session is reported rather than only the failures, because the useful question after a
// multi-session wait is which ones got there, and a caller reading JSON needs the whole set to act on.
// Ordered by the caller's list rather than by completion, so output does not depend on which session
// happened to finish first.
//
// Exits non-zero unless every session was satisfied. A partial success is a failure for the default
// form, since `cm wait --tag ... && collect` must not collect from sessions that are still working.
//
// Takes both writers rather than reaching for os.Stderr, so the diagnostics are testable. They are the
// part worth testing: which sessions failed and what they were doing instead is the whole output of a
// failed multi-session wait.
func reportWaitMany(
	stdout, stderr io.Writer,
	names []string,
	got map[string]*serverv1.WaitResponse,
	until string,
	asJSON bool,
) error {
	if asJSON {
		out := make([]waitJSON, 0, len(names))
		for _, name := range names {
			resp, ok := got[name]
			if !ok {
				continue
			}
			out = append(out, toWaitJSON(name, until, resp))
		}
		if err := writeJSON(stdout, out); err != nil {
			return err
		}
	}

	var unsatisfied []string
	for _, name := range names {
		resp, ok := got[name]
		if !ok || !resp.Satisfied {
			unsatisfied = append(unsatisfied, name)
		}
	}
	if len(unsatisfied) == 0 {
		return nil
	}

	if !asJSON {
		// One line per session that did not get there, naming what it was doing instead. A single
		// summary line would drop exactly the detail that says which session to look at.
		for _, name := range unsatisfied {
			resp, ok := got[name]
			if !ok {
				continue
			}
			fmt.Fprintf(stderr, "%s: timed out waiting for %s to be %s; it is %s\n",
				paths.Name, name, until, waitDoing(resp))
		}
		fmt.Fprintf(stderr, "%s: %d of %d sessions did not reach %s\n",
			paths.Name, len(unsatisfied), len(names), until)
	}
	return &exitCodeError{code: 1, reported: true}
}
