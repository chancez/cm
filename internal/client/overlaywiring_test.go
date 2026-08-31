package client

import (
	"context"
	"sync"
	"testing"
	"time"
)

// overlayWaitFor takes one value off a channel or fails, rather than blocking forever.
//
// A deadline rather than a bare receive because of what the failure looks like without one: with the
// overlay unwired every case here blocks, and `go test` reports that as a panic after the package's
// ten-minute timeout with no indication which assertion was waiting. Measured while mutation-testing
// this file, which is exactly when a test has to say what it wanted.
func overlayWaitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out after 5s waiting for %s", what)
		var zero T
		return zero
	}
}

// overlayHarness prepares a runSession call with both keys live and a terminal to paint on.
//
// A pty rather than a pipe, because the overlay is only enabled where there is a terminal to paint on:
// tty.Size on a pipe reports nothing, and a row number guessed wrong would write into the session.
func overlayHarness(t *testing.T) *harness {
	t.Helper()

	h := newPtyHarness(t, 24, 80)
	detach, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}
	prefix, err := ParsePrefixKey(DefaultPrefixKey)
	if err != nil {
		t.Fatalf("ParsePrefixKey() error = %v", err)
	}
	h.opts.DetachKey = detach
	h.opts.PrefixKey = prefix
	return h
}

// recordedCommands collects what the overlay asked to run, from the goroutine the attach loop dispatches
// it on.
type recordedCommands struct {
	mu   sync.Mutex
	refs []string
	args [][]string
	// out and err are what the runner reports back, so a case can drive the result screen.
	out string
	err error
	// ran is closed once, on the first command, so a test can wait for the dispatch rather than sleep.
	ran chan struct{}
}

func newRecordedCommands() *recordedCommands {
	return &recordedCommands{ran: make(chan struct{}, 4)}
}

func (r *recordedCommands) run(_ context.Context, ref string, args []string) (string, error) {
	r.mu.Lock()
	r.refs = append(r.refs, ref)
	r.args = append(r.args, args)
	out, err := r.out, r.err
	r.mu.Unlock()
	r.ran <- struct{}{}
	return out, err
}

func (r *recordedCommands) last() (string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.args) == 0 {
		return "", nil
	}
	return r.refs[len(r.refs)-1], r.args[len(r.args)-1]
}

// The prefix key opens the overlay and its action key detaches, with nothing reaching the session.
//
// Fed as one read on purpose: that is what a terminal delivers when the two keys are typed quickly, and
// the remainder of a read is exactly what an earlier design dropped.
func TestRunSessionPrefixThenDetach(t *testing.T) {
	h := overlayHarness(t)
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.input <- []byte("\x1dd")

	if oc := overlayWaitFor(t, done, "the attachment to end"); oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone", oc)
	}
	if !h.result.Detached {
		t.Error("result.Detached = false, want true")
	}
	if got := h.stream.inputs(); len(got) != 0 {
		t.Errorf("input reached the session: %q, want the overlay to have consumed it", got)
	}
}

// A command typed at the overlay runs against this session, and closing the overlay afterwards asks for a
// repaint by dropping the resume position and reconnecting.
//
// The repaint is the half worth asserting: the overlay wrote over rows holding the program's screen, and
// cm's model is the only thing that knows what was there. A client that resumed instead would leave the
// bar on screen until the program next drew that row.
func TestRunSessionOverlayRunsACommandThenRepaints(t *testing.T) {
	h := overlayHarness(t)
	rec := newRecordedCommands()
	rec.out = "ref now names @a7k2m9x4\n"
	h.opts.RunCommand = rec.run
	h.stream.opened("test", 7, nil)

	done := h.runAsync(context.Background())
	h.input <- []byte("\x1d:bind ref\r")
	overlayWaitFor(t, rec.ran, "the overlay to dispatch a command")

	ref, args := rec.last()
	if len(args) != 2 || args[0] != "bind" || args[1] != "ref" {
		t.Errorf("ran %q, want [bind ref]", args)
	}
	if ref == "" {
		t.Error("the command was given no session to act on")
	}

	// Dismiss the result, which closes the overlay.
	h.input <- []byte("\r")
	if oc := overlayWaitFor(t, done, "the repaint"); oc != outcomeReconnect {
		t.Errorf("outcome = %v, want outcomeReconnect so the screen is repainted", oc)
	}
	if h.resumeFrom != nil {
		t.Errorf("resumeFrom = %v, want nil: a resume would not repaint the rows the overlay covered",
			*h.resumeFrom)
	}
	if got := h.stream.inputs(); len(got) != 0 {
		t.Errorf("input reached the session: %q, want the overlay to have consumed all of it", got)
	}
}

