package client

import (
	"context"
	"time"
)

const (
	// serverStartInterval is the shortest gap between attempts to start a replacement server.
	//
	// Well above the roughly 450ms a server takes to come up, so a client that starts one has time to see
	// it before trying again, and a server that refuses to start is not respawned in a loop. The retry loop
	// itself dials once a second during an outage, so this throttles only the starting.
	serverStartInterval = 5 * time.Second
	// stoppedGrace is how long a deliberate stop is honored while a client is still attached.
	//
	// Bounded rather than indefinite because the alternative failure is silent and worse: an upgrade that
	// dies between stopping the old server and starting the new one would leave every window waiting for a
	// server nobody is going to start. Two minutes is long enough to restore a database snapshot, which is
	// the case that needs the stop respected, and that case has no clients attached anyway: the recipe
	// stops the sessions first.
	stoppedGrace = 2 * time.Minute
)

// serverStarter starts a replacement server for a client whose own has gone.
//
// The mechanics live in the caller because spawning a process and knowing where the runtime directory is
// are not this package's business, and because the policy below is the part worth testing: when to try,
// when to leave a stop alone, and what to say when a start fails.
type serverStarter struct {
	// start launches a server and waits for it to accept connections. Nil disables recovery entirely,
	// which is what a follower and any caller that did not ask for it get.
	start func(context.Context) error
	// stopped reports whether a server was stopped on purpose, which suppresses starting one. Nil means
	// nothing is suppressed.
	stopped func() bool

	// lastAttempt is when a start was last tried, so a refusing server is not respawned in a loop.
	lastAttempt time.Time
	// lastErr is why the last attempt failed, shown on screen because a server that will not start is
	// otherwise indistinguishable from one that is slow to arrive.
	lastErr error
}

// attempt starts a server if this outage warrants one, and reports what to tell the user.
//
// The return is a whole phrase rather than a fragment, because the two cases read nothing alike: a stop
// being honored is not a failure, and pasting it after "not starting" said the opposite of what it meant.
// Empty means there is nothing to say beyond the wait itself.
func (s *serverStarter) attempt(ctx context.Context, waited time.Duration, now time.Time) (note string) {
	if s.start == nil || waited < reconnectQuietPeriod {
		return ""
	}

	// A stop that is being honored, which is not a failure and says so differently: the server is not
	// coming back until someone brings it back, and a client silently waiting for that is the confusion
	// this whole change is about.
	if s.stopped != nil && s.stopped() && waited < stoppedGrace {
		return "the server was stopped, waiting for it to come back"
	}

	if !s.lastAttempt.IsZero() && now.Sub(s.lastAttempt) < serverStartInterval {
		// Between attempts, so the last failure is still the news.
		return startFailure(s.lastErr)
	}
	s.lastAttempt = now
	s.lastErr = ctx.Err()
	if s.lastErr == nil {
		s.lastErr = s.start(ctx)
	}
	return startFailure(s.lastErr)
}

// startFailure renders a failed start for the notice, or empty when the last attempt worked.
//
// The error carries whatever the server printed before dying, which is the part worth showing: a server
// that refuses to start is otherwise indistinguishable from one that is slow to arrive, and that was a real
// outage, where a single unknown config setting stopped a replacement from ever coming up.
func startFailure(err error) string {
	if err == nil {
		return ""
	}
	return "the server is not starting: " + err.Error()
}
