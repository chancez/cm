package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/shquote"
	"github.com/chancez/cm/internal/store"
)

// resumeNextFrame is the frame a declaration carried into a revived session is bound to: whichever opens
// next.
//
// A revived session's declaration was made in a frame of the previous incarnation, which no shell will ever
// close. Left unbound it would outlive the program it describes, so quitting a resumed claude would still
// resume claude after the next reboot. The restore types the command into the new shell, so the next frame
// to open is that command's, and binding to it ends the declaration when the resumed program exits, whether
// or not the program declares again. Not a valid frame id, which is a shell's salt and a counter.
const resumeNextFrame = "next"

// SetResume records the command this session should be restarted with, bound to the innermost command
// frame open now.
//
// Innermost because the declaring process is the program running there: a hook run by claude is claude's
// child, and claude is the command the shell's frame was opened for. With no frame open, which is a shell
// without cm's integration, the declaration is unbound and lasts until it is replaced or cleared.
//
// Ignored once `cm kill --keep` is ending the session, like every other change: what revives is the
// declaration in force when the kill arrived. See dropFramedLocked.
func (s *Session) SetResume(argv []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keepRecord.Load() {
		return
	}
	frame := ""
	if n := len(s.frames); n > 0 {
		frame = s.frames[n-1].id
	}
	s.resume = store.ResumeCommand{Argv: slices.Clone(argv), Frame: frame}
	s.signalRestoreLocked()
}

// ClearResume withdraws the declaration, and says nothing is to be brought back for the frame open now.
//
// Recorded as an empty declaration bound to the innermost frame rather than as no declaration, because the
// typed command would otherwise fill the gap: `cm restore-with --clear` from a `terraform apply` wrapper means
// a revive must not type `terraform apply` back either. It ends with its frame like a declaration, so a
// command run after the program returns is restorable again. With no frame open there is nothing to bind it
// to, and nothing typed either, so it is simply no declaration.
func (s *Session) ClearResume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keepRecord.Load() {
		return
	}
	cleared := store.ResumeCommand{}
	if n := len(s.frames); n > 0 {
		cleared.Frame = s.frames[n-1].id
	}
	if len(s.resume.Argv) == 0 && s.resume.Frame == cleared.Frame {
		return
	}
	s.resume = cleared
	s.signalRestoreLocked()
}

// Resume returns the current declaration.
func (s *Session) Resume() store.ResumeCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return store.ResumeCommand{Argv: slices.Clone(s.resume.Argv), Frame: s.resume.Frame}
}

// Typed returns the command line the session's shell is running, as a revive would bring it back.
func (s *Session) Typed() store.TypedCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.typed
}

// bindResumeLocked binds a declaration waiting for the next frame to the one just opened.
func (s *Session) bindResumeLocked(frame string) {
	if s.resume.Frame != resumeNextFrame || len(s.resume.Argv) == 0 {
		return
	}
	s.resume.Frame = frame
	s.signalRestoreLocked()
}

// OnRestoreFrame returns what the session's own on_restore covers. See store.Session.OnRestoreFrame.
func (s *Session) OnRestoreFrame() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onRestoreFrame
}

// bindGrantLocked binds an on_restore grant waiting for the next frame to the one just opened, which is the
// frame of the command an --in-shell attach or a revive typed into the shell, by the same rule a carried
// declaration binds. It ends with that frame, in dropFramedLocked.
func (s *Session) bindGrantLocked(frame string) {
	if s.onRestoreFrame != resumeNextFrame {
		return
	}
	s.onRestoreFrame = frame
	s.signalRestoreLocked()
}

