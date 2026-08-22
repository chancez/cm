package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/tags"
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
		session  string
		dir      string
		detach   bool
		timeout  time.Duration
		persist  bool
		asJSON   bool
		env      []string
		quiet    bool
		raw      bool
		tagArgs  []string
		match    string
		matchRaw bool
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
			sessionTags, err := tags.ParseAll(tagArgs)
			if err != nil {
				return err
			}
			if matchRaw && match == "" {
				return errors.New("--match-raw only applies with --match")
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			if err := ensureServer(cmd.Context(), dirs); err != nil {
				return err
			}

			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				id, created, err := startRun(ctx, cl, runOptions{
					session: session,
					dir:     dir,
					command: args,
					persist: persist,
					env:     sessionEnv(env),
					tags:    sessionTags,
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
				// Every RPC takes a reference rather than a bare identity, so an ID has to carry the
				// sigil: passed bare it would be looked up as a name and find nothing.
				ref := session
				if ref == "" {
					ref = paths.FormatSessionID(id)
				}

				if !created {
					cfg, cerr := g.config()
					if cerr != nil {
						return cerr
					}
					logger, closeLog := newClientLogger(dirs, cfg)
					if closeLog != nil {
						defer closeLog.Close()
					}
					return runInExistingSession(
						cmd.Context(), dirs, ref, args, detach, timeout, quiet, raw,
						match, matchRaw, logger)
				}

				if detach {
					// The reference rather than the ID, so a caller doing `s=$(cm run -d ...)` gets
					// back the name it chose when it chose one, and something it can pass straight to
					// another command either way.
					if asJSON {
						return writeJSON(os.Stdout, map[string]string{"session": ref, "id": id})
					}
					_, err := fmt.Println(ref)
					return err
				}

				return waitForRun(ctx, cl, id, ref, timeout, asJSON, quiet)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&session, "session", "",
		"name for the session (the server allocates one when omitted)")
	f.StringVar(&dir, "dir", "", "working directory for the command")
	f.BoolVarP(&detach, "detach", "d", false,
		"return as soon as the command starts, printing the session name")
	addTimeoutFlag(f, &timeout)
	f.BoolVar(&persist, "persist", false,
		"keep the session's output across a reboot")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	f.BoolVarP(&quiet, "quiet", "q", false,
		"do not print the command's output; rely on the exit status")
	f.BoolVar(&raw, "raw", false,
		"keep escape sequences in the output instead of stripping them")
	f.StringArrayVar(&env, "env", nil,
		"set a KEY=VALUE in the command's environment (repeatable)")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"label the session, as key or key=value (repeatable, ignored when reusing a session)")
	f.StringVar(&match, "match", "",
		"on a reused session, wait until this text appears instead of for the shell to be idle")
	f.BoolVar(&matchRaw, "match-raw", false,
		"match the bytes the program emitted rather than the text they rendered to")
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
	// tags label the session, for grouping a fan-out of runs and addressing it as a unit.
	tags map[string]string
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
) (id string, created bool, err error) {
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
				Rows:    24,
				Cols:    80,
				Persist: opts.persist,
				// Set on the session rather than inherited, because the shim is spawned by the server:
				// whatever the caller exported is not in the server's environment and so never reaches
				// the command.
				Env: opts.env,
				// Only applied when this call creates the session. Reusing a name sends the command
				// to a shell that is already running, and its tags were set when it was created.
				Tags: opts.tags,
				// Always, so `cm history` works once the command has exited, which is what this
				// command's help promises. Without it the output is gone the moment the command
				// finishes, which for a short command is immediately.
				//
				// Not the same as --persist: this does not ask the session to survive a reboot, and
				// the session stays eligible for prompt cleanup rather than being kept for the week a
				// persisted session gets.
				CaptureOutput: true,
				// Reported like every other client's, so a `cm run` holding a session appears in a
				// listing with a build and a pid rather than as an anonymous attachment. This Open is
				// built by hand rather than through Options.Open, which is the drift that constructor
				// exists to prevent, so anything added there has to be repeated here.
				ClientVersion: paths.Version(),
				ClientPid:     int32(os.Getpid()),
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
		Event: &serverv1.AttachRequest_Detach{Detach: &serverv1.Detach{NoAck: true}},
	})
	// The ID rather than the name. Polling by name would find nothing at all for a `cm run` that named
	// no session, and could find a *different* session if the name were pointed elsewhere while the
	// command ran.
	return opened.SessionId, opened.Created, nil
}

