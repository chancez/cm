package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// waitClient answers Wait from a per-session script, so multi-session behavior can be tested without
// real sessions.
type waitClient struct {
	serverv1.ServerClient
	// replies is what each session's wait returns, keyed by name.
	replies map[string]*serverv1.WaitResponse
	// delays holds how long each session's wait takes, so ordering and concurrency are controllable.
	delays map[string]time.Duration

	mu sync.Mutex
	// inFlight and maxInFlight record concurrency, which is the property that distinguishes this from
	// a sequential loop.
	inFlight, maxInFlight int
	// started names every session whose wait was issued, so a test can tell one that was cancelled
	// from one that never ran.
	started []string
}

func (c *waitClient) Wait(
	ctx context.Context, req *serverv1.WaitRequest,
) (*serverv1.WaitResponse, error) {
	c.mu.Lock()
	c.inFlight++
	if c.inFlight > c.maxInFlight {
		c.maxInFlight = c.inFlight
	}
	c.started = append(c.started, req.Session)
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.inFlight--
		c.mu.Unlock()
	}()

	if d := c.delays[req.Session]; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			// Cancellation is how --any stops the siblings, so it must be reported rather than swallowed.
			return nil, ctx.Err()
		}
	}
	resp, ok := c.replies[req.Session]
	if !ok {
		return &serverv1.WaitResponse{}, nil
	}
	return resp, nil
}

// satisfied is a reply for a session that reached the state.
func satisfied() *serverv1.WaitResponse {
	return &serverv1.WaitResponse{Satisfied: true}
}

// timedOut is a reply for a session that did not, still running the named command.
func timedOut(command string) *serverv1.WaitResponse {
	return &serverv1.WaitResponse{Satisfied: false, Busy: true, Command: command}
}

// Waits must run concurrently, which is the whole reason waitMany exists rather than a shell loop.
//
// Sequential waits on N sessions take the sum of their durations, so a fan-out of five agents each
// taking a minute would take five minutes to collect while the agents themselves finished in one. This
// is the same trap the cm skill documents for `cm send --wait`.
func TestWaitManyRunsConcurrently(t *testing.T) {
	names := []string{"one", "two", "three"}
	cl := &waitClient{
		replies: map[string]*serverv1.WaitResponse{
			"one": satisfied(), "two": satisfied(), "three": satisfied(),
		},
		// Long enough that a sequential implementation could not overlap them.
		delays: map[string]time.Duration{
			"one": 50 * time.Millisecond, "two": 50 * time.Millisecond, "three": 50 * time.Millisecond,
		},
	}

	var stdout, stderr bytes.Buffer
	start := time.Now()
	err := waitMany(context.Background(), &stdout, &stderr, cl, names,
		waitTarget{state: serverv1.WaitState_WAIT_STATE_IDLE, until: "idle"}, 0, false, true)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("waitMany() error = %v", err)
	}

	// All three in flight at once is the direct assertion. The timing below is a second, weaker check:
	// concurrency is the property, and the clock only corroborates it.
	if cl.maxInFlight != len(names) {
		t.Errorf("max concurrent waits = %d, want %d: the waits ran sequentially",
			cl.maxInFlight, len(names))
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("waitMany took %v for three 50ms waits, want roughly one of them", elapsed)
	}
}

// The default form requires every session, so a partial success is a failure.
//
// It has to be: `cm wait --tag ... && collect` must not collect from sessions that are still working.
func TestWaitManyAllRequiresEverySession(t *testing.T) {
	names := []string{"one", "two"}
	cl := &waitClient{replies: map[string]*serverv1.WaitResponse{
		"one": satisfied(),
		"two": timedOut("make"),
	}}

	var stdout, stderr bytes.Buffer
	err := waitMany(context.Background(), &stdout, &stderr, cl, names,
		waitTarget{state: serverv1.WaitState_WAIT_STATE_IDLE, until: "idle"}, 0, false, true)
	if err == nil {
		t.Fatal("waitMany() = nil with one session unsatisfied, want a non-zero exit")
	}
	var exit *exitCodeError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Errorf("error = %v, want exit status 1", err)
	}
}

func TestWaitManyAllSucceedsWhenEverySessionReaches(t *testing.T) {
	names := []string{"one", "two"}
	cl := &waitClient{replies: map[string]*serverv1.WaitResponse{
		"one": satisfied(), "two": satisfied(),
	}}

	var stdout, stderr bytes.Buffer
	if err := waitMany(context.Background(), &stdout, &stderr, cl, names,
		waitTarget{state: serverv1.WaitState_WAIT_STATE_IDLE, until: "idle"}, 0, false, true); err != nil {
		t.Errorf("waitMany() error = %v, want nil when all are satisfied", err)
	}
}

// --any returns on the first session to reach the state, without waiting for the rest.
func TestWaitManyAnyReturnsOnTheFirst(t *testing.T) {
	names := []string{"slow", "fast"}
	cl := &waitClient{
		replies: map[string]*serverv1.WaitResponse{
			"slow": satisfied(), "fast": satisfied(),
		},
		delays: map[string]time.Duration{
			// The slow one would dominate if --any waited for everything.
			"slow": 10 * time.Second,
			"fast": 10 * time.Millisecond,
		},
	}

	var stdout, stderr bytes.Buffer
	start := time.Now()
	err := waitMany(context.Background(), &stdout, &stderr, cl, names,
		waitTarget{state: serverv1.WaitState_WAIT_STATE_IDLE, until: "idle"}, 0, true, true)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("waitMany(--any) error = %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("waitMany(--any) took %v, want it to return on the fast session", elapsed)
	}
	// Both were issued: --any starts every wait and takes the first answer, rather than guessing which
	// will finish first.
	cl.mu.Lock()
	started := len(cl.started)
	cl.mu.Unlock()
	if started != len(names) {
		t.Errorf("started %d waits, want %d", started, len(names))
	}
}