// noteTypedLocked records the command a frame just opened for, when it is the session's own.
//
// Only the outermost frame, which is the one this session's own shell opened. A frame above it comes from a
// shell further in, written through this pty: the far side of an ssh, a nested `cm attach`, or a subshell. Its
// command belongs to that shell, and typed back into this session's new shell after a reboot it would run
// the remote or nested program here. The outermost frame is the ssh or the `cm attach` itself, which typed
// back reconnects, and that is the right answer for both.
//
// And only in a session whose own program is a shell. One spawned as `kitten ssh host` has no shell here, so
// even its outermost frame is written by the remote shell, when that has cm's integration: a remote `vim`
// recorded as its typed command would be typed into a local shell on a revive and run here, in place of the
// ssh. What brings such a session back is its spawn argv. See runsAShell.
//
// Bound to the frame, and dropped with it by dropFramedLocked, which is the same rule a declaration follows,
// and frozen the same way while a kill that keeps the session is ending it.
func (s *Session) noteTypedLocked(f osc.Frame, outermost bool) {
	if !outermost || !runsAShell(s.record.Command) || s.keepRecord.Load() {
		return
	}
	s.typed = store.TypedCommand{Line: f.Argv, Frame: f.ID}
	s.signalRestoreLocked()
}

// runsAShell reports whether a session spawned with this argv runs a shell of its own: the default shell,
// which an empty argv starts, or an argv that is just a shell. Only such a session's frames are its own.
func runsAShell(command []string) bool {
	return len(command) == 0 || isJustAShell(command)
}

// shells are the programs a spawn argv can name and still be just a shell.
var shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "fish": true, "dash": true, "ksh": true, "mksh": true,
	"tcsh": true, "csh": true, "nu": true,
}

// isJustAShell reports whether a spawn argv starts nothing but an interactive shell, like `zsh -l` or `bash
// -i`, which a revive starts anyway. One running a script with -c, or a file, is a program, as is anything
// that is not a shell. kitty's shell launcher counts as a shell: see isKittyShellLauncher.
func isJustAShell(argv []string) bool {
	if isKittyShellLauncher(argv) {
		return true
	}
	name := filepath.Base(argv[0])
	if !shells[name] && name != filepath.Base(os.Getenv("SHELL")) {
		return false
	}
	for _, arg := range argv[1:] {
		if !strings.HasPrefix(arg, "-") || arg == "-c" {
			return false
		}
	}
	return true
}

// kittyRunShellValued are the options of `kitten run-shell` that take a value, as `--opt value` or
// `--opt=value`, from its --help in kitty 0.51.
var kittyRunShellValued = map[string]bool{
	"--shell-integration": true, "--shell": true, "--env": true, "--cwd": true, "--inject-self-onto-path": true,
}

// isKittyShellLauncher reports whether argv is `kitten run-shell` with nothing but its own options, which is
// kitty's launcher for the user's shell: `kitten run-shell --shell zsh --shell-integration disabled
// --cwd=DIR` starts zsh and nothing else.
//
// A session spawned that way is a shell window. Read as a program, its typed `vim` was never recorded, and a
// revive prefilled the launcher's own argv at the new shell's prompt. An argument that is not one of its
// options is the command run-shell runs before the shell, which makes it a program like any other, and so
// does an option it does not have, since that is not a command line this rule can read.
func isKittyShellLauncher(argv []string) bool {
	if len(argv) < 2 || filepath.Base(argv[0]) != "kitten" || argv[1] != "run-shell" {
		return false
	}
	for i := 2; i < len(argv); i++ {
		arg := argv[i]
		name, _, hasValue := strings.Cut(arg, "=")
		switch {
		case arg == "--":
			return i == len(argv)-1
		case !kittyRunShellValued[name]:
			return false
		case !hasValue:
			// The value is the next argument; one missing it leaves run-shell refusing to start.
			i++
			if i == len(argv) {
				return false
			}
		}
	}
	return true
}

