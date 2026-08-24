package client

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A stopped server stays stopped while a client waits, and the client says so rather than sitting silent.
//
// The cases that need this: restoring a database snapshot, and running a server in the foreground to watch
// it. Both require that nothing starts one behind your back, and both are defeated by a client that helpfully
// brings one back.
func TestServerStarterHonorsADeliberateStop(t *testing.T) {
	started := 0
	s := &serverStarter{
		start:   func(context.Context) error { started++; return nil },
		stopped: func() bool { return true },
	}

	got := s.attempt(context.Background(), 10*time.Second, time.Now())

	if started != 0 {
		t.Errorf("started %d servers, want none while a stop is in effect", started)
	}
	if got != "the server was stopped, waiting for it to come back" {
		t.Errorf("note = %q, want it to say the stop is being honored rather than reading as a failure", got)
	}
}

// Past the grace period a stop is no longer honored, because the alternative failure is silent: an upgrade
// that died between stopping the old server and starting the new one would leave every window waiting for a
// server nobody is going to start.
func TestServerStarterGivesUpOnAStopEventually(t *testing.T) {
	started := 0
	s := &serverStarter{
		start:   func(context.Context) error { started++; return nil },
		stopped: func() bool { return true },
	}

	if got := s.attempt(context.Background(), stoppedGrace, time.Now()); got != "" {
		t.Errorf("note = %q, want nothing: a start was attempted", got)
	}
	if started != 1 {
		t.Errorf("started %d servers, want 1 once the grace period is over", started)
	}
}

// Nothing is started while an outage is still routine, which is what keeps an ordinary restart from having
// two servers race to replace one that is already coming back.
func TestServerStarterWaitsOutTheQuietPeriod(t *testing.T) {
	started := 0
	s := &serverStarter{start: func(context.Context) error { started++; return nil }}

	s.attempt(context.Background(), reconnectQuietPeriod-time.Millisecond, time.Now())

	if started != 0 {
		t.Errorf("started %d servers during the quiet period, want none", started)
	}
}

// A server that refuses to start must not be respawned once a second for the life of the window, and the
// reason it refused is what the user needs: that error carries whatever the server printed before dying.
func TestServerStarterThrottlesAndReportsFailure(t *testing.T) {
	started := 0
	s := &serverStarter{start: func(context.Context) error {
		started++
		return errors.New("server did not become ready within 10s: unknown setting foo")
	}}
	now := time.Now()

	first := s.attempt(context.Background(), 5*time.Second, now)
	if started != 1 {
		t.Fatalf("started %d servers on the first attempt, want 1", started)
	}
	want := "the server is not starting: server did not become ready within 10s: unknown setting foo"
	if first != want {
		t.Errorf("note = %q, want %q", first, want)
	}

	// Inside the interval: no second attempt, and the same news.
	again := s.attempt(context.Background(), 6*time.Second, now.Add(serverStartInterval-time.Millisecond))
	if started != 1 {
		t.Errorf("started %d servers inside the interval, want the attempt throttled to 1", started)
	}
	if again != first {
		t.Errorf("note = %q, want the last failure still reported: %q", again, first)
	}

	// Past it: tried again.
	s.attempt(context.Background(), 11*time.Second, now.Add(serverStartInterval))
	if started != 2 {
		t.Errorf("started %d servers after the interval, want 2", started)
	}
}

// A successful start reports nothing, so the notice keeps showing the wait rather than an error that is not
// there. The client reconnects on its own once the server answers.
func TestServerStarterSaysNothingWhenItWorks(t *testing.T) {
	s := &serverStarter{start: func(context.Context) error { return nil }}

	if got := s.attempt(context.Background(), 5*time.Second, time.Now()); got != "" {
		t.Errorf("note = %q, want nothing after a start that worked", got)
	}
}

// A caller that supplied no starter gets no recovery and no attempt to describe one.
func TestServerStarterDisabledWithoutAStartFunc(t *testing.T) {
	s := &serverStarter{}
	if got := s.attempt(context.Background(), time.Minute, time.Now()); got != "" {
		t.Errorf("note = %q, want nothing when recovery is not configured", got)
	}
}

// A cancelled client must not spawn a server on its way out: the window is closing, and starting one for it
// would leave a process behind that nobody asked for.
func TestServerStarterDoesNotStartWhenCancelled(t *testing.T) {
	started := 0
	s := &serverStarter{start: func(context.Context) error { started++; return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s.attempt(ctx, 10*time.Second, time.Now())

	if started != 0 {
		t.Errorf("started %d servers for a cancelled client, want none", started)
	}
}
