package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/client"
	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/input"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/sessionenv"
	"github.com/chancez/cm/internal/tags"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newAttachCommand(g *globals) *cobra.Command {
	var (
		readOnly     bool
		dir          string
		setTitle     bool
		persist      bool
		onRestore    string
		env          []string
		detachKeyArg string
		noAttach     bool
		tagArgs      []string
	)
	cmd := &cobra.Command{
		Use: "attach [session]",
		// The one command typed dozens of times a day, so it gets the shortest name that is not ambiguous.
		Aliases: []string{"a"},
		Short:   "Attach to a session, creating it if needed",
		Long: `Attach to a session, creating it if it does not exist.

Being idempotent is what lets a terminal emulator use one command for both
creating a window's session and reattaching to it after a restart.

With no name, the server allocates one and prints it, which is how a per-window
session is created without the caller inventing names.

--no-attach creates the session and prints its name without attaching, for
pre-creating one that something else will attach to later. Distinct from
'cm run -d', which needs a command and captures its output for a few minutes: this
makes an ordinary shell session with ordinary persistence.

Detaching leaves the session running. The key is ctrl-\ by default, set by
detach_key in the config file, and overridden for one attachment by --detach-key.

That override matters when something outside this client already claims the key: a
multiplexer you are attaching from within sees it first, so the inner client never
receives it and the window closes instead of detaching.`,
		// Only the args before "--" are the session name; everything after is the command
		// to run, so MaximumNArgs would wrongly reject "attach x -- sh -c ...".
		Args: func(cmd *cobra.Command, args []string) error {
			n := cmd.ArgsLenAtDash()
			if n < 0 {
				n = len(args)
			}
			if n > 1 {
				return fmt.Errorf("expected at most one session name before --, got %d", n)
			}
			return nil
		},
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			var session string
			if n := cmd.ArgsLenAtDash(); n != 0 && len(args) > 0 {
				session = args[0]
				if err := paths.ValidateSessionName(session); err != nil {
					return err
				}
			}
			sessionTags, err := tags.ParseAll(tagArgs)
			if err != nil {
				return err
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			if dir == "" {
				// Default to the caller's cwd so a new session starts where the user is,
				// which is what a terminal emulator opening a window expects.
				dir, _ = os.Getwd()
			}
			cfg, err := g.config()
			if err != nil {
				return err
			}

			// The flag wins over the config file, as flags do. Worth having as a flag and not only a
			// setting: the case for changing it is usually one attachment rather than every one, such as
			// attaching from inside another multiplexer that claims ctrl-\ for itself.
			keySpec := cfg.DetachKey
			if detachKeyArg != "" {
				keySpec = detachKeyArg
			}
			detachKey, err := client.ParseDetachKey(keySpec)
			if err != nil {
				return err
			}

			opts := client.Options{
				SocketPath: dirs.ServerSocket(),
				Session:    session,
				ReadOnly:   readOnly,
				Dir:        dir,
				Command:    argsAfterDash(cmd, args),
				DetachKey:  detachKey,
				// Recorded so a shell already running in this session can refresh values that
				// describe the terminal, which may have been replaced since it started.
				ClientEnv: sessionenv.Capture(os.Environ(), cfg.EnvMatcher()),
				Persist:   persist,
				OnRestore: onRestore,
				Tags:      sessionTags,
				// Set on the session rather than inherited, because the shim is spawned by the server:
				// whatever this process exported is not in the server's environment and so never
				// reaches the shell. Only applies when this call creates the session.
				//
				// Carries this client's whole environment as well as --env, so a session resembles
				// the thing that created it rather than the shell that happened to start the server,
				// which may be days old. See sessionEnv.
				Env: sessionEnv(env),
				// Tells the server this attach is nested, so the session it is running inside stops
				// reading the bytes passing through it as reports about itself.
				InsideSession: insideCmSession(),
			}
			logger, closeLog := newClientLogger(dirs, cfg)
			if closeLog != nil {
				defer closeLog.Close()
			}
			opts.Log = logger

			// Checked before anything that assumes a terminal, since this path has none.
			if noAttach {
				return createWithoutAttaching(cmd.Context(), dirs, opts)
			}

			if setTitle {
				// Forward the session's title to the outer terminal.
				//
				// The shell reports its title to cm, not to the terminal, so without this a tab
				// shows the client's process name. Emitted only when output is a terminal, since
				// escape bytes in a pipe would corrupt it.
				opts.OnMetadata = func(meta client.SessionMetadata) {
					if meta.Title == "" {
						return
					}
					fmt.Fprintf(os.Stdout, "\x1b]2;%s\x07", meta.Title)
				}
			}
			return runAttach(cmd.Context(), dirs, opts)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&readOnly, "read-only", false,
		"follow the session without sending input")
	f.StringVar(&dir, "dir", "", "working directory for a newly created session")
	f.BoolVar(&setTitle, "set-title", true,
		"forward the session's title to the terminal")
	f.BoolVar(&persist, "persist", false,
		"keep this session's content across a reboot")
	f.StringVar(&onRestore, "on-restore", "",
		"what to do when reviving this session: shell, none, or command")
	f.StringArrayVar(&env, "env", nil,
		"set a KEY=VALUE in the new session's environment (repeatable, ignored when reattaching)")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"label the new session, as key or key=value (repeatable, ignored when reattaching)")
	f.StringVar(&detachKeyArg, "detach-key", "",
		`key that detaches this client: "ctrl-<key>" or "none" (default from config)`)
	f.BoolVar(&noAttach, "no-attach", false,
		"create the session and print its name without attaching")
	return cmd
}