// dropFramedLocked withdraws the declaration and the typed command if their frame is among those closed:
// the program has exited back to a shell, and either one outliving it would resurrect something deliberately
// quit. zellij's set-resurrect-command has exactly that flaw, lasting until the pane closes.
//
// Not while `cm kill --keep` is ending the session. The program did not quit, it was stopped so it could be
// revived, and the hangup is what closes its frame: zsh prints "zsh: hangup" and draws a prompt on the way
// out, so a suspended session lost its declaration 36ms after the kill arrived and revived into a bare
// shell. ClearResume is frozen the same way, since a program's exit hook can withdraw it as it dies.
func (s *Session) dropFramedLocked(closed map[string]bool) {
	resume := s.resume.Frame != "" && s.resume.Frame != resumeNextFrame && closed[s.resume.Frame]
	typed := s.typed.Frame != "" && closed[s.typed.Frame]
	grant := s.onRestoreFrame != "" && s.onRestoreFrame != resumeNextFrame && closed[s.onRestoreFrame]
	if !resume && !typed && !grant {
		return
	}
	if s.keepRecord.Load() {
		s.log.Info("kept what a revive brings back, whose command ended with a kill that keeps the session",
			"session", s.label, "resume_frame", s.resume.Frame, "typed_frame", s.typed.Frame,
			"on_restore_frame", s.onRestoreFrame)
		return
	}
	if resume {
		s.log.Info("dropped a resume command with the command it was declared in",
			"session", s.label, "frame", s.resume.Frame)
		s.resume = store.ResumeCommand{}
	}
	if typed {
		s.typed = store.TypedCommand{}
	}
	if grant {
		// Back to the spawn argv, which for a session whose grant was bound to a frame is the shell: what the
		// user types next is theirs, not what the attach started.
		s.onRestoreFrame = ""
	}
	s.signalRestoreLocked()
}

func (s *Session) signalRestoreLocked() {
	select {
	case s.restoreChanged <- struct{}{}:
	default:
	}
}

// persistRestore writes what a revive of this session would bring back, its declaration and its typed command,
// to the store whenever either changes, until the session ends.
//
// The store is the only copy of the declaration, which arrives by RPC and is in no output stream; the typed
// command is in the output, but a server restart replays only the shim's retained window, which has long lost
// the frame of a program started hours ago. Written off the pump, since a frame is output and a store write
// on the pump would stall it, and coalesced: the writer reads both values when it runs.
func (m *Manager) persistRestore(sess *Session) {
	write := func() {
		r, typed, grant := sess.Resume(), sess.Typed(), sess.OnRestoreFrame()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		u := store.Update{Resume: &r, Typed: &typed, OnRestoreFrame: &grant}
		if err := m.store.Apply(ctx, sess.id, u); err != nil {
			m.log.Warn("recording what a revive brings back failed", "session", sess.id, "error", err)
		}
	}
	for {
		select {
		case <-sess.restoreChanged:
			write()
		case <-sess.Done():
			// One last look, so a change signalled as the session ended is not lost to the select picking
			// this case first.
			select {
			case <-sess.restoreChanged:
				write()
			default:
			}
			return
		}
	}
}

