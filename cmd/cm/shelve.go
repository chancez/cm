package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newShelveCommand(g *globals) *cobra.Command {
	var (
		expire  string
		asJSON  bool
		tagArgs []string
	)
	cmd := &cobra.Command{
		Use:   "shelve <session>...",
		Short: "End sessions, keeping what is needed to restore them as they were",
		Long: `End a session's programs, keeping everything needed to bring it back as it was.

Its record, names, tags, saved output and the resume command a program declared
with 'cm restore-with' are all kept, and the session is listed as shelved:

  cm shelve work
  cm restore work          # or 'cm attach work', which restores it and attaches

This is not SIGTSTP, which is what "suspend" means in a shell (ctrl-z): nothing
is stopped to be continued. The shell and its programs are ended with SIGHUP,
waited for, and killed if they outlive the wait. Restoring starts a new shell in
the recorded directory with the saved output replayed, and the declared resume
command is handled by on_restore as on any revive.

Only a session that saves its output can be shelved, since that is what a
restore comes back to: one started with --persist or matching persist.sessions. A
session that has already ended is marked shelved as it stands, and one that
already is shelved is reported and left alone.

A shelved session does not expire. --expire-after gives it a retention, a Go
duration counted from now, after which it is removed like any ended session.
'cm forget' and 'cm kill' remove one at once; 'cm forget --ended' leaves it.

--tag shelves the sessions carrying a tag, and a selector that matches
nothing is an error.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(tagArgs) > 0 {
				if len(args) > 0 {
					return errors.New("--tag takes no session names")
				}
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("expire-after") {
				if _, err := store.ParseExpiry(expire); err != nil {
					return fmt.Errorf("--expire-after: %w", err)
				}
			}
			if err := validateRefs(args, tagArgs); err != nil {
				return err
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireServerCapability(serverCapabilities(ctx, cl), capability.Shelve,
					"shelve a session"); err != nil {
					return err
				}
				names, err := selectedSessions(ctx, cl, args, tagArgs)
				if err != nil {
					return err
				}
				resp, err := cl.Shelve(ctx, &serverv1.ShelveRequest{Sessions: names, ExpireAfter: expire})
				if err != nil {
					return err
				}
				return reportOutcomes(os.Stdout, outcomes{
					done: resp.Shelved, doneKey: "shelved", doneWord: "shelved",
					noop: resp.AlreadyShelved, noopKey: "already_shelved", noopWord: "already shelved",
					errors: resp.Errors,
				}, asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&expire, "expire-after", "",
		"remove the shelved session after this long, a Go duration or never (default never)")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"shelve the sessions with this tag, as key or key=value (repeatable, all must match)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

func newRestoreCommand(g *globals) *cobra.Command {
	var (
		sizeArg   string
		onRestore string
		asJSON    bool
		tagArgs   []string
	)
	cmd := &cobra.Command{
		Use:   "restore <session>...",
		Short: "Start ended sessions again without attaching",
		Long: `Start an ended session again, without attaching to it.

Any session that saved its output can be restored, however it ended: shelved by
'cm shelve', exited, or dead after a reboot or a crash. All three come back the
same way: a new shell in the recorded directory, the saved output replayed, and
the resume command its program declared, else the command its shell was running
when it ended (cm info --field typed_command), else the program it was started
with, handled by on_restore, the same as any revive. command runs it when the session asked with
--on-restore=command or the program is in safe_commands, and prefills it
otherwise; prefill types it unrun; shell and none ignore it. --on-restore chooses
for this restore.

The session is created at this terminal's size, or at --size COLSxROWS, or at
24x80 when there is no terminal. Its retention, if it had one, is cleared, so
when it ends again it follows the configured schedule.

'cm attach' on an ended session brings it back the same way and attaches. A
running session is reported and left alone. A session that saved no output is
refused, since there is nothing to restore it to.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(tagArgs) > 0 {
				if len(args) > 0 {
					return errors.New("--tag takes no session names")
				}
				return nil
			}
			return cobra.MinimumNArgs(1)(cmd, args)
		},
		ValidArgsFunction: completeRestorableSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateRefs(args, tagArgs); err != nil {
				return err
			}
			size, ok := callerSize(os.Stdout, os.Stderr, os.Stdin)
			if !ok {
				size = conventionalSize
			}
			if sizeArg != "" {
				var err error
				if size, err = parseSize(sizeArg); err != nil {
					return err
				}
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireServerCapability(serverCapabilities(ctx, cl), capability.Shelve,
					"restore a session"); err != nil {
					return err
				}
				names, err := selectedSessions(ctx, cl, args, tagArgs)
				if err != nil {
					return err
				}
				resp, err := cl.Restore(ctx, &serverv1.RestoreRequest{
					Sessions: names,
					Rows:     uint32(size.rows),
					Cols:     uint32(size.cols),
					// What run sends for a session it creates, chosen by the machine the session runs on,
					// so a remote restore does not carry this machine's environment across. See
					// globals.sessionEnvFor.
					Env:       g.sessionEnvFor(os.Environ(), nil),
					OnRestore: onRestore,
				})
				if err != nil {
					return err
				}
				return reportOutcomes(os.Stdout, outcomes{
					done: resp.Restored, doneKey: "restored", doneWord: "restored",
					noop: resp.AlreadyRunning, noopKey: "already_running", noopWord: "already running",
					errors: resp.Errors,
				}, asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&sizeArg, "size", "", "start the session at COLSxROWS rather than this terminal's size")
	f.StringVar(&onRestore, "on-restore", "",
		"what to do with the declared resume command for this restore: shell, none, prefill, or command")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"restore the sessions with this tag, as key or key=value (repeatable, all must match)")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// validateRefs checks the session references and selectors a shelve or restore was given, before anything
// reaches a server.
func validateRefs(args, tagArgs []string) error {
	if err := validateSelectors(tagArgs); err != nil {
		return err
	}
	for _, name := range args {
		if err := paths.ValidateSessionRef(name); err != nil {
			return err
		}
	}
	return nil
}

// selectedSessions resolves what a shelve or restore acts on: the names given, or what a selector matches.
//
// A selector matching nothing is an error, as it is for kill, since it is usually a typo and exiting 0 would
// let a script report success having done nothing.
func selectedSessions(ctx context.Context, cl serverv1.ServerClient, args, tagArgs []string) ([]string, error) {
	if len(tagArgs) == 0 {
		return args, nil
	}
	names, err := resolveSelector(ctx, cl, tagArgs)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no sessions match %s", describeSelectors(tagArgs))
	}
	return names, nil
}

