package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// runPollInterval is how often a waiting `run` checks whether the command finished.
//
// Polling rather than a streaming wait: the exit status lives in the session record, which is already
// updated when the shell exits, so a poll needs no new RPC and no long-lived stream that a server
// restart would break.
const runPollInterval = 200 * time.Millisecond

func newRunCommand(g *globals) *cobra.Command {
	var (
		session string
		dir     string
		detach  bool
		timeout time.Duration
		persist bool
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "run [flags] -- <command>...",
		Short: "Run a command in a new session",
		Long: `Run a command in a new session, without attaching a terminal to it.

The command runs on a pty, so programs that check for a terminal behave as they
would interactively, and its output is captured as session scrollback readable
with 'cm history'.

By default this waits for the command to finish and exits with its status, so it
composes with anything that checks an exit code. --detach returns immediately and
leaves the session running.

Unlike waiting on a shell prompt, the exit status here is the real one: the shim
owns the process and reaps it, so nothing is inferred from output.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if cmd.ArgsLenAtDash() > 0 {
				return errors.New("arguments before -- are not accepted; use flags")
			}
			if len(args) == 0 {
				return errors.New("no command given; use -- <command>")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if session != "" {
				if err := paths.ValidateSessionName(session); err != nil {
					return err
				}
			}
			if dir == "" {
				dir, _ = os.Getwd()
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			if err := ensureServer(cmd.Context(), dirs); err != nil {
				return err
			}

			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				name, err := startRun(ctx, cl, runOptions{
					session: session,
					dir:     dir,
					command: args,
					persist: persist,
				})
				if err != nil {
					return err
				}

				if detach {
					if asJSON {
						return writeJSON(os.Stdout, map[string]string{"session": name})
					}
					_, err := fmt.Println(name)
					return err
				}

				return waitForRun(ctx, cl, name, timeout, asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&session, "session", "",
		"name for the session (the server allocates one when omitted)")
	f.StringVar(&dir, "dir", "", "working directory for the command")
	f.BoolVarP(&detach, "detach", "d", false,
		"return as soon as the command starts, printing the session name")
	f.DurationVar(&timeout, "timeout", 0,
		"give up waiting after this long (0 waits indefinitely)")
	f.BoolVar(&persist, "persist", false,
		"keep the session's output across a reboot")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

type runOptions struct {
	session string
	dir     string
	command []string
	persist bool
}

// startRun opens a session running the command and returns its resolved name.
//
// Implemented as an attach that immediately detaches, because a session *is* a command on a pty: the
// only difference between this and `cm attach` is that no terminal is wired up. Reusing the path
// means run inherits naming, persistence, and exit tracking rather than reimplementing them.
func startRun(ctx context.Context, cl serverv1.ServerClient, opts runOptions) (string, error) {
	stream, err := cl.Attach(ctx)
	if err != nil {
		return "", err
	}

	if err := stream.Send(&serverv1.AttachRequest{
		Event: &serverv1.AttachRequest_Open{
			Open: &serverv1.Open{
				Session: opts.session,
				Command: opts.command,
				Cwd:     opts.dir,
				// A conventional size rather than none: a program that asks gets a plausible answer
				// instead of zeros, and full-screen output is captured at a usable width.
				Rows: 24,
				Cols: 80,
				// Never owned. An owning client ends its session on disconnect, and this client
				// disconnects immediately by design.
				Own:     false,
				Persist: opts.persist,
				// Always, so `cm history` works once the command has exited, which is what this
				// command's help promises. Without it the output is gone the moment the command
				// finishes, which for a short command is immediately.
				//
				// Not the same as --persist: this does not ask the session to survive a reboot, and
				// the session stays eligible for prompt cleanup rather than being kept for the week a
				// persisted session gets.
				CaptureOutput: true,
			},
		},
	}); err != nil {
		return "", err
	}

	resp, err := stream.Recv()
	if err != nil {
		return "", err
	}
	opened := resp.GetOpened()
	if opened == nil {
		return "", errors.New("server did not open the session")
	}

	// Detach explicitly rather than dropping the connection, so the session is left running for the
	// same reason a user detaching leaves theirs running.
	_ = stream.Send(&serverv1.AttachRequest{
		Event: &serverv1.AttachRequest_Detach{Detach: &serverv1.Detach{}},
	})
	return opened.Session, nil
}

// waitForRun blocks until the command finishes, then exits with its status.
func waitForRun(
	ctx context.Context,
	cl serverv1.ServerClient,
	name string,
	timeout time.Duration,
	asJSON bool,
) error {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	for {
		resp, err := cl.List(ctx, &serverv1.ListRequest{Prefix: name})
		if err != nil {
			return err
		}

		for _, s := range resp.Sessions {
			if s.Name != name {
				continue
			}
			state := stateName(s)
			if state == "running" {
				break
			}

			if asJSON {
				if err := writeJSON(os.Stdout, map[string]any{
					"session":   name,
					"state":     state,
					"exit_code": s.ExitCode,
				}); err != nil {
					return err
				}
			}
			if state == "dead" {
				// The shim vanished, so there is no status to report. Distinguished from an exit
				// because "the command failed" and "cm lost track of it" are different problems.
				return fmt.Errorf("session %s ended unexpectedly", name)
			}
			if s.ExitCode != 0 {
				// The command's status is this command's status, so `cm run -- false` fails the way
				// `false` does. Already reported above in JSON mode, so nothing is printed twice.
				return &exitCodeError{code: int(s.ExitCode), reported: asJSON}
			}
			return nil
		}

		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("timed out waiting for %s; it is still running", name)
			}
			return ctx.Err()
		case <-time.After(runPollInterval):
		}
	}
}

// exitCodeError carries a command's exit status so main can exit with it.
type exitCodeError struct {
	code int
	// reported means the detail has already been printed, so main should not print it again.
	reported bool
}

func (e *exitCodeError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.code)
}

// ExitCode is what the process should exit with.
func (e *exitCodeError) ExitCode() int { return e.code }
