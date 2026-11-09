package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/progstatus"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Reported is what a program inside a session said about itself.
//
// Deliberately not agent-specific. cm has no notion of an agent, and does not try to detect one: a state
// a program reports is just a state, and a build script, a test runner, and a coding agent's hook all
// look the same from here. That is the whole design difference from a screen-scraping approach, which has
// to know what each program's UI looks like and chase it as it changes.
type Reported struct {
	// State is the program status protocol's (OSC 7501): "idle", "working", "done", "blocked", or "error".
	// Empty when nothing has reported.
	State string
	// Detail is a short note to show alongside the state, a report's message or failing that its title.
	Detail string
	// Source names the reporter, so one program's report is distinguishable from another's: what `cm report`
	// or OSC 25453 named, or failing that an OSC 7501 report's app.
	Source string
	// Kind says what a blocked program needs: "permission", "question", or "auth". Empty otherwise.
	Kind string
	// Progress is a percentage, or -1 for none. Meaningful only while working or blocked.
	Progress int
	// At is when the report was made. Zero when nothing has reported.
	//
	// Carried so a caller can tell a report made a moment ago from one made before the last server
	// restart. Both are the program's own last word, and a restored one is a claim nobody has retracted
	// rather than a live statement, so the age is the part that separates "blocked, waiting for you" from
	// "said it was blocked hours ago".
	//
	// Deliberately excluded from what counts as a change: see sameStatement.
	At time.Time
}

// sameStatement reports whether two reports say the same thing.
//
// The timestamp is left out on purpose, and the reason is not tidiness. A change is what increments
// reportRuns and what a `cm wait` treats as the program having moved, so a program repeating its current
// state must not count: with the time compared, a hook that reports "busy" on every prompt would satisfy
// every wait issued against it.
func (r Reported) sameStatement(other Reported) bool {
	return r.State == other.State && r.Detail == other.Detail && r.Source == other.Source &&
		r.Kind == other.Kind && r.Progress == other.Progress
}

// Busy reports whether the program says it is still at work: working, or blocked mid-task. What cm used to
// call "busy", and what a listing's busy column and a `cm wait --until busy` mean by it.
func (r Reported) Busy() bool { return r.State == "working" || r.State == "blocked" }

// Finished reports whether the program says it has nothing in progress: idle, done, or failed. What a
// `cm wait --until idle` waits for. done and error count because both are a program that has stopped, and
// a wait for an agent to finish would otherwise hang on the agent saying exactly that.
func (r Reported) Finished() bool {
	return r.State == "idle" || r.State == "done" || r.State == "error"
}

// summarize describes a session by its most urgent record, or reports nothing when there are no records.
func summarize(recs *progstatus.Records) Reported {
	rec, ok := recs.Summary()
	if !ok {
		return Reported{}
	}
	detail := rec.Message
	if detail == "" {
		detail = rec.Title
	}
	source := rec.Source
	if source == "" {
		source = rec.App
	}
	return Reported{
		State:    rec.State,
		Detail:   detail,
		Source:   source,
		Kind:     rec.Kind,
		Progress: rec.Progress,
		At:       rec.At,
	}
}

// recordFromReport turns a report in cm's own vocabulary, from `cm report` or OSC 25453, into a record
// about the session's root.
//
// "busy" is the name cm used before adopting the program status protocol, and becomes "working". The
// source stays free-form, carried beside the app rather than as one: cm does not care what reports, and
// an app is restricted to 32 bytes of [A-Za-z0-9_.+-]. The detail is fitted to what a message may carry
// rather than rejected, so a report that used to be shown does not start vanishing. Kind and progress
// follow the protocol's rules, so `cm report` cannot say what a sequence could not.
func recordFromReport(r Reported, legacy bool) progstatus.Record {
	state := r.State
	if state == "busy" {
		state = "working"
	}
	return progstatus.Record{
		Status: osc.Status{
			State:    state,
			Kind:     r.Kind,
			Progress: r.Progress,
			Message:  fitText(r.Detail, 2048),
		}.Normalized(),
		Source: r.Source,
		Legacy: legacy,
		At:     r.At,
	}
}

// fitText drops control characters, which a report may not carry, and cuts to limit bytes without
// splitting a character.
func fitText(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// reportedStateNames maps the wire enum to what a caller sees.
var reportedStateNames = map[serverv1.ReportedState]string{
	serverv1.ReportedState_REPORTED_STATE_IDLE:    "idle",
	serverv1.ReportedState_REPORTED_STATE_BUSY:    "working",
	serverv1.ReportedState_REPORTED_STATE_BLOCKED: "blocked",
	serverv1.ReportedState_REPORTED_STATE_DONE:    "done",
	serverv1.ReportedState_REPORTED_STATE_ERROR:   "error",
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

	progress := -1
	if req.Progress != nil {
		progress = int(*req.Progress)
	}
	sess.setReported(Reported{
		State:    name,
		Detail:   req.Detail,
		Source:   req.Source,
		Kind:     req.Kind,
		Progress: progress,
	})
	return &serverv1.ReportResponse{}, nil
}