// waitForRun blocks until the command finishes, then exits with its status.
func waitForRun(
	ctx context.Context,
	cl serverv1.ServerClient,
	id string,
	ref string,
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
		// Unfiltered: a prefix filters on names, and this session is identified by its ID, which it has
		// whether or not anything names it. Session counts are in the tens.
		resp, err := cl.List(ctx, &serverv1.ListRequest{})
		if err != nil {
			return err
		}

		for _, s := range resp.Sessions {
			if s.Id != id {
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
				if err := printRunOutput(ctx, cl, ref); err != nil {
					return err
				}
			}

			if asJSON {
				if err := writeJSON(os.Stdout, map[string]any{
					"session":   ref,
					"id":        id,
					"state":     state,
					"exit_code": s.ExitCode,
				}); err != nil {
					return err
				}
			}
			if state == "dead" {
				// The shim vanished, so there is no status to report. Distinguished from an exit
				// because "the command failed" and "cm lost track of it" are different problems.
				return fmt.Errorf("session %s ended unexpectedly", ref)
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
				return fmt.Errorf("timed out waiting for %s; it is still running", ref)
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
func printRunOutput(ctx context.Context, cl serverv1.ServerClient, ref string) error {
	resp, err := cl.Read(ctx, &serverv1.ReadRequest{
		Session: ref,
		Lines:   0,
		Unwrap:  true,
	})
	if err != nil {
		// Not fatal. The command's status is the point of this command, and failing to show its output should
		// not turn a successful run into a failure. Reported so the absence is not silent.
		fmt.Fprintf(os.Stderr, "warning: could not read %s output: %v\n", ref, err)
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
	match string,
	matchRaw bool,
	log *slog.Logger,
) error {
	// The submitting CR is held apart from the command line rather than appended, so the server writes it
	// as its own pty write. See writeInputThenEnter: concatenating them makes a long command line arrive
	// as a multi-read burst that a full-screen reader treats as a paste, swallowing the CR.
	data := strings.Join(command, " ")
	const enter = "\r"

	// Detached, or asked to say nothing: send and return without watching. `--detach` means "do not wait", and
	// that reading is the same whether the session was created or reused.
	if detach || quiet {
		conn, cl, err := dialServer(dirs)
		if err != nil {
			return err
		}
		defer conn.Close()

		until := serverv1.WaitState_WAIT_STATE_UNSPECIFIED
		if !detach && match == "" {
			// Not detached, so the exit status still has to be waited for even though nothing is printed.
			until = serverv1.WaitState_WAIT_STATE_IDLE
		}
		if _, err := cl.Send(ctx, &serverv1.SendRequest{
			Session:       name,
			Data:          []byte(data),
			Enter:         []byte(enter),
			WaitUntil:     until,
			Match:         match,
			MatchRaw:      matchRaw,
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

	if match != "" {
		// Send, wait for the text, then print what the command produced.
		//
		// Not sendAndFollow, which streams until its wait resolves: a match resolves the moment the text
		// appears, which is usually mid-command, so following would cut the stream off partway through
		// output the caller wanted and read as truncation. Reading afterwards prints a complete view of
		// what the session has, which is what --match is for on a shell that cannot say when a command
		// ended.
		return sendMatchThenRead(ctx, dirs, name, data, enter, match, matchRaw, timeout, raw)
	}

	return sendAndFollow(ctx, dirs, name, data, enter, serverv1.WaitState_WAIT_STATE_IDLE, timeout, raw, log)
}

// sendMatchThenRead sends input, waits for a pattern, and prints the session's recent output.
func sendMatchThenRead(
	ctx context.Context,
	dirs paths.Dirs,
	name, data, enter, match string,
	matchRaw bool,
	timeout time.Duration,
	raw bool,
) error {
	conn, cl, err := dialServer(dirs)
	if err != nil {
		return err
	}
	defer conn.Close()

	resp, err := cl.Send(ctx, &serverv1.SendRequest{
		Session:       name,
		Data:          []byte(data),
		Enter:         []byte(enter),
		Match:         match,
		MatchRaw:      matchRaw,
		WaitTimeoutMs: uint64(timeout.Milliseconds()),
	})
	if err != nil {
		return err
	}

	// Printed whether or not the text appeared, since output produced before a timeout is still what the
	// command said and is usually how a caller works out why the match failed.
	out, readErr := cl.Read(ctx, &serverv1.ReadRequest{
		Session: name,
		Lines:   uint32(defaultReadLines),
		Unwrap:  true,
		Raw:     raw,
	})
	if readErr == nil {
		if _, err := os.Stdout.Write(out.Data); err != nil {
			return err
		}
	}

	if w := resp.GetWait(); w != nil && !w.Satisfied {
		return fmt.Errorf("timed out waiting for %q in %s", match, name)
	}
	return nil
}
