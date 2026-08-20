package client

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// A client whose server goes away must keep trying, however long the outage lasts and however many
// outages it has already survived.
//
// The bug this replaces killed real sessions. reconnectTimeout was a 30s budget, and it was armed on a
// client's *first* failure and never rearmed on a successful reconnect, so it bounded a client's whole
// lifetime rather than any single outage. A window open for hours had spent it on earlier restarts, and
// the next restart ended it.
//
// Observed: three sessions died together across one `cm server stop` while twenty others reconnected
// fine. The only thing separating them was history. The three had first reconnected hours earlier, at
// 08:31 and 10:18, so their budgets were long gone; every survivor was on a first or recent reconnect.
// The three were also the first to retry, inside the ~180ms before the new server bound its socket, so
// they saw a dial failure where later clients saw a live server.
//
// Driven with an outage far longer than that old budget, since the budget is the thing being removed. A
// test with a short outage passes against the broken code.
func TestClientSurvivesAnOutageLongerThanTheOldBudget(t *testing.T) {
	// The old bound, exceeded on purpose. Not referenced as a constant because the constant is gone;
	// the number is what the regression is about.
	const oldBudget = 30 * time.Second

	dir, err := os.MkdirTemp("", "cmc")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")

	// Drops every attachment as soon as it opens, so each connection ends in a reconnect rather than in
	// a detach. That is what makes this an outage the client has to ride out.
	var svc stubService
	svc.handle = func(_ int, srv serverv1.Server_AttachServer) error {
		return sendOpened(srv, "t", 0)
	}

	stop := serveStubOn(t, socket, &svc)
	tty, opts := attachOpts(t, socket)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, aerr := Attach(ctx, tty, opts)
		done <- aerr
	}()

	// Let it connect once, so it is past the never-connected path and into the retry loop.
	waitFor(t, 5*time.Second, func() bool { return svc.connections() >= 1 })

	// The server disappears for longer than the old budget. Under the old code the client gave up in the
	// middle of this and Attach returned "server did not return within 30s".
	stop()
	time.Sleep(oldBudget + 3*time.Second)

	// Still running, which is the property under test.
	select {
	case aerr := <-done:
		t.Fatalf("Attach() returned %v during a %s outage, want it still retrying.\n"+
			"Giving up discards a terminal whose shell is alive: the shim holds the pty, so a slow server "+
			"is a reason to wait rather than to close a window someone is using.", aerr, oldBudget)
	default:
	}

	// And it reconnects once the server comes back, so waiting is not merely surviving: the session has
	// to actually resume.
	before := svc.connections()
	serveStubOn(t, socket, &svc)
	waitFor(t, 15*time.Second, func() bool { return svc.connections() > before })

	cancel()
	select {
	case aerr := <-done:
		if !errors.Is(aerr, context.Canceled) {
			t.Errorf("Attach() error = %v, want context.Canceled once cancelled", aerr)
		}
	case <-time.After(10 * time.Second):
		t.Error("Attach() did not return after cancellation")
	}
}

