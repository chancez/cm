package server

import (
	"context"
	"errors"
	"fmt"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Signal delivers a signal to a session's shell.
//
// Deliberately not the same thing as writing a control character with Send, even though ctrl-c and
// SIGINT are the same intent. A control character travels through the pty, so the line discipline
// decides what it means: a program that put its terminal in raw mode reads 0x03 as a byte and never
// sees a signal, and a shell with no foreground job has nothing to interrupt. This bypasses all of
// that, which is what makes it the right tool for stopping something that will not stop.
//
// Requires a live session, unlike Tag. A signal is delivered to a process, so a recorded session whose
// shell has gone has nothing to receive it, and reporting that is better than succeeding at nothing.
func (s *Service) Signal(
	ctx context.Context, req *serverv1.SignalRequest,
) (*serverv1.SignalResponse, error) {
	if req.Session == "" {
		return nil, errors.New("a session name is required")
	}
	// Zero is a real value to signal(2) -- it tests whether a process exists rather than sending
	// anything -- but as a request here it means nobody set the field, and silently probing a process
	// when a caller meant to stop one would be the wrong way to be permissive.
	if req.Signal <= 0 {
		return nil, fmt.Errorf("a signal number is required, got %d", req.Signal)
	}

	sess, ok := s.mgr.Get(req.Session)
	if !ok {
		// Distinguishes a session that has ended from one that never existed, since the caller's next
		// move differs: one is finished business, the other is a wrong name.
		if _, err := s.mgr.store.Get(ctx, req.Session); err == nil {
			return nil, fmt.Errorf("session %s has ended, so there is nothing to signal", req.Session)
		}
		return nil, fmt.Errorf("session %s not found", req.Session)
	}

	if err := sess.Signal(ctx, req.Signal, req.ProcessOnly); err != nil {
		// A session whose shell exited between the lookup above and here is not a failure to report as
		// one: the caller wanted the process stopped and it is stopped. Same race `cm run` loses on a
		// fast command, one step later.
		if ended, _ := sess.Ended(); ended {
			s.mgr.log.Info("signalled a session that had already ended",
				"session", req.Session, "signal", req.Signal)
			return &serverv1.SignalResponse{}, nil
		}
		return nil, fmt.Errorf("signalling session %s: %w", req.Session, err)
	}

	s.mgr.log.Info("signalled session",
		"session", req.Session, "signal", req.Signal, "process_only", req.ProcessOnly)
	return &serverv1.SignalResponse{}, nil
}
