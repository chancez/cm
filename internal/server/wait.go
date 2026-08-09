package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Wait blocks until a session reaches the requested state, or the deadline passes.
//
// Server-side rather than a client polling loop. The server already consumes the session's output, so
// it can answer from the event that changes the state; a caller sampling `list` would both burn
// requests and be able to miss a transition entirely, which for a command that finishes quickly is the
// common case rather than a corner.
//
// Reports whether the condition was reached rather than returning an error on timeout, because "not yet"
// is an ordinary answer a caller acts on, not a failure. The session's state is returned either way, so
// a caller that timed out learns what it is instead.
func (s *Service) Wait(ctx context.Context, req *serverv1.WaitRequest) (*serverv1.WaitResponse, error) {
	if req.Session == "" {
		return nil, errors.New("a session name is required")
	}
	// Refused rather than combined. "idle and also matching" and "idle or matching" are both plausible
	// readings, so neither is assumed, matching how --lines and --since-commands are handled on read.
	if req.Match != "" && req.Until != serverv1.WaitState_WAIT_STATE_UNSPECIFIED {
		return nil, errors.New("match and a state cannot both be waited for")
	}
	if req.Match == "" && req.Until == serverv1.WaitState_WAIT_STATE_UNSPECIFIED {
		return nil, errors.New("a state to wait for, or text to match, is required")
	}
	if req.MatchRaw && req.Match == "" {
		// Refused rather than ignored: a caller passing it believes it is changing how something is
		// matched, and there is nothing being matched.
		return nil, errors.New("match_raw only applies with match")
	}

	sess, live := s.mgr.Get(req.Session)
	if !live {
		// Not running. An exited session already satisfies a wait for EXITED, which matters because a
		// caller racing a short command should not be told its session does not exist.
		rec, err := s.mgr.store.Get(ctx, req.Session)
		if err != nil {
			return nil, err
		}
		if req.Match != "" {
			// A match waits on output that has not been produced yet, and an ended session will produce
			// none. Said plainly rather than waiting out the timeout, and pointing at the command that can
			// answer from what was saved.
			return nil, fmt.Errorf(
				"session %s has ended, so no further output will match; read its output instead",
				req.Session)
		}
		if req.Until == serverv1.WaitState_WAIT_STATE_EXITED {
			return &serverv1.WaitResponse{
				Satisfied: true,
				State:     sessionState(rec.State),
				ExitCode:  int32(rec.ExitCode),
			}, nil
		}
		// Anything else can never happen now, so say so immediately rather than making the caller wait
		// out a timeout for a session that will never report again.
		return nil, fmt.Errorf("session %s has ended, so it will never become %s",
			req.Session, waitStateName(req.Until))
	}

	// Subscribe before the first check, not after.
	//
	// The order is the whole correctness argument. Checking first and subscribing second leaves a window
	// where the transition happens in between and the wait then blocks until its timeout, having missed
	// the thing it was waiting for. Subscribing first means a change is either already in the seeded
	// value or arrives on the channel.
	if req.Match != "" {
		// A separate loop, because a match wakes on output while a state wait wakes on metadata. See
		// awaitMatch.
		return s.awaitMatch(ctx, sess, req.Match, req.MatchRaw, req.TimeoutMs)
	}

	sub := sess.subscribeMetadata()
	defer sess.unsubscribeMetadata(sub)

	// Wait reports the current state, so a session already in it satisfies immediately: a caller asking
	// "is it idle yet" wants yes, not a wait for the next transition.
	return s.awaitState(ctx, sess, sub, req.Until, req.TimeoutMs, false, 0)
}