// sessionEnv returns the environment for a session this client creates: this process's own
// environment, then the caller's --env entries.
//
// Forwarding this process's environment is what makes a session resemble the thing that created it.
// A session created by hand from a shell gets that shell's environment, like a subshell would, and a
// session created by a terminal emulator's integration gets the emulator's, which is close to fresh
// because such a client has no shell between it and launchd. Both fall out of forwarding rather than
// needing a mode flag: the client's own ancestry is the signal, and it is already correct.
//
// Order is relied on rather than assumed. Go's exec dedups the environment it passes, keeping the
// last occurrence of a name, so --env last means an explicit `--env PATH=...` beats the forwarded
// value instead of being silently dropped behind it. Verified both ways, since the failure would be
// quiet: with the order reversed the flag appears to work while doing nothing.
//
// Shared by attach and run because they build separate Open messages, and nothing makes the two
// agree. That is not hypothetical here: --tag worked everywhere except --no-attach for exactly this
// reason, accepted and validated and then dropped.
func sessionEnv(env []string) []string {
	return sessionEnvFrom(os.Environ(), env)
}

// sessionEnvFrom is sessionEnv with the process environment passed in.
//
// Split out so a test can supply a small environment instead of the real one. That is not only for
// convenience: a test asserting on the whole result prints it on failure, and with os.Environ()
// inlined here a failure dumped the developer's actual environment, API tokens included, into the
// test output. Failure output goes to terminals, CI logs, and bug reports.
func sessionEnvFrom(environ, env []string) []string {
	return append(sessionenv.Inherit(environ), env...)
}

// argsAfterDash returns the command to run in a new session, which is everything after a
// literal "--". Keeping it separate from the session name means a command can contain
// anything without being mistaken for a flag.
func argsAfterDash(cmd *cobra.Command, args []string) []string {
	if n := cmd.ArgsLenAtDash(); n >= 0 && n <= len(args) {
		return args[n:]
	}
	return nil
}

// newClientLogger opens the shared client log, tagged with which client is writing.
//
// One file for every client, with pid and boot as fields rather than in the filename. A client is short-lived
// and there can be one per attached window, so a file each would accumulate for diagnostics that are only read
// when something is wrong. The fields keep it filterable: pid names the process, and boot distinguishes a reused
// pid from the same pid in this boot, which matters because the log outlives a reboot.
//
// Failing to open the log is not fatal. Diagnostics are advisory, and refusing to attach because a log file
// could not be written would turn a nicety into an outage.
func newClientLogger(dirs paths.Dirs, cfg *config.Config) (*slog.Logger, io.Closer) {
	level, enabled, err := cfg.Logging()
	if err != nil || !enabled {
		return nil, nil
	}
	if err := dirs.Ensure(); err != nil {
		return nil, nil
	}

	logger, closer, err := cmlog.New(cmlog.Options{
		Enabled: true,
		Level:   level,
		Path:    dirs.ClientLog(),
	})
	if err != nil {
		return nil, nil
	}
	return logger.With("pid", os.Getpid(), "boot", paths.BootID()), closer
}

