package server

import (
	"context"
	"errors"
	"fmt"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Reported is what a program inside a session said about itself.
//
// Deliberately not agent-specific. cm has no notion of an agent, and does not try to detect one: a state
// a program reports is just a state, and a build script, a test runner, and a coding agent's hook all
// look the same from here. That is the whole design difference from a screen-scraping approach, which has
// to know what each program's UI looks like and chase it as it changes.
type Reported struct {
	// State is "idle", "busy", or "blocked". Empty when nothing has reported.
	State string
	// Detail is a short note to show alongside the state.
	Detail string
	// Source names the reporter, so one program's report is distinguishable from another's.
	Source string
}

// reportedStateNames maps the wire enum to what a caller sees.
var reportedStateNames = map[serverv1.ReportedState]string{
	serverv1.ReportedState_REPORTED_STATE_IDLE:    "idle",
	serverv1.ReportedState_REPORTED_STATE_BUSY:    "busy",
	serverv1.ReportedState_REPORTED_STATE_BLOCKED: "blocked",
}

// Report records what a program in a session says it is doing.
//
// The state a program reports takes precedence over what cm derives from OSC 133, because it is better
// evidence: a shell reports one long-running command for the whole life of a coding agent, while the
// agent itself knows whether it is working, waiting for an answer, or finished.
//
// Blocked is the case that cannot be derived at all. A shell marks a command as running whether it is
// computing or sitting at a prompt of its own, so nothing outside the program can tell the difference.
// That is why the alternative is scraping the screen for each program's approval dialog, which herdr does
// and has to keep updating as those dialogs change.
func (s *Service) Report(ctx context.Context, req *serverv1.ReportRequest) (*serverv1.ReportResponse, error) {
	if req.Session == "" {
		return nil, errors.New("a session name is required")
	}
	if req.State == serverv1.ReportedState_REPORTED_STATE_UNSPECIFIED {
		return nil, errors.New("a state is required")
	}

	id, err := s.mgr.Resolve(ctx, req.Session)
	if err != nil {
		return nil, err
	}
	sess, ok := s.mgr.Get(id)
	if !ok {
		return nil, fmt.Errorf("session %s is not running", req.Session)
	}

	if req.State == serverv1.ReportedState_REPORTED_STATE_CLEAR {
		sess.setReported(Reported{})
		return &serverv1.ReportResponse{}, nil
	}

	name, known := reportedStateNames[req.State]
	if !known {
		return nil, fmt.Errorf("unknown state %v", req.State)
	}

	sess.setReported(Reported{
		State:  name,
		Detail: req.Detail,
		Source: req.Source,
	})
	return &serverv1.ReportResponse{}, nil
}