// --any with nothing satisfied still fails, rather than hanging or reporting success.
func TestWaitManyAnyWithNoSessionSatisfied(t *testing.T) {
	names := []string{"one", "two"}
	cl := &waitClient{replies: map[string]*serverv1.WaitResponse{
		"one": timedOut("make"), "two": timedOut("sleep"),
	}}

	var stdout, stderr bytes.Buffer
	err := waitMany(context.Background(), &stdout, &stderr, cl, names,
		waitTarget{state: serverv1.WaitState_WAIT_STATE_IDLE, until: "idle"}, 0, true, true)
	if err == nil {
		t.Fatal("waitMany(--any) = nil with nothing satisfied, want a non-zero exit")
	}
	var exit *exitCodeError
	if !errors.As(err, &exit) {
		t.Errorf("error = %v, want an exit status error", err)
	}
}

// A cancelled sibling is not reported as a failure.
//
// --any cancels the others by design, so treating their cancellation as an error would turn every
// successful --any into a reported failure.
func TestWaitManyAnyDoesNotReportCancelledSiblings(t *testing.T) {
	names := []string{"fast", "slow"}
	cl := &waitClient{
		replies: map[string]*serverv1.WaitResponse{"fast": satisfied(), "slow": satisfied()},
		delays: map[string]time.Duration{
			"fast": time.Millisecond,
			"slow": 30 * time.Second,
		},
	}

	var stdout, stderr bytes.Buffer
	if err := waitMany(context.Background(), &stdout, &stderr, cl, names,
		waitTarget{state: serverv1.WaitState_WAIT_STATE_IDLE, until: "idle"}, 0, true, true); err != nil {
		t.Fatalf("waitMany(--any) error = %v, want the cancellation ignored", err)
	}
}

// The failure report names each session that did not get there and what it was doing instead.
//
// The detail is the point: "2 of 3 did not reach idle" alone does not say which to look at, and a
// multi-session wait is used precisely when the caller is not watching each one.
func TestReportWaitManyNamesWhatFailed(t *testing.T) {
	names := []string{"one", "two", "three"}
	got := map[string]*serverv1.WaitResponse{
		"one":   satisfied(),
		"two":   timedOut("make -j4"),
		"three": {Satisfied: false, Busy: false},
	}

	var stdout, stderr bytes.Buffer
	err := reportWaitMany(&stdout, &stderr, names, got, "idle", false)
	if err == nil {
		t.Fatal("reportWaitMany() = nil with two unsatisfied, want an error")
	}

	msg := stderr.String()
	// Each failure named, with what it was doing.
	if !strings.Contains(msg, "two") || !strings.Contains(msg, "make -j4") {
		t.Errorf("stderr = %q, want it to name session two and its command", msg)
	}
	if !strings.Contains(msg, "three") {
		t.Errorf("stderr = %q, want it to name session three", msg)
	}
	// The satisfied one is not reported as a failure.
	if strings.Contains(msg, "for one to be") {
		t.Errorf("stderr = %q, want the satisfied session left out of the failures", msg)
	}
	// And a summary, so the count is visible without counting lines.
	if !strings.Contains(msg, "2 of 3") {
		t.Errorf("stderr = %q, want a summary naming 2 of 3", msg)
	}
}

// JSON output is an array covering every session, in the caller's order.
//
// Ordered by the list rather than by completion, so two runs of the same wait produce the same output
// and a diff of it means something.
func TestReportWaitManyJSONIsOrderedAndComplete(t *testing.T) {
	names := []string{"one", "two"}
	got := map[string]*serverv1.WaitResponse{
		"one": satisfied(),
		"two": timedOut("make"),
	}

	var stdout, stderr bytes.Buffer
	// Non-nil error, since one session timed out; the output is what is under test.
	_ = reportWaitMany(&stdout, &stderr, names, got, "idle", true)

	body := stdout.String()
	if !strings.HasPrefix(strings.TrimSpace(body), "[") {
		t.Errorf("JSON = %q, want an array so a script can iterate", body)
	}
	if i, j := strings.Index(body, `"one"`), strings.Index(body, `"two"`); i < 0 || j < 0 || i > j {
		t.Errorf("JSON = %q, want both sessions in the caller's order", body)
	}
	// Diagnostics stay off stdout, so a pipeline reading JSON is not corrupted by them.
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want it empty when reporting JSON", stderr.String())
	}
}

func TestReportWaitManySucceedsWhenAllSatisfied(t *testing.T) {
	names := []string{"one", "two"}
	got := map[string]*serverv1.WaitResponse{"one": satisfied(), "two": satisfied()}

	var stdout, stderr bytes.Buffer
	if err := reportWaitMany(&stdout, &stderr, names, got, "idle", false); err != nil {
		t.Errorf("reportWaitMany() error = %v, want nil", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing reported on success", stderr.String())
	}
}

// A session with no reply at all counts as unsatisfied rather than being skipped.
//
// Otherwise a wait whose RPC failed for one session would report success for the group, which is the
// worst direction for this to be wrong in.
func TestReportWaitManyTreatsAMissingReplyAsFailure(t *testing.T) {
	names := []string{"one", "missing"}
	got := map[string]*serverv1.WaitResponse{"one": satisfied()}

	var stdout, stderr bytes.Buffer
	if err := reportWaitMany(&stdout, &stderr, names, got, "idle", false); err == nil {
		t.Error("reportWaitMany() = nil with a session missing its reply, want an error")
	}
}
