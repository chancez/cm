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

	matcher := newOutputMatcher(pattern, raw)

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
				"session", sess.name, "pattern", pattern)
		}

		if matcher.feed(chunk.Data) {
			return waitResult(sess, true), nil
		}
	}
}
