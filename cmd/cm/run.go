package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
		env     []string
		quiet   bool
		raw     bool
	)
	cmd := &cobra.Command{
		Use:   "run [flags] -- <command>...",
		Short: "Run a command in a new session",
		Long: `Run a command in a new session, without attaching a terminal to it.

The command runs on a pty, so programs that check for a terminal behave as they
would interactively.

By default this waits for the command to finish, prints its output, and exits with
its status, so it composes with anything that checks an exit code. --detach returns
immediately and leaves the session running, printing the session name instead.

--session names the session, and reusing a name reuses the session: the first call
creates it, later calls send the command to the shell already running there. That
keeps a directory or an activated environment between runs, and costs one pty rather
than one per command.

Reuse changes how the command is interpreted, which is worth knowing. Creating a
session runs the arguments as an argv, untouched. Reusing one sends them to a shell,
joined with spaces, so the shell parses them and your quoting matters:

  cm run --session build -- make -j4        # creates: argv, no shell involved
  cm run --session build -- 'make -j4'      # reuses: the shell parses this

The output is rendered rather than raw: escape sequences are stripped, so a
redirected build log is text rather than a file full of colour codes. Use
'cm history --format vt' for the bytes as the program emitted them, and --quiet to
print nothing but rely on the exit status.

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
				name, created, err := startRun(ctx, cl, runOptions{
					session: session,
					dir:     dir,
					command: args,
					persist: persist,
					env:     env,
				})
				if err != nil {
					return err
				}

				// An existing session has its own shell, so the command has to be sent to it rather than
				// being the session. Without this the command was silently dropped: the server returned the
				// existing session and ignored the command, so this exited 0 having run nothing.
				//
				// Sent through the same path as `cm send --follow`, which already solves the ordering and the
				// stop condition, rather than reimplementing either here.
				if !created {
					return runInExistingSession(cmd.Context(), dirs, name, args, detach, timeout, quiet, raw)
				}

				if detach {
					if asJSON {
						return writeJSON(os.Stdout, map[string]string{"session": name})
					}
					_, err := fmt.Println(name)
					return err
				}

				return waitForRun(ctx, cl, name, timeout, asJSON, quiet)
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
	f.BoolVarP(&quiet, "quiet", "q", false,
		"do not print the command's output; rely on the exit status")
	f.BoolVar(&raw, "raw", false,
		"keep escape sequences in the output instead of stripping them")
	f.StringArrayVar(&env, "env", nil,
		"set a KEY=VALUE in the command's environment (repeatable)")
	return cmd
}

type runOptions struct {
	session string
	dir     string
	command []string
	persist bool
	// env holds extra KEY=VALUE entries for the command.
	//
	// Set on the session rather than inherited from this process, because the shim is spawned by the
	// server: whatever the caller exported is not in the server's environment and so never reaches the
	// command.
	env []string
}

// startRun opens a session running the command and returns its resolved name.
//
// Implemented as an attach that immediately detaches, because a session *is* a command on a pty: the
// only difference between this and `cm attach` is that no terminal is wired up. Reusing the path
// means run inherits naming, persistence, and exit tracking rather than reimplementing them.
// startRun opens the session and reports whether it was created by this call.
//
// The created flag is what distinguishes the two things `cm run --session NAME` can mean. A session that did
// not exist runs the command as its shell; one that already existed has a shell of its own, and the command
// has to be sent to it. Without this the server returned the existing session and silently discarded the
// command, so `cm run --session build -- make` exited 0 having run nothing and printed the *previous*
// command's output.
func startRun(
	ctx context.Context, cl serverv1.ServerClient, opts runOptions,
) (name string, created bool, err error) {
	stream, err := cl.Attach(ctx)
	if err != nil {
		return "", false, err
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
				// Set on the session rather than inherited, because the shim is spawned by the server:
				// whatever the caller exported is not in the server's environment and so never reaches
				// the command.
				Env: opts.env,
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
		return "", false, err
	}

	resp, err := stream.Recv()
	if err != nil {
		return "", false, err
	}
	opened := resp.GetOpened()
	if opened == nil {
		return "", false, errors.New("server did not open the session")
	}

	// Detach explicitly rather than dropping the connection, so the session is left running for the
	// same reason a user detaching leaves theirs running.
	_ = stream.Send(&serverv1.AttachRequest{
		Event: &serverv1.AttachRequest_Detach{Detach: &serverv1.Detach{}},
	})
	return opened.Session, opened.Created, nil
}

// waitForRun blocks until the command finishes, then exits with its status.
func waitForRun(
	ctx context.Context,
	cl serverv1.ServerClient,
	name string,
	timeout time.Duration,
	asJSON bool,
	quiet bool,
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

			// The command's output, printed before anything is said about its status.
			//
			// The ordering matters for a failing command: returning the status first means the output that
			// explains the failure is never printed, which is the opposite of useful. Printed even when the
			// command failed, for the same reason.
			//
			// Rendered rather than raw, matching `cm read`: escape sequences in a captured build log are noise
			// at best and corrupt a redirected file at worst. `cm history --format vt` is there for the raw
			// bytes.
			//
			// Skipped in JSON mode, where mixing free-form output into a structured document would break the
			// parser that asked for JSON.
			if !quiet && !asJSON {
				if err := printRunOutput(ctx, cl, name); err != nil {
					return err
				}
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

// printRunOutput writes a finished command's captured output.
//
// Read with lines set to everything, since a caller running a command wants all of it: the 100-line default of
// `cm read` is for looking at a live session, and truncating a build log to its tail would lose the error at the
// top.
//
// Unwrapped, so a path or a stack frame the terminal broke to fit 80 columns comes back as one line. The width
// is cm's own choice here -- `cm run` opens the session at 24x80 -- so rejoining is undoing something cm did
// rather than something the program meant.
func printRunOutput(ctx context.Context, cl serverv1.ServerClient, name string) error {
	resp, err := cl.Read(ctx, &serverv1.ReadRequest{
		Session: name,
		Lines:   0,
		Unwrap:  true,
	})
	if err != nil {
		// Not fatal. The command's status is the point of this command, and failing to show its output should
		// not turn a successful run into a failure. Reported so the absence is not silent.
		fmt.Fprintf(os.Stderr, "warning: could not read %s output: %v\n", name, err)
		return nil
	}
	if len(resp.Data) == 0 {
		return nil
	}
	if _, err := os.Stdout.Write(resp.Data); err != nil {
		return err
	}
	// A trailing newline when the render lacks one, so a shell prompt does not land on the same line as the
	// last line of output.
	if n := len(resp.Data); n > 0 && resp.Data[n-1] != '\n' {
		if _, err := os.Stdout.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	return nil
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

// runInExistingSession sends a command to a session that already exists and waits for it.
//
// The other half of `cm run --session NAME`, and what makes naming a session useful: the first call creates it,
// every later one reuses it. Reusing a shell is also cheaper than a pty per command, and it keeps state --
// a directory, an activated environment -- between runs, which is usually why someone named the session.
//
// The command is joined with spaces and a carriage return appended, exactly as `cm send --enter` does, because
// this is input to a shell rather than an argv: the shell parses it. That means the caller's quoting matters
// here in a way it does not when creating a session, where the argv is passed through untouched. Worth knowing,
// and the reason the help says so.
//
// Delegates to the follow path rather than reimplementing it. That path already establishes the stream before
// sending, so nothing the command prints at the start is missed, and it already knows when to stop.
func runInExistingSession(
	ctx context.Context,
	dirs paths.Dirs,
	name string,
	command []string,
	detach bool,
	timeout time.Duration,
	quiet, raw bool,
) error {
	data := strings.Join(command, " ") + "\r"

	// Detached, or asked to say nothing: send and return without watching. `--detach` means "do not wait", and
	// that reading is the same whether the session was created or reused.
	if detach || quiet {
		conn, cl, err := dialServer(dirs)
		if err != nil {
			return err
		}
		defer conn.Close()

		until := serverv1.WaitState_WAIT_STATE_UNSPECIFIED
		if !detach {
			// Not detached, so the exit status still has to be waited for even though nothing is printed.
			until = serverv1.WaitState_WAIT_STATE_IDLE
		}
		if _, err := cl.Send(ctx, &serverv1.SendRequest{
			Session:       name,
			Data:          []byte(data),
			WaitUntil:     until,
			WaitTimeoutMs: uint64(timeout.Milliseconds()),
		}); err != nil {
			return err
		}
		if detach {
			_, err := fmt.Println(name)
			return err
		}
		return nil
	}

	return sendAndFollow(ctx, dirs, name, data, serverv1.WaitState_WAIT_STATE_IDLE, timeout, raw)
}
