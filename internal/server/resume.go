package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"

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