// awaitState blocks until a session satisfies until, using an already-registered subscription.
//
// Takes the subscription rather than creating one, because the caller decides when to subscribe and that
// timing is load-bearing. Send subscribes before writing its input; Wait subscribes before its first
// check. Either way the window where a transition could be missed is closed before anything can cause
// one.
//
// afterInput changes what the wait means, and is the difference between the two callers. Wait answers
// "is it in this state", so a session already there satisfies at once. Send answers "did what I just
// sent finish", which is not the same question: a shell at a prompt is already idle, and the command
// takes a few hundred milliseconds to start, so an immediate check succeeds before the command exists
// and the caller then reads output from before its own input. Measured at about 300ms for zsh, which is
// long enough to lose every time rather than occasionally.
//
// sinceRuns is the count of state changes when the input was sent, and is what makes this work for a
// command too fast to observe running. It counts both OSC 133 commands and explicit reports, because an
// agent driven by reports alone produces no commands to count: using only the command count meant a
// `send --wait idle` against such a session never saw a start and burned its whole timeout on work that
// had already finished. The state subscription coalesces to a depth of one, so `true`
// starts and finishes between two reads and collapses into one event: watching for the session to *be*
// busy misses it entirely and the wait then times out saying "waiting for idle; it is idle", which is
// both true and useless. Comparing the counter instead asks whether a command ran at all, which survives
// the coalescing.
func (s *Service) awaitState(
	ctx context.Context,
	sess *Session,
	sub *metaSub,
	until serverv1.WaitState,
	timeoutMs uint64,
	afterInput bool,
	sinceRuns uint64,
) (*serverv1.WaitResponse, error) {
	// started records that the input has visibly done something, so the target state now describes the
	// caller's own command rather than the moment before it.
	//
	// For anything but a wait for idle this is immediate: becoming busy or exiting *is* the change, so
	// there is nothing to disambiguate. Only idle is ambiguous, because it is also where the session
	// begins.
	started := !afterInput || until != serverv1.WaitState_WAIT_STATE_IDLE
	if !started {
		if ended, _ := sess.Ended(); ended {
			// Nothing further will be reported, so waiting for a start that cannot happen would only
			// burn the timeout.
			started = true
		} else if sess.StateRuns() != sinceRuns {
			// A command has already come and gone since the input was sent.
			started = true
		}
	}

	if started && satisfied(sess, until) {
		return waitResult(sess, true), nil
	}

	// A zero timeout waits indefinitely, which is what a caller wanting "however long the build takes"
	// means. The request context still applies, so a caller that goes away is not waited on forever.
	var deadline <-chan time.Time
	if timeoutMs > 0 {
		t := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
		defer t.Stop()
		deadline = t.C
	}

	for {
		select {
		case <-sub.ch:
			// Re-checked against the session rather than trusting the event's contents: metadata
			// coalesces, so an event only means "something changed", and the session holds the current
			// values.
			//
			// Waiting for the session to become busy is what marks the start, rather than waiting for any
			// event: the shell echoes the input it was sent, and that echo is output without being a
			// state change. Treating an event as evidence the command began would resolve on the echo,
			// which happens before the command runs.
			// Either the session is visibly running something, or the counter shows a command has
			// been and gone. The second case is the fast one, and is why a counter exists.
			//
			// The counter covers reports as well as OSC 133 commands, so a program describing its own state
			// marks a start the same way. Deliberately not "a report exists": the session may have been
			// reporting before the input arrived, and treating that as a start would resolve the wait on the
			// state it was already in, handing the caller the previous turn's output.
			if !started && (sess.Command().Running || sess.StateRuns() != sinceRuns) {
				started = true
			}
			if started && satisfied(sess, until) {
				return waitResult(sess, true), nil
			}

		case <-sess.Done():
			// The session ended. That satisfies a wait for EXITED and defeats any other, since nothing
			// further will be reported.
			return waitResult(sess, until == serverv1.WaitState_WAIT_STATE_EXITED), nil

		case <-deadline:
			return waitResult(sess, false), nil

		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// satisfied reports whether a session currently meets a wait condition.
func satisfied(sess *Session, until serverv1.WaitState) bool {
	if ended, _ := sess.Ended(); ended {
		// An ended session is not idle or busy, it is over. Reporting it as idle would let a wait for
		// idle succeed on a session whose shell is gone, which is a different outcome than the caller
		// asked about.
		return until == serverv1.WaitState_WAIT_STATE_EXITED
	}

	// A program's own report wins over what cm derived from the shell.
	//
	// Better evidence, not merely newer: a shell reports one long-running command for the whole life of a
	// coding agent, so the derived state says "busy" from start to finish while the agent moves between
	// working, waiting for an answer, and done. Only the program knows which.
	if r := sess.Reported(); r.State != "" {
		switch until {
		case serverv1.WaitState_WAIT_STATE_IDLE:
			return r.State == "idle"
		case serverv1.WaitState_WAIT_STATE_BUSY:
			return r.State == "busy"
		case serverv1.WaitState_WAIT_STATE_BLOCKED:
			return r.State == "blocked"
		}
		return false
	}

	cmd := sess.Command()
	switch until {
	case serverv1.WaitState_WAIT_STATE_IDLE:
		return !cmd.Running
	case serverv1.WaitState_WAIT_STATE_BUSY:
		return cmd.Running
	case serverv1.WaitState_WAIT_STATE_BLOCKED:
		// Nothing has reported, and blocked cannot be derived from a shell marker: a command is running
		// whether it is computing or waiting at a prompt of its own. Unreachable rather than false, so a
		// caller waiting for it is told instead of timing out.
		return false
	}
	return false
}

// waitResult builds a response describing the session as it is now.
func waitResult(sess *Session, ok bool) *serverv1.WaitResponse {
	cmd := sess.Command()
	r := sess.Reported()
	resp := &serverv1.WaitResponse{
		Satisfied: ok,
		Busy:      cmd.Running,
		Command:   cmd.Command,
		// The outcome, so a caller that waited for idle learns whether the command worked without a
		// second call that could race the next command starting.
		LastCommandExitCode: int32(cmd.ExitCode),
		CommandFinished:     cmd.Exited,
		State:               serverv1.SessionState_SESSION_STATE_RUNNING,
		ReportedState:       r.State,
		ReportedDetail:      r.Detail,
		// How far the server has consumed the session's output, so a follower can tell when it has caught
		// up. The wait says the command finished; it does not say the output has reached the client, and a
		// caller that stops following on the reply truncates whatever is still in flight.
		LastSeq: sess.LastSeq(),
	}
	// A report describes the session better than the derived state, so busy reflects it when present.
	if r.State != "" {
		resp.Busy = r.State != "idle"
	}
	if ended, code := sess.Ended(); ended {
		resp.State = serverv1.SessionState_SESSION_STATE_EXITED
		resp.ExitCode = int32(code)
		if code < 0 {
			// A negative code marks an unreachable shim rather than a status, matching how list reports
			// the same case.
			resp.ExitCode = 0
			resp.State = serverv1.SessionState_SESSION_STATE_DEAD
		}
		// An ended session is neither busy nor running anything, whatever the last report said.
		resp.Busy = false
		resp.Command = ""
	}
	return resp
}

// waitStateName renders a wait state for an error message.
func waitStateName(s serverv1.WaitState) string {
	switch s {
	case serverv1.WaitState_WAIT_STATE_IDLE:
		return "idle"
	case serverv1.WaitState_WAIT_STATE_BUSY:
		return "busy"
	case serverv1.WaitState_WAIT_STATE_EXITED:
		return "exited"
	case serverv1.WaitState_WAIT_STATE_BLOCKED:
		return "blocked"
	}
	return "unspecified"
}
