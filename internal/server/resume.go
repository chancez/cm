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
func (s *Session) SetResume(argv []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame := ""
	if n := len(s.frames); n > 0 {
		frame = s.frames[n-1].id
	}
	s.resume = store.ResumeCommand{Argv: slices.Clone(argv), Frame: frame}
	s.signalResumeLocked()
}

// ClearResume withdraws the declaration.
func (s *Session) ClearResume() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.resume.Argv) == 0 {
		return
	}
	s.resume = store.ResumeCommand{}
	s.signalResumeLocked()
}

// Resume returns the current declaration.
func (s *Session) Resume() store.ResumeCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return store.ResumeCommand{Argv: slices.Clone(s.resume.Argv), Frame: s.resume.Frame}
}

// bindResumeLocked binds a declaration waiting for the next frame to the one just opened.
func (s *Session) bindResumeLocked(frame string) {
	if s.resume.Frame != resumeNextFrame || len(s.resume.Argv) == 0 {
		return
	}
	s.resume.Frame = frame
	s.signalResumeLocked()
}

// dropResumeLocked withdraws the declaration if its frame is among those closed: the program that made it
// has exited back to a shell, and a declaration that outlived it would resurrect something deliberately
// quit. zellij's set-resurrect-command has exactly that flaw, lasting until the pane closes.
func (s *Session) dropResumeLocked(closed map[string]bool) {
	if s.resume.Frame == "" || s.resume.Frame == resumeNextFrame || !closed[s.resume.Frame] {
		return
	}
	s.log.Info("dropped a resume command with the command it was declared in",
		"session", s.label, "frame", s.resume.Frame)
	s.resume = store.ResumeCommand{}
	s.signalResumeLocked()
}

func (s *Session) signalResumeLocked() {
	select {
	case s.resumeChanged <- struct{}{}:
	default:
	}
}

// persistResume writes a session's declaration to the store whenever it changes, until the session ends.
//
// The store is the only copy: a declaration arrives by RPC and is in no output stream. Written off the pump,
// since a frame closing is output and a store write on the pump would stall it.
func (m *Manager) persistResume(sess *Session) {
	for {
		select {
		case <-sess.resumeChanged:
			r := sess.Resume()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := m.store.Apply(ctx, sess.id, store.Update{Resume: &r}); err != nil {
				m.log.Warn("recording a resume command failed", "session", sess.id, "error", err)
			}
			cancel()
		case <-sess.Done():
			// One last look, so a change signalled as the session ended is not lost to the select picking
			// this case first.
			select {
			case <-sess.resumeChanged:
				r := sess.Resume()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := m.store.Apply(ctx, sess.id, store.Update{Resume: &r}); err != nil {
					m.log.Warn("recording a resume command failed", "session", sess.id, "error", err)
				}
				cancel()
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

// typedRestore is a declared resume command a revive types into the new shell.
type typedRestore struct {
	argv []string
	// run submits the line. Without it the command sits on the prompt for the user to run, edit, or clear.
	run bool
}

// restoreDeclared decides how a revive restores a declared command.
//
// Typed into a shell rather than exec'd as the session's command, as kitty does for cmd_at_shell_startup:
// quitting a resumed program should leave the window at a prompt rather than close it. Typing also means
// the shell's preexec hook opens a frame for it, which is what the carried declaration binds to.
//
// Running it is the one outcome that executes something the user did not type just now, so it takes what
// re-running the recorded command takes: asking for it on this session, or the program being on the
// allowlist. Otherwise it is prefilled rather than dropped, which is what on_restore = "command" without
// the allowlist would otherwise mean: the user asked for the command back, and the safe form of that is
// one keypress away from it.
func (m *Manager) restoreDeclared(argv []string, action RestoreAction, explicit bool) *typedRestore {
	run := false
	if action == RestoreCommand {
		run = explicit || (m.persist.CommandIsSafeToRerun != nil && m.persist.CommandIsSafeToRerun(argv))
	}
	return &typedRestore{argv: slices.Clone(argv), run: run}
}

// restorePromptWait bounds how long a revive waits for the new shell to prompt before typing anyway.
//
// Typing before the prompt is not lost, since the tty holds it until the line editor reads it, but an rc
// file that reads stdin would take it. So the prompt mark is waited for when the shell sends one, and a
// shell that sends none gets the typeahead.
const restorePromptWait = 10 * time.Second

// typeRestore types a declared command into a revived session's shell once it is at its first prompt.
func (m *Manager) typeRestore(sess *Session, t typedRestore) {
	deadline := time.NewTimer(restorePromptWait)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
wait:
	for !sess.PromptSeen() {
		select {
		case <-tick.C:
		case <-deadline.C:
			m.log.Info("no prompt mark from a revived shell, typing its resume command anyway",
				"session", sess.id, "waited", restorePromptWait)
			break wait
		case <-sess.Done():
			return
		}
	}

	line := shquote.Join(t.argv, isFish(sess.record.Command))
	var enter []byte
	if t.run {
		enter = []byte("\r")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := writeInputThenEnter(ctx, sess, []byte(line), enter); err != nil {
		m.log.Warn("typing a resume command failed", "session", sess.id, "error", err)
		return
	}
	m.log.Info("restored a declared resume command", "session", sess.id, "run", t.run)
}

// isFish reports whether a session's shell is fish, whose quoting differs from POSIX inside single quotes.
//
// From the recorded command, or the server's SHELL when the session runs the default: the shim starts the
// default shell from the environment it inherits from the server.
func isFish(command string) bool {
	sh := command
	if sh == "" {
		sh = os.Getenv("SHELL")
	}
	if f := strings.Fields(sh); len(f) > 0 {
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