// outcomes is what a shelve or restore reports: what it did, what needed nothing, and what failed, with the
// JSON key and the text wording for each of the first two.
type outcomes struct {
	done     []string
	doneKey  string
	doneWord string
	noop     []string
	noopKey  string
	noopWord string
	errors   map[string]string
}

// reportOutcomes prints what a shelve or restore did, and fails if any session failed.
//
// Text is one line per session, the ones acted on first. JSON is an object with every list spelled out, empty
// rather than null, so a script can iterate each unconditionally, the shape kill and forget print.
func reportOutcomes(w io.Writer, o outcomes, asJSON bool) error {
	done, noop, errs := o.done, o.noop, o.errors
	if done == nil {
		done = []string{}
	}
	if noop == nil {
		noop = []string{}
	}
	if errs == nil {
		errs = map[string]string{}
	}
	if asJSON {
		if err := writeJSON(w, map[string]any{o.doneKey: done, o.noopKey: noop, "errors": errs}); err != nil {
			return err
		}
	} else {
		for _, name := range done {
			fmt.Fprintf(w, "%s %s\n", o.doneWord, name)
		}
		for _, name := range noop {
			fmt.Fprintf(w, "%s is %s\n", name, o.noopWord)
		}
	}
	return reportedErrors(errs, asJSON)
}

// noticeIgnoredCommand says on w that an attach's command will be ignored, when the session it names has
// ended with saved output to come back to.
//
// The server decides, in prepareRestore; this asks the same question beforehand so the notice can be printed
// while stderr is still a line of its own, before the attach takes over the terminal. Silent on any failure
// to ask, since the attach that follows reports a server it cannot reach better than a notice could.
func noticeIgnoredCommand(ctx context.Context, g *globals, w io.Writer, ref string) {
	conn, cl, err := g.connect(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	resp, err := cl.List(ctx, &serverv1.ListRequest{})
	if err != nil {
		return
	}
	if msg := ignoredCommandNotice(resp.Sessions, ref); msg != "" {
		fmt.Fprintln(w, msg)
	}
}

// ignoredCommandNotice is the notice for an attach with a command to ref, or empty when ref names no
// session that a revive brings back as it was.
//
// Restorable is the server's word for that, and an older server sends false, which is right: it ran an
// attach's command on a revive.
func ignoredCommandNotice(sessions []*serverv1.Session, ref string) string {
	s := sessionNamed(sessions, ref)
	if s == nil || !s.Restorable {
		return ""
	}
	return fmt.Sprintf("%s: %s is %s and comes back as it was; ignoring the command after --",
		paths.Name, ref, stateName(s))
}

// namesShelved reports whether ref, a name or an "@id", names a shelved session among these.
func namesShelved(sessions []*serverv1.Session, ref string) bool {
	s := sessionNamed(sessions, ref)
	return s != nil && s.State == serverv1.SessionState_SESSION_STATE_SHELVED
}

// sessionNamed finds the session ref names, by any of its names or by "@id", or nil.
func sessionNamed(sessions []*serverv1.Session, ref string) *serverv1.Session {
	for _, s := range sessions {
		if s.Name == ref || paths.FormatSessionID(s.Id) == ref || slices.Contains(s.Names, ref) {
			return s
		}
	}
	return nil
}