// A routine restart logs nothing, and an outage that outlasts the quiet period says so.
//
// Both halves matter and they pull in opposite directions. A restart takes about 450ms and the client
// recovers by itself, so logging every reconnect produced a line per session per restart: with twenty
// sessions that is twenty lines saying nothing, which is what made the log useless for finding a real
// outage. But the client holds the terminal while it waits, so a long silence is indistinguishable from
// a hang, and now that retrying never gives up there is no eventual error to explain it either.
func TestReconnectLoggingIsQuietForARestartAndSpeaksForAnOutage(t *testing.T) {
	t.Run("routine restart is silent", func(t *testing.T) {
		var buf lockedBuffer
		o := outageState{everConnected: true}
		log := slog.New(slog.NewTextHandler(&buf, nil))

		// An outage that resolves well inside the quiet period.
		o.begin(errors.New("connection refused"), 100, 0)
		o.report(log)
		o.end(log)

		if got := buf.String(); got != "" {
			t.Errorf("a restart logged %q, want silence.\n"+
				"A restart takes about 450ms and recovers on its own. One line per session per restart is "+
				"what buried the outages worth reading.", got)
		}
	})

	t.Run("a long outage is reported and its recovery too", func(t *testing.T) {
		var buf lockedBuffer
		log := slog.New(slog.NewTextHandler(&buf, nil))

		// Backdated rather than slept, so the test is deterministic and fast. The quiet period is a
		// property of the elapsed time, which is what is being constructed here.
		o := outageState{everConnected: true}
		o.begin(errors.New("connection refused"), 100, 7)
		o.since = time.Now().Add(-reconnectQuietPeriod - time.Second)

		o.report(log)
		got := buf.String()
		if !strings.Contains(got, "waiting for the server to return") {
			t.Errorf("a %s outage logged %q, want it reported.\n"+
				"With retrying unbounded and the terminal held, silence is the only signal a user gets and it "+
				"looks exactly like a hang.", reconnectQuietPeriod, got)
		}
		// The detail is what makes the line actionable rather than decorative: pending bytes say whether
		// typing was swallowed.
		if !strings.Contains(got, "pending_bytes=7") {
			t.Errorf("outage log = %q, want the held input count in it", got)
		}

		buf.Reset()
		o.end(log)
		if got := buf.String(); !strings.Contains(got, "server returned") {
			t.Errorf("recovery after a reported outage logged %q, want it noted.\n"+
				"A reported outage with no recorded end leaves a reader unable to tell a resolved outage from "+
				"one still going.", got)
		}
	})

	t.Run("recovery is silent when the outage was never reported", func(t *testing.T) {
		var buf lockedBuffer
		log := slog.New(slog.NewTextHandler(&buf, nil))

		o := outageState{everConnected: true}
		o.begin(errors.New("connection refused"), 100, 0)
		o.report(log) // inside the quiet period, so nothing is logged
		o.end(log)

		if got := buf.String(); got != "" {
			t.Errorf("recovery from an unreported outage logged %q, want silence: announcing the end of a "+
				"problem nobody was told about is the same noise in a different place.", got)
		}
	})
}

// The retry interval backs off once an outage stops looking routine.
//
// Needed because retrying no longer ends. A server that is gone for good would otherwise be dialled ten
// times a second for as long as the window stays open, which is a busy loop with a 100ms sleep in it.
func TestReconnectBacksOffAfterTheQuietPeriod(t *testing.T) {
	o := outageState{everConnected: true}
	o.begin(errors.New("refused"), 0, 0)

	// Fresh outage: fast retry, so an ordinary restart is picked up promptly.
	start := time.Now()
	if err := o.sleep(context.Background()); err != nil {
		t.Fatalf("sleep() error = %v", err)
	}
	fast := time.Since(start)
	if fast >= reconnectSlowInterval {
		t.Errorf("a fresh outage slept %s, want about %s: a restart must be noticed quickly",
			fast, reconnectInterval)
	}

	// Same outage, now past the quiet period.
	o.since = time.Now().Add(-reconnectQuietPeriod - time.Second)
	start = time.Now()
	if err := o.sleep(context.Background()); err != nil {
		t.Fatalf("sleep() error = %v", err)
	}
	if slow := time.Since(start); slow < reconnectSlowInterval {
		t.Errorf("a long outage slept %s, want at least %s.\n"+
			"Retrying is unbounded, so without backoff a permanently dead server is dialled ten times a "+
			"second for the life of the window.", slow, reconnectSlowInterval)
	}
}

// sleep must return as soon as the client is cancelled, rather than finishing its interval.
//
// The only exit from an unbounded retry loop, so it is asserted directly as well as through Attach.
func TestReconnectSleepHonoursCancellation(t *testing.T) {
	o := outageState{everConnected: true}
	o.begin(errors.New("refused"), 0, 0)
	// Past the quiet period, so the interval is the long one and a sleep that ignored ctx would be
	// visibly slower than the assertion below.
	o.since = time.Now().Add(-reconnectQuietPeriod - time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := o.sleep(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("sleep() error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed >= reconnectSlowInterval {
		t.Errorf("sleep() took %s to notice cancellation, want it to return immediately", elapsed)
	}
}

// waitFor polls until cond holds, failing the test if it never does.
//
// Polling rather than a fixed sleep: these tests wait for a client to reconnect, and the timing depends
// on the retry interval and on machine load.
func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", limit)
}

// lockedBuffer is a bytes.Buffer safe for a logger written from more than one goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
