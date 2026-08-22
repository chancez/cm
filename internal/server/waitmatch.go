package server

import (
	"context"
	"errors"
	"time"

	"github.com/chancez/cm/internal/seqlog"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// awaitMatch blocks until a pattern appears in the session's output, or the deadline passes.
//
// Its own loop rather than a branch inside awaitState, because the two wait on different things.
// awaitState wakes on metadata changes -- title, cwd, whether a command is running -- and re-reads the
// session's current values. Output is not part of that: a session can print for an hour without any
// metadata changing, so a match folded into that loop would only be evaluated when something unrelated
// happened to change.
//
// Subscribed before the first read, for the same reason awaitState subscribes before its first check: the
// gap between checking and subscribing is where the thing being waited for slips through.
func (s *Service) awaitMatch(
	ctx context.Context,
	sess *Session,
	pattern string,
	raw bool,
	timeoutMs uint64,
) (*serverv1.WaitResponse, error) {
	reader := sess.SubscribeOutput()
	defer reader.Close()

	// No echo to skip: a bare wait sent no input, so everything arriving is the session's own output.
	return s.matchOn(ctx, sess, reader, pattern, raw, timeoutMs, 0)
}

// matchOn scans a subscription until the pattern appears, the session ends, or the deadline passes.
//
// Takes the reader rather than opening one, because when the subscription is opened is load-bearing and
// differs between callers. A bare wait subscribes at the moment it is asked; a send subscribes before
// writing its input, so a command that prints and finishes immediately is still caught. Neither ordering
// belongs to this function, so neither is decided here.
func (s *Service) matchOn(
	ctx context.Context,
	sess *Session,
	reader *seqlog.Reader,
	pattern string,
	raw bool,
	timeoutMs uint64,
	skipEcho int,
) (*serverv1.WaitResponse, error) {
	matcher := newOutputMatcher(pattern, raw)
	if skipEcho > 0 {
		matcher.skipEcho(skipEcho)
	}

	// The deadline is enforced by cancelling the read, since Next blocks until output arrives and a
	// session that has gone quiet would otherwise hold the wait past its timeout.
	//
	// A zero timeout waits indefinitely, which is what "however long the build takes" means. The request
	// context still applies, so a caller that goes away is not waited on forever.
	readCtx := ctx
	if timeoutMs > 0 {
		var cancel context.CancelFunc
		readCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
		defer cancel()
	}

	for {
		chunk, err := reader.Next(readCtx)
		if err != nil {
			switch {
			case errors.Is(err, seqlog.ErrClosed):
				// The session ended. Whatever it printed has been fed already, so the latched result is
				// the answer: a pattern that appeared in the final output still counts.
				return waitResult(sess, matcher.matched()), nil
			case errors.Is(err, context.DeadlineExceeded):
				// The timeout, which is an ordinary answer rather than a failure, matching how a state
				// wait reports not-yet.
				return waitResult(sess, matcher.matched()), nil
			case ctx.Err() != nil:
				// The caller went away, which is different from the wait not being satisfied.
				return nil, ctx.Err()
			default:
				return nil, err
			}
		}

		// A gap means output was dropped before this reader saw it, so the pattern may have been in what
		// was lost. Reported through the wait's own result rather than as an error: the caller asked
		// whether the text appeared, and the honest answer is that it cannot be known from here.
		//
		// Only reachable for a session producing output faster than this loop consumes it, which for a
		// matcher doing a substring scan means a session flooding megabytes. Logged so it is not silent.
		if chunk.Gap {
			s.mgr.log.Warn("output was dropped while waiting for a match, so the pattern may have been missed",
				"session", sess.label, "pattern", pattern)
		}

		if matcher.feed(chunk.Data) {
			return waitResult(sess, true), nil
		}
	}
}

// sendAndAwaitMatch writes input and waits for a pattern in what it causes.
//
// The subscription is opened before the write, which is the whole reason this lives beside Send rather
// than being composed from two calls. A command runs as soon as its input lands, so one that prints and
// finishes faster than a second request could arrive would have its output already past by the time a
// separate wait subscribed -- and that wait would then block until its timeout, having missed what it was
// created for. Arming first closes the window rather than narrowing it.
//
// This is the ordering trap `cm wait --match` cannot avoid from outside, which is why a caller composing
// the two has to start the wait first.
func (s *Service) sendAndAwaitMatch(
	ctx context.Context, sess *Session, req *serverv1.SendRequest,
) (*serverv1.SendResponse, error) {
	reader := sess.SubscribeOutput()
	defer reader.Close()

	// Taken before the input for the same reason the state path takes it: a caller warning that a wait may
	// never resolve wants to know whether the shell was already reporting, and this call's own command
	// would otherwise make every session look like it reports.
	runsBefore := sess.StateRuns()

	if err := writeInputThenEnter(ctx, sess, req.Data, req.Enter); err != nil {
		return nil, err
	}

	// The shell echoes the input back, and that echo contains the command, so a pattern naming anything in
	// the command would match the echo rather than the output. Skipping the bytes just written steps over
	// it. This is the match-wait counterpart of the afterInput qualifier a state wait uses.
	//
	// Both writes are counted, since the echo covers the submitting keypress as well as the text.
	wait, err := s.matchOn(
		ctx, sess, reader, req.Match, req.MatchRaw, req.WaitTimeoutMs, len(req.Data)+len(req.Enter))
	if err != nil {
		return nil, err
	}
	return &serverv1.SendResponse{Wait: wait, ShellReports: runsBefore > 0}, nil
}