func runAttach(ctx context.Context, dirs paths.Dirs, opts client.Options) error {
	// Attaching must work whether or not a server happens to be running, which is what
	// lets the user never think about one.
	if err := ensureServer(ctx, dirs); err != nil {
		return err
	}

	tty, err := client.OpenTTY(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}

	res, attachErr := client.Attach(ctx, tty, opts)

	// Restore the terminal before printing anything, so the message is not erased by the
	// reset sequence. Close is idempotent, but calling it once keeps the reset from being
	// emitted twice, which would leave a stray character on screen.
	closeErr := tty.Close()

	if attachErr != nil {
		return attachErr
	}
	switch {
	case res.Exited && res.ExitCode < 0:
		// A negative code means the shim became unreachable rather than the shell reporting a
		// status, so there is no exit code worth showing.
		fmt.Fprintf(os.Stderr, "session %s ended unexpectedly\n", res.Session)
	case res.Exited:
		fmt.Fprintf(os.Stderr, "session %s ended (exit %d)\n", res.Session, res.ExitCode)
	case res.Detached:
		fmt.Fprintf(os.Stderr, "detached from %s\n", res.Session)
	}
	return closeErr
}

func newListCommand(g *globals) *cobra.Command {
	var (
		prefix  string
		asJSON  bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List sessions",
		Long: `List sessions.

--tag filters by the labels set with 'cm tag' or --tag at creation. Repeating it
narrows rather than widens, so asking for two tags lists the sessions that have
both:

  cm ls --tag project=cm --tag role=reviewer

A bare key matches whatever its value is, so --tag project lists everything that
belongs to some project.

Tags group sessions that a name cannot. A per-window session is named by the
server, so it is called "s17" and --prefix has nothing to match on, and a session
belongs to several groupings at once while its name only says one thing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Parsed client-side too, so a typo is reported the same way whether or not a server is
			// running, and the error names the character that was wrong.
			if _, err := tags.ParseSelector(tagArgs); err != nil {
				return err
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.List(ctx, &serverv1.ListRequest{Prefix: prefix, Tags: tagArgs})
				if err != nil {
					return err
				}
				if asJSON {
					return printSessionsJSON(os.Stdout, resp.Sessions)
				}
				return printSessionsTable(os.Stdout, resp.Sessions)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&prefix, "prefix", "", "only sessions whose name starts with this")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"only sessions with this tag, as key or key=value (repeatable, all must match)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

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

func newSendCommand(g *globals) *cobra.Command {
	var (
		newline  bool
		until    string
		timeout  time.Duration
		asJSON   bool
		follow   bool
		raw      bool
		keys     []string
		match    string
		matchRaw bool
	)
	cmd := &cobra.Command{
		Use:   "send <session> [text]...",
		Short: "Send input to a session without attaching",
		Long: `Send input to a session without attaching.

Text is written to the pty exactly as typing it would be, so the session's own
echo and prompt appear in its output.

--key sends a keystroke instead of characters, which text cannot express:

  cm send build --key ctrl-c              # interrupt what is running
  cm send agent --key escape             # leave a program's insert mode
  cm send agent 'yes' --key enter        # type, then press enter
  cm send menu --key down --key down --key enter

Accepts ctrl-c (or c-c, or ^C), alt-x, named keys like enter, tab, escape,
backspace, delete, up, down, left, right, home, end, pageup, pagedown, and f1
through f12. Repeat it to send several in order; keys are sent after any text.

An unknown key name is an error rather than being sent as text. That matters
because the failure is otherwise silent: 'cm send build ctrl-c' types the
characters "ctrl-c" onto the command line and the build keeps running, which reads
as cm having ignored the request.

--key goes through the pty, so it reaches whatever has the terminal in the state a
keypress would. Use 'cm signal' instead when the target is the process rather than
the keyboard: a program that reads ctrl-c as a byte rather than as an interrupt
will not stop for --key, and a session with no foreground job has nothing to
interrupt.

--wait blocks until the session reaches a state after the input lands, which is
how a script or an agent runs something and then reads the result:

  cm send build 'make' --enter --wait idle && cm read build

That is one request, not a send followed by 'cm wait', and the difference is
correctness rather than efficiency. The command starts as soon as the input
arrives, so a fast one finishes before a separate wait could be issued, and that
wait would then block until its timeout having missed what it was waiting for.
The server arms the wait before writing the input.

--follow streams the session's output while the command runs and returns when it
finishes, which is what watching a build looks like without attaching:

  cm send build 'make' --enter --follow

It implies --wait idle, since it has to know when to stop. Escape sequences are
stripped, so the output is plain text: a colour code in a redirected build log is
noise, and this is usually what replaces a send followed by a read where you had to
guess how much to read. Use --raw to keep the sequences.

What you see is everything the session printed, which includes the shell echoing the
line it was sent and the prompt that follows. Those are the session's own output, not
something cm adds, and suppressing them would mean not writing the input through the
pty at all. 'cm read' afterwards renders the screen instead, if the command's output
alone is what you want.

The stream is opened before the input is sent, so nothing the command prints at
the start is missed. Doing it the other way round loses whatever appears before the
follower connects, which for a fast command can be all of it.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("a session name is required")
			}
			// Text is optional only because --key can supply the input instead. Without either there is
			// nothing to send, and silently sending an empty string would look like it worked.
			if len(args) == 1 && len(keys) == 0 {
				return errors.New("nothing to send; give text or use --key")
			}
			return nil
		},
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := paths.ValidateSessionName(name); err != nil {
				return err
			}
			data := strings.Join(args[1:], " ")

			// Keys after any text, so `--key ctrl-c` interrupts what is running and
			// `cm send s 'make' --key enter` types then presses. A caller wanting the other order can
			// make two calls; guessing an interleaving from flag position would be worse than a fixed
			// rule, since cobra does not preserve it.
			if len(keys) > 0 {
				encoded, err := input.ParseKeys(keys)
				if err != nil {
					return err
				}
				data += string(encoded)
			}
			if newline {
				// Carriage return, not newline: a shell at its prompt has the pty in raw
				// mode, where CR is what accept-line is bound to.
				data += "\r"
			}

			if match != "" && until != "" {
				return errors.New("--match and --wait cannot be combined; " +
					"--match waits on output and --wait waits on a state")
			}
			if matchRaw && match == "" {
				return errors.New("--match-raw only applies with --match")
			}
			if match != "" && follow {
				// Refused rather than resolved. --follow stops when its wait resolves, and a match
				// resolving mid-command would cut the stream off partway through output the caller was
				// watching, which reads as truncation rather than as the flag working.
				return errors.New("--match and --follow cannot be combined; " +
					"follow streams until the command ends, which is a different stopping point")
			}

			// --follow implies waiting for idle, since streaming until "whenever" is not a thing: the
			// command has to end for this to return. An explicit --wait still wins, so
			// `--follow --wait exited` watches until the session itself finishes rather than until the
			// command does.
			if follow && until == "" {
				until = "idle"
			}

			var state serverv1.WaitState
			if until != "" {
				var ok bool
				state, ok = waitStates[until]
				if !ok {
					return fmt.Errorf("unknown state %q, want one of idle, busy, blocked, exited", until)
				}
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			if follow {
				if raw {
					warnIfTerminal(os.Stdout)
				}
				cfg, cerr := g.config()
				if cerr != nil {
					return cerr
				}
				logger, closeLog := newClientLogger(dirs, cfg)
				if closeLog != nil {
					defer closeLog.Close()
				}
				return sendAndFollow(cmd.Context(), dirs, name, data, state, timeout, raw, logger)
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.Send(ctx, &serverv1.SendRequest{
					Session:       name,
					Data:          []byte(data),
					WaitUntil:     state,
					Match:         match,
					MatchRaw:      matchRaw,
					WaitTimeoutMs: uint64(timeout.Milliseconds()),
				})
				if err != nil {
					return err
				}
				if resp.GetWait() == nil {
					return nil
				}
				// Described by what was waited for, so a timeout message names the pattern rather than
				// an empty state.
				return reportWait(os.Stdout, os.Stderr, name,
					waitTarget{match: match, until: until}.describe(), resp.GetWait(), asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&newline, "enter", "n", false,
		"append a carriage return so the shell runs the input")
	f.StringArrayVar(&keys, "key", nil,
		"send a key rather than text: ctrl-c, enter, up, f5, alt-x (repeatable, in order)")
	f.StringVar(&until, "wait", "",
		"after sending, wait until the session is idle, busy, blocked, or exited")
	f.StringVar(&match, "match", "",
		"after sending, wait until this text appears in the output")
	f.BoolVar(&matchRaw, "match-raw", false,
		"match the bytes the program emitted rather than the text they rendered to")
	addTimeoutFlag(f, &timeout)
	f.BoolVar(&asJSON, "json", false, "print the wait result as JSON")
	f.BoolVarP(&follow, "follow", "f", false,
		"stream the session's output until the command finishes (implies --wait idle)")
	f.BoolVar(&raw, "raw", false,
		"with --follow, keep escape sequences instead of stripping them")
	return cmd
}

func newInfoCommand(g *globals) *cobra.Command {
	var (
		field   string
		asJSON  bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:   "info [session]",
		Short: "Print one session's details",
		Long: `Print details for a single session.

--field prints one value alone, which is what a script wants: a terminal emulator
opening a new window in a session's directory needs the path with no header,
padding, or parsing.

cwd is empty when the session has reported a directory on another host, since
acting on a remote path locally would be wrong.

--tag prints every session carrying a tag. With --field the values print one per
line, so a selector plus a field is a list of that field across the group:

  cm info --tag run=abc123 --field cwd`,
		// At most one name, since --tag supplies the rest.
		Args:              sessionOrTagArg,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				names, fromSelector, err := sessionTargets(ctx, cl, args, tagArgs)
				if err != nil {
					return err
				}
				resp, err := cl.List(ctx, &serverv1.ListRequest{})
				if err != nil {
					return err
				}
				// Indexed rather than scanned per name, so N sessions cost one pass instead of N.
				byName := make(map[string]*serverv1.Session, len(resp.Sessions))
				for _, s := range resp.Sessions {
					byName[s.Name] = s
				}

				found := make([]*serverv1.Session, 0, len(names))
				for _, name := range names {
					s, ok := byName[name]
					if !ok {
						// Only reachable for a named session: a selector's names came from this same
						// server, though a session could still end in between.
						return fmt.Errorf("session %q not found", name)
					}
					found = append(found, s)
				}

				if asJSON {
					// An array for a selector and a bare object for one named session, so an existing
					// `cm info NAME --json | jq .cwd` keeps working while a selector composes with `.[]`.
					if fromSelector {
						out := make([]sessionJSON, 0, len(found))
						for _, s := range found {
							out = append(out, toSessionJSON(s))
						}
						return writeJSON(os.Stdout, out)
					}
					return writeJSON(os.Stdout, toSessionJSON(found[0]))
				}

				for i, s := range found {
					// A field prints bare even from a selector, so `--tag ... --field cwd` is a list of
					// paths rather than a headed report a caller would have to strip.
					if fromSelector && field == "" {
						if err := writeSessionHeader(os.Stdout, s.Name, i == 0); err != nil {
							return err
						}
					}
					if err := printSessionInfo(os.Stdout, s, field); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&field, "field", "",
		"print only this field: "+strings.Join(SessionFieldNames(), ", "))
	f.StringArrayVar(&tagArgs, "tag", nil,
		"print the sessions with this tag, as key or key=value (repeatable, all must match)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

func newHistoryCommand(g *globals) *cobra.Command {
	var (
		format  string
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:   "history [session]",
		Short: "Print a session's contents, scrollback included",
		Long: `Print a session's contents, including scrollback.

Plain text by default, so it can be piped or paged. --format=vt keeps colors and
styling; --format=html produces styled markup.

The whole thing, always. 'cm read' is the bounded form: it takes --lines, rejoins
soft-wrapped lines, and with --raw gives the emitted bytes of just the tail, which
this cannot do. What only lives here is --format=html, since neither rendered text nor
raw bytes can carry styling as markup.

--tag prints every session carrying a tag, each under a header naming it. Not
available with --format=html, which produces a whole document per session that
cannot be concatenated into valid markup.`,
		// At most one name, since --tag supplies the rest.
		Args:              sessionOrTagArg,
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			var f serverv1.HistoryFormat
			switch format {
			case "plain", "":
				f = serverv1.HistoryFormat_HISTORY_FORMAT_UNSPECIFIED
			case "vt":
				f = serverv1.HistoryFormat_HISTORY_FORMAT_VT
			case "html":
				f = serverv1.HistoryFormat_HISTORY_FORMAT_HTML
			default:
				return fmt.Errorf("unknown format %q, want plain, vt, or html", format)
			}
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				names, fromSelector, err := sessionTargets(ctx, cl, args, tagArgs)
				if err != nil {
					return err
				}
				// Refused rather than emitted, because the output would be broken in a way that is not
				// obvious: each session's HTML is a complete document with its own head and styles, so
				// several in a row is not a document at all. Checked against the count rather than the flag
				// so a selector matching exactly one still works.
				if f == serverv1.HistoryFormat_HISTORY_FORMAT_HTML && len(names) > 1 {
					return fmt.Errorf(
						"--format=html needs one session, but --tag matched %d; "+
							"each session's HTML is a whole document and they cannot be concatenated",
						len(names))
				}

				for i, name := range names {
					if fromSelector {
						if err := writeSessionHeader(os.Stdout, name, i == 0); err != nil {
							return err
						}
					}
					resp, err := cl.History(ctx, &serverv1.HistoryRequest{
						Session: name,
						Format:  f,
					})
					if err != nil {
						return err
					}
					if _, err := os.Stdout.Write(resp.Data); err != nil {
						return err
					}
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&format, "format", "plain", "output format: plain, vt, or html")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"print the sessions with this tag, as key or key=value (repeatable, all must match)")
	return cmd
}

func newCompletionsCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "completions <shell>",
		Short:     "Print a shell completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell %q", args[0])
			}
		},
	}
}