// persistPromptSeen records the shell's first OSC 133 marker in the store, once.
//
// The store is what a restarted server reads it back from. The markers themselves are in the output, but
// adoption replays only the shim's retained window, and an idle shell has none left in it, so without this
// `cm doctor` reported every long-idle shell as having no integration after a server restart. stored is
// what the record already says, which spares the write for a session adopted with it set.
func (m *Manager) persistPromptSeen(sess *Session, stored bool) {
	if stored {
		return
	}
	// Checked first, since a session adopted with markers in its replay is already past the signal.
	if !sess.PromptSeen() {
		select {
		case <-sess.promptSeenChanged:
		case <-sess.Done():
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.Apply(ctx, sess.id, store.Update{PromptSeen: new(true)}); err != nil {
		m.log.Warn("recording shell integration failed", "session", sess.id, "error", err)
	}
}

// SetResume records or clears the command a program in a session should be restarted with.
//
// Only for a running session: a declaration describes the program running now, so there is nothing to
// declare for a session whose processes are gone.
func (s *Service) SetResume(ctx context.Context, req *serverv1.SetResumeRequest) (*serverv1.SetResumeResponse, error) {
	if req.Session == "" {
		return nil, errors.New("a session name is required")
	}
	if !req.Clear && len(req.Argv) == 0 {
		return nil, errors.New("a command is required")
	}
	id, err := s.mgr.Resolve(ctx, req.Session)
	if err != nil {
		return nil, err
	}
	sess, ok := s.mgr.Get(id)
	if !ok {
		return nil, fmt.Errorf("session %s is not running", req.Session)
	}
	if req.Clear {
		sess.ClearResume()
	} else {
		sess.SetResume(req.Argv)
	}
	return &serverv1.SetResumeResponse{}, nil
}

// typedCommand is a command typed into a new session's shell once it prompts: what a revive brings back, or
// the command of an attach asking for it to run in the shell. See OpenOptions.InShell.
type typedCommand struct {
	argv []string
	// line is typed verbatim instead of argv, for a typed command, which is a line the shell reported rather
	// than an argv and is typed back for the same shell to parse again.
	line string
	// run submits the line. Without it the command sits on the prompt for the user to run, edit, or clear.
	run bool
	// declared says this is the session's declared resume command, which the new record carries forward
	// bound to the next frame. Nothing else is carried: a typed or recorded command is recorded again by the
	// frame its typed line opens.
	declared bool
	// granted says it may run without the allowlist because the session's own on_restore or the reviving
	// request's said so, which the new record carries forward as a grant bound to the frame its typed line
	// opens. See keptGrantCovers.
	granted bool
}

// restoreWhatRan decides what a revive brings back when the caller named no command, in order:
//
//  1. the declared resume command, from `cm restore-with`, since the program said how to bring itself back;
//     an empty declaration from `restore-with --clear` stops here with nothing;
//  2. else the typed command, the line the shell was running, in a session that runs a shell of its own;
//  3. else the spawn argv, when it is a program rather than just a shell;
//  4. else a shell, which is what a revive starts anyway.
//
// The policy for each is the one a recorded command always had: under on_restore = "command" it runs when
// the session asked for command or the program is in safe_commands, and is prefilled otherwise; "prefill"
// prefills; "shell" brings nothing back. Prefilled rather than dropped, because the user asked for the
// command back and the safe form of that is one keypress away from it.
//
// "The session asked" is asked, the reviving request's own --on-restore, which covers whatever this revive
// brings back, or else the session's kept one, which covers only what the attach that set it started. See
// keptGrantCovers.
func (m *Manager) restoreWhatRan(
	opts OpenOptions, rec store.Session, action, asked RestoreAction,
) OpenOptions {
	if action != RestorePrefill && action != RestoreCommand {
		return opts
	}
	explicit := func(frame string, spawn bool) bool {
		if asked != "" {
			return asked == RestoreCommand
		}
		return rec.OnRestore == string(RestoreCommand) && keptGrantCovers(rec, frame, spawn)
	}
	switch {
	case len(rec.Resume.Argv) > 0:
		opts.typeOnStart = m.restoreDeclared(rec.Resume.Argv, action, explicit(rec.Resume.Frame, false))
	case rec.Resume.Frame != "" && rec.Resume.Frame != resumeNextFrame:
		// Cleared in a frame that was still open, which says nothing is to come back for it.
	case rec.Typed.Line != "" && runsAShell(rec.Command):
		// A session that is a program, like an ssh, comes back as that program rather than as a line its far
		// side ran. Such a session records no typed command now, but a record kept before that rule has one.
		opts.typeOnStart = m.restoreTyped(rec.Typed.Line, action, explicit(rec.Typed.Frame, false))
	case len(rec.Command) > 0 && !isJustAShell(rec.Command):
		argv := slices.Clone(rec.Command)
		// Exec'd in place when it runs, as a recorded command always was, rather than typed: it is what the
		// session was, not something its shell ran.
		if action == RestoreCommand && (explicit("", true) || m.safeToRerun(argv)) {
			opts.Command = argv
		} else {
			opts.typeOnStart = &typedCommand{argv: argv}
		}
	}
	return opts
}

// keptGrantCovers reports whether a session's own on_restore, kept with its record, covers a command: one
// bound to frame, a declaration or a typed command, or with spawn the spawn argv.
//
// It covers what the attach that set it started, not what the user runs in the session later. Before it was
// scoped, a session created with --in-shell --on-restore=command ran any line typed into its shell since,
// on every revive, without the allowlist. From a plain attach that is the spawn argv, the session itself,
// which is the ssh case the kept value exists for; and a declaration bound to no frame in a session that is
// a program, since a session with no shell of its own has no frames, so only that program or something it
// runs can have made it. From an --in-shell attach, or a revive that typed a command back, it is the command
// typed into the shell and a declaration it made, both bound to its frame, until that frame closes.
func keptGrantCovers(rec store.Session, frame string, spawn bool) bool {
	if rec.OnRestoreFrame == "" {
		return spawn || (frame == "" && !runsAShell(rec.Command))
	}
	return !spawn && frame == rec.OnRestoreFrame
}

// restoreDeclared decides how a revive restores a declared command.
//
// Typed into a shell rather than exec'd as the session's command, as kitty does for cmd_at_shell_startup:
// quitting a resumed program should leave the window at a prompt rather than close it. Typing also means
// the shell's preexec hook opens a frame for it, which is what the carried declaration binds to.
func (m *Manager) restoreDeclared(argv []string, action RestoreAction, explicit bool) *typedCommand {
	run := action == RestoreCommand && (explicit || m.safeToRerun(argv))
	return &typedCommand{argv: slices.Clone(argv), run: run, declared: true, granted: explicit}
}

// restoreTyped decides how a revive restores a typed command, typed back verbatim like a declared one.
//
// It runs only when it is a plain program invocation, whatever the allowlist or the session's own
// --on-restore says, since the line goes to a shell that runs all of it: with `sleep` allowlisted, `sleep 1;
// rm -rf x` would otherwise run in full. Anything else is prefilled, one keypress away. safe_commands is
// matched against the program word as the shell would read it, quotes removed. A line at the length the
// shells and the parser truncate to is never run either, since its end may have been cut off.
func (m *Manager) restoreTyped(line string, action RestoreAction, explicit bool) *typedCommand {
	argv, plain := plainInvocation(line)
	run := action == RestoreCommand && plain && (explicit || m.safeToRerun(argv)) &&
		len(line) < osc.MaxFrameArgv
	return &typedCommand{line: line, run: run, granted: explicit}
}

// plainInvocation splits a typed line into its words when it is nothing but a program and its arguments,
// reporting false for anything a shell would read as more than that.
//
// Words are separated by spaces and tabs, and may be quoted with '...' or "...". Refused, so the line is only
// ever prefilled:
//
//   - operators and grouping: ; | & < > ( ) { }, which covers &&, ||, >> and here-documents;
//   - expansions: $ and `, outside quotes or inside double quotes;
//   - globs and their zsh relatives: * ? [ outside quotes, and # ^ which zsh's extended globbing reads as
//     patterns;
//   - history: ! anywhere outside single quotes, and a leading ^, which is bash and zsh quick substitution;
//   - a backslash outside single quotes, which covers a continuation, and a newline anywhere;
//   - a ~ that is not a lone word or the start of ~/, so ~user and zsh's a~b exclusion are refused;
//   - a word starting with =, which zsh expands to a command's path;
//   - a first word that assigns, VAR=value;
//   - an unbalanced quote, and an empty line.
//
// What it cannot see is what a word means to the shell: an alias or a function named like an allowlisted
// program runs as itself. The allowlist was never more than a convenience for that reason.
func plainInvocation(line string) ([]string, bool) {
	if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimLeft(line, " \t"), "^") {
		return nil, false
	}
	var (
		words    []string
		word     strings.Builder
		inWord   bool
		wordRaw  strings.Builder // the word as typed, quotes included, for the checks on its start
		quote    rune
		finished = func() bool {
			raw := wordRaw.String()
			if strings.HasPrefix(raw, "=") ||
				(strings.HasPrefix(raw, "~") && raw != "~" && !strings.HasPrefix(raw, "~/")) {
				return false
			}
			if len(words) == 0 && isAssignment(raw) {
				return false
			}
			words = append(words, word.String())
			word.Reset()
			wordRaw.Reset()
			inWord = false
			return true
		}
	)
	for _, r := range line {
		if r == '\n' || r == '\r' {
			return nil, false
		}
		switch quote {
		case '\'':
			wordRaw.WriteRune(r)
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		case '"':
			wordRaw.WriteRune(r)
			switch r {
			case '"':
				quote = 0
			case '$', '`', '\\', '!':
				return nil, false
			default:
				word.WriteRune(r)
			}
			continue
		}
		switch r {
		case ' ', '\t':
			if inWord && !finished() {
				return nil, false
			}
			continue
		case '\'', '"':
			quote = r
			inWord = true
			wordRaw.WriteRune(r)
			continue
		case ';', '|', '&', '<', '>', '(', ')', '{', '}', '$', '`', '*', '?', '[', '!', '\\', '#', '^':
			return nil, false
		case '~':
			if inWord && wordRaw.Len() > 0 {
				// Inside a word, which zsh's extended globbing reads as an exclusion.
				return nil, false
			}
		}
		inWord = true
		word.WriteRune(r)
		wordRaw.WriteRune(r)
	}
	if quote != 0 {
		return nil, false
	}
	if inWord && !finished() {
		return nil, false
	}
	return words, len(words) > 0
}

// isAssignment reports whether a word as typed is VAR=value, which a shell reads as setting a variable for the
// command that follows rather than as a program.
func isAssignment(raw string) bool {
	name, _, found := strings.Cut(raw, "=")
	if !found || name == "" {
		return false
	}
	for i, r := range name {
		if r != '_' && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && (i == 0 || !(r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// safeToRerun asks the configured allowlist about a command.
func (m *Manager) safeToRerun(argv []string) bool {
	return m.persist.CommandIsSafeToRerun != nil && m.persist.CommandIsSafeToRerun(argv)
}

// typePromptWait bounds how long a new shell is waited for to prompt before typing anyway.
//
// Typing before the prompt is not lost, since the tty holds it until the line editor reads it, but an rc
// file that reads stdin would take it. So the prompt mark is waited for when the shell sends one, and a
// shell that sends none gets the typeahead.
const typePromptWait = 10 * time.Second

// typeCommand types a command into a new session's shell once it is at its first prompt.
func (m *Manager) typeCommand(sess *Session, t typedCommand) {
	deadline := time.NewTimer(typePromptWait)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
wait:
	for !sess.PromptSeen() {
		select {
		case <-tick.C:
		case <-deadline.C:
			m.log.Info("no prompt mark from a new shell, typing its command anyway",
				"session", sess.id, "waited", typePromptWait)
			break wait
		case <-sess.Done():
			return
		}
	}

	line := t.line
	if line == "" {
		line = shquote.Join(t.argv, isFish(sess.record.Command))
	}
	var enter []byte
	if t.run {
		enter = []byte("\r")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writeInputThenEnter(ctx, sess, []byte(line), enter); err != nil {
		m.log.Warn("typing a command into a new shell failed", "session", sess.id, "error", err)
		return
	}
	m.log.Info("typed a command into a new shell", "session", sess.id, "run", t.run)
}

// isFish reports whether a session's shell is fish, whose quoting differs from POSIX inside single quotes.
//
// From the recorded command, or the server's SHELL when the session runs the default: the shim starts the
// default shell from the environment it inherits from the server.
func isFish(command []string) bool {
	if len(command) > 0 {
		return filepath.Base(command[0]) == "fish"
	}
	if f := strings.Fields(os.Getenv("SHELL")); len(f) > 0 {
		return filepath.Base(f[0]) == "fish"
	}
	return false
}

// PromptSeen reports whether the shell has sent a prompt marker.
func (s *Session) PromptSeen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promptSeen
}
