package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// signalNames maps the names people use to signal numbers.
//
// A table rather than a lookup through the C library, because Go exposes no name-to-signal function and
// the set here is what a person actually sends to a session. Restricted to signals that make sense for
// one: the fatal ones a caller reaches for, plus the job-control pair.
//
// Deliberately not every signal the platform defines. A caller who needs an obscure one can pass the
// number, which is why numbers are accepted at all, and listing everything would mean maintaining a
// table that disagrees with the host it runs on.
var signalNames = map[string]syscall.Signal{
	"hup":   syscall.SIGHUP,
	"int":   syscall.SIGINT,
	"quit":  syscall.SIGQUIT,
	"kill":  syscall.SIGKILL,
	"term":  syscall.SIGTERM,
	"usr1":  syscall.SIGUSR1,
	"usr2":  syscall.SIGUSR2,
	"stop":  syscall.SIGSTOP,
	"cont":  syscall.SIGCONT,
	"tstp":  syscall.SIGTSTP,
	"winch": syscall.SIGWINCH,
}

// signalNamesList returns the accepted names in a stable order, for help and error text.
func signalNamesList() []string {
	out := make([]string, 0, len(signalNames))
	for name := range signalNames {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// parseSignal resolves a signal name or number.
//
// Accepts "int", "SIGINT", "sigint", and "2", since a caller coming from `kill` will try any of them and
// rejecting a spelling that obviously means one thing is a gratuitous failure.
func parseSignal(spec string) (int32, error) {
	s := strings.ToLower(strings.TrimSpace(spec))
	if s == "" {
		return 0, fmt.Errorf("a signal is required")
	}

	// A bare number, for anything not in the table.
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			// Zero is a real argument to kill(2), where it probes for a process rather than signalling
			// it, but as a request here it is almost certainly a mistake.
			return 0, fmt.Errorf("signal number must be positive, got %d", n)
		}
		return int32(n), nil
	}

	s = strings.TrimPrefix(s, "sig")
	sig, ok := signalNames[s]
	if !ok {
		return 0, fmt.Errorf("unknown signal %q, want one of %s, or a number",
			spec, strings.Join(signalNamesList(), ", "))
	}
	return int32(sig), nil
}

func newSignalCommand(g *globals) *cobra.Command {
	var (
		processOnly bool
		tagArgs     []string
	)
	cmd := &cobra.Command{
		Use:   "signal [session] <signal>",
		Short: "Send a signal to a session's shell and its process group",
		Long: `Send a signal to a session's shell, and to its process group by default.

  cm signal build int          # interrupt, as ctrl-c would
  cm signal build term         # ask it to stop
  cm signal build 9            # a number, for anything not named
  cm signal --tag run=abc int  # every session in a group

Names may be given as int, SIGINT, or 2. The named signals are hup, int, quit,
kill, term, usr1, usr2, stop, cont, tstp, and winch; anything else can be passed as
a number.

This is not the same as sending ctrl-c with 'cm send --key', and which one is right
depends on what you are stopping. A control character travels through the pty, so
the line discipline decides what it means: a program that put its terminal in raw
mode reads it as an ordinary byte and never sees a signal, and a shell sitting at a
prompt with no job running has nothing to interrupt. A signal is delivered to the
process regardless. So --key is what a keypress does, and this is what you reach
for when a keypress did not work.

The process group is signalled rather than the shell alone, matching what a
keypress does: a pty delivers to the foreground process group, so signalling only
the shell would leave the job you meant to stop running. --process-only sends to
the shell alone.

A session that has ended is reported rather than treated as success, since a signal
needs a process to receive it.`,
		Args: sessionOrTagArg2,
		// Only the first argument completes as a session name; the second is a signal.
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionArg, sigSpec, err := splitSignalArgs(args, tagArgs)
			if err != nil {
				return err
			}
			sig, err := parseSignal(sigSpec)
			if err != nil {
				return err
			}
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Deliberately not ensureServer, matching `cm report` and `cm tag`: signalling a session
			// implies one exists, and starting a server here would create one that has never heard of it.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				names, _, err := sessionTargets(ctx, cl, sessionArg, tagArgs)
				if err != nil {
					return err
				}
				// Every failure is collected rather than stopping at the first, so signalling a group
				// does not leave an arbitrary suffix of it unsignalled with no report of which.
				var failed []string
				for _, name := range names {
					if _, err := cl.Signal(ctx, &serverv1.SignalRequest{
						Session:     name,
						Signal:      sig,
						ProcessOnly: processOnly,
					}); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s: %v\n", name, err)
						failed = append(failed, name)
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "signalled %s\n", name)
				}
				if len(failed) > 0 {
					return &exitCodeError{code: 1, reported: true}
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&processOnly, "process-only", false,
		"signal the shell alone rather than its process group")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"signal the sessions with this tag, as key or key=value (repeatable, all must match)")
	return cmd
}

// splitSignalArgs separates the session name from the signal.
//
// With --tag the signal is the only positional argument, so the two forms differ in length rather than
// in shape. Kept explicit rather than inferred from whether the argument parses as a signal: a session
// named "int" is legal, and guessing would make that session unsignalable in a way nothing explains.
func splitSignalArgs(args, tagArgs []string) (sessionArg []string, sigSpec string, err error) {
	switch {
	case len(tagArgs) > 0:
		if len(args) != 1 {
			return nil, "", fmt.Errorf(
				"with --tag, give only the signal, got %d arguments", len(args))
		}
		return nil, args[0], nil
	case len(args) == 2:
		return args[:1], args[1], nil
	case len(args) == 1:
		// One argument and no selector: the session is the calling one, from CM_SESSION, so a program
		// can signal its own session's job.
		name, err := sessionTarget(nil, "signal")
		if err != nil {
			return nil, "", err
		}
		return []string{name}, args[0], nil
	default:
		return nil, "", fmt.Errorf("expected a session and a signal, got %d arguments", len(args))
	}
}

// sessionOrTagArg2 accepts the one or two positional arguments `cm signal` takes.
func sessionOrTagArg2(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("a signal is required")
	}
	if len(args) > 2 {
		return fmt.Errorf("expected at most a session and a signal, got %d arguments", len(args))
	}
	return nil
}