// The overlay forwards the detach key to the program, which is the only way to reach a key cm intercepts.
// Before this, no 0x1c ever reached a pty from a cm client, so SIGQUIT was unreachable inside a session.
func TestRunSessionOverlaySendsTheDetachKeyToTheProgram(t *testing.T) {
	h := overlayHarness(t)
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.input <- []byte("\x1dq")
	// The Open, then the forwarded key.
	h.stream.waitForRequests(t, 2)

	if got := string(h.stream.inputs()); got != "\x1c" {
		t.Errorf("input forwarded = %q, want the detach key itself", got)
	}
	if n := h.stream.detaches(); n != 0 {
		t.Errorf("client sent %d Detach messages, want 0: the key was for the program", n)
	}
	if oc := overlayWaitFor(t, done, "the repaint"); oc != outcomeReconnect {
		t.Errorf("outcome = %v, want outcomeReconnect for the repaint", oc)
	}
}

// While a command is in flight the loop keeps running, which is why it is dispatched rather than run
// inline: a command run inline would freeze the session's output for as long as it took.
func TestRunSessionKeepsPaintingWhileACommandRuns(t *testing.T) {
	h := overlayHarness(t)
	rec := newRecordedCommands()
	h.opts.RunCommand = rec.run
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.input <- []byte("\x1d:list\r")
	overlayWaitFor(t, rec.ran, "the overlay to dispatch a command")

	// Output arriving now is still delivered, and the session ending is still noticed.
	h.stream.output(0, "still alive")
	h.stream.exited(3)
	if oc := overlayWaitFor(t, done, "the session to end"); oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone", oc)
	}
	if h.result.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3: the loop was not reading the stream", h.result.ExitCode)
	}
}

// With no runner wired the keys still work and a command says why it cannot run, rather than appearing to
// do nothing. Every caller except `cm attach` is in this state.
func TestRunSessionOverlayWithoutARunner(t *testing.T) {
	h := overlayHarness(t)
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.input <- []byte("\x1d:list\r")
	// Nothing to wait for but the dismissal, since the refusal is painted rather than sent anywhere.
	h.input <- []byte("\r")

	if oc := overlayWaitFor(t, done, "the overlay to close"); oc != outcomeReconnect {
		t.Errorf("outcome = %v, want outcomeReconnect after the overlay closed", oc)
	}
	if got := h.stream.inputs(); len(got) != 0 {
		t.Errorf("input reached the session: %q", got)
	}
}

// A client with no terminal to paint on gets no overlay, and the prefix key reaches the session like any
// other keystroke. That is every follower: `cm read --follow` and anything streaming to a pipe.
//
// Without this the key would be intercepted by a gate that then had nowhere to paint, so a follower would
// silently swallow one byte of whatever it was piping.
func TestRunSessionNoOverlayWithoutATerminal(t *testing.T) {
	h := newHarness(t)
	prefix, err := ParsePrefixKey(DefaultPrefixKey)
	if err != nil {
		t.Fatalf("ParsePrefixKey() error = %v", err)
	}
	h.opts.PrefixKey = prefix
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.input <- []byte("\x1dd")
	h.stream.waitForRequests(t, 2)

	if got := string(h.stream.inputs()); got != "\x1dd" {
		t.Errorf("input forwarded = %q, want the prefix key and the key after it passed through", got)
	}

	h.stream.exited(0)
	if oc := overlayWaitFor(t, done, "the session to end"); oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone", oc)
	}
}
