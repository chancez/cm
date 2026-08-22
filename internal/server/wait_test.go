package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// waitFixture returns a live session and the service wrapping its manager.
func waitFixture(t *testing.T, name, script string) (*Service, *Session) {
	t.Helper()

	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor(name, script))
	rec.State = "running"
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get(name)
	if !ok {
		t.Fatal("session was not adopted")
	}
	return NewService(mgr), sess
}

// setBusy makes a session report a running command, standing in for a shell's OSC 133 report.
//
// Goes through the tracker and noteCommand rather than writing the field, so the bookkeeping the real
// path does happens here too. Setting sess.command directly is the obvious shortcut and skips the run
// counter, which made these tests fail against correct code: the fake was not exercising the mechanism
// under test.
//
// Driven through the tracker rather than a real shell because these tests are about the wait's decisions,
// not about parsing markers, which internal/osc covers.
func setBusy(sess *Session, running bool, command string) {
	if running {
		esc := strings.ReplaceAll(command, " ", `\ `)
		sess.commands.Feed([]byte("\x1b]133;C;cmdline=" + esc + "\x07"))
	} else {
		sess.commands.Feed([]byte("\x1b]133;D;0\x07"))
	}
	sess.noteCommand()
}

// setTitle publishes a metadata change that is not a command state change.
//
// Stands in for the shell echoing the input it was sent: real output, and therefore a real event, while
// the session is still idle and no command has started.
func setTitle(sess *Session, title string) {
	sess.mu.Lock()
	sess.title = title
	cwd, cmd := sess.cwd, sess.command
	sess.mu.Unlock()
	sess.publishMetadata(Metadata{Title: title, Cwd: cwd, Command: cmd})
}

// A session already in the requested state satisfies a wait immediately.
//
// `cm wait` answers "is it in this state", so a caller asking whether a session is idle wants yes rather
// than a wait for the next transition.
func TestWaitReturnsAtOnceWhenAlreadyInState(t *testing.T) {
	svc, _ := waitFixture(t, "already", "sleep 5")
	ctx := context.Background()

	start := time.Now()
	resp, err := svc.Wait(ctx, &serverv1.WaitRequest{
		Session: "already",
		Until:   serverv1.WaitState_WAIT_STATE_IDLE,
	})
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if !resp.Satisfied {
		t.Errorf("Wait() = %+v, want satisfied for a session already idle", resp)
	}
	// No timeout was set, so a wait that did not resolve here would hang rather than fail.
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Wait() took %s, want it to return at once", elapsed)
	}
}

// A wait resolves when the state changes, not by polling.
func TestWaitResolvesOnStateChange(t *testing.T) {
	svc, sess := waitFixture(t, "changes", "sleep 5")
	ctx := context.Background()

	setBusy(sess, true, "make")

	done := make(chan *serverv1.WaitResponse, 1)
	go func() {
		resp, err := svc.Wait(ctx, &serverv1.WaitRequest{
			Session:   "changes",
			Until:     serverv1.WaitState_WAIT_STATE_IDLE,
			TimeoutMs: 10_000,
		})
		if err != nil {
			t.Errorf("Wait() error = %v", err)
			done <- nil
			return
		}
		done <- resp
	}()

	// Still busy, so the wait must not have resolved.
	select {
	case resp := <-done:
		t.Fatalf("Wait() returned %+v while the session was still busy", resp)
	case <-time.After(200 * time.Millisecond):
	}

	setBusy(sess, false, "")

	select {
	case resp := <-done:
		if resp == nil || !resp.Satisfied {
			t.Errorf("Wait() = %+v, want satisfied once the command finished", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait() did not resolve after the session went idle")
	}
}

// A wait that gives up reports the state instead, rather than failing.
//
// "Not yet" is an ordinary answer a caller acts on. Returning an error would make a timeout
// indistinguishable from a session that does not exist.
func TestWaitTimeoutReportsCurrentState(t *testing.T) {
	svc, sess := waitFixture(t, "slow", "sleep 5")
	ctx := context.Background()

	setBusy(sess, true, "cargo build")

	resp, err := svc.Wait(ctx, &serverv1.WaitRequest{
		Session:   "slow",
		Until:     serverv1.WaitState_WAIT_STATE_IDLE,
		TimeoutMs: 100,
	})
	if err != nil {
		t.Fatalf("Wait() error = %v, want a result rather than a failure", err)
	}
	want := &serverv1.WaitResponse{
		Satisfied: false,
		Busy:      true,
		Command:   "cargo build",
		State:     serverv1.SessionState_SESSION_STATE_RUNNING,
	}
	if resp.Satisfied != want.Satisfied || resp.Busy != want.Busy ||
		resp.Command != want.Command || resp.State != want.State {
		t.Errorf("Wait() = %+v, want %+v", resp, want)
	}
}

// Waiting for a state a finished session can never reach must fail immediately.
//
// Blocking until the timeout would be worse than useless: the answer is already known, and a caller
// waiting minutes for a session that ended is a bug that looks like slowness.
func TestWaitOnEndedSessionFailsFastForUnreachableStates(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	if err := st.Create(ctx, store.Session{ID: "gone", State: store.StateExited}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	nameSession(t, st, "gone")
	svc := NewService(mgr)

	if _, err := svc.Wait(ctx, &serverv1.WaitRequest{
		Session:   "gone",
		Until:     serverv1.WaitState_WAIT_STATE_IDLE,
		TimeoutMs: 60_000,
	}); err == nil {
		t.Error("Wait(idle) on an ended session succeeded, want an error rather than a long wait")
	}

	// Exited is different: it already holds, so it is satisfied rather than an error.
	resp, err := svc.Wait(ctx, &serverv1.WaitRequest{
		Session: "gone",
		Until:   serverv1.WaitState_WAIT_STATE_EXITED,
	})
	if err != nil {
		t.Fatalf("Wait(exited) error = %v", err)
	}
	if !resp.Satisfied {
		t.Errorf("Wait(exited) = %+v, want satisfied for a session that has ended", resp)
	}
}

// A session ending satisfies a wait for exited and defeats any other.
func TestWaitEndsWhenTheSessionDoes(t *testing.T) {
	svc, sess := waitFixture(t, "ending", "sleep 5")
	ctx := context.Background()

	setBusy(sess, true, "make")

	// waiting is closed once the wait is registered, so Close below cannot run first.
	//
	// Without it the goroutine and Close race: when Close wins, Wait takes its fail-fast path for a
	// session that has already ended and returns an error rather than entering the loop under test. That
	// showed up as a flake in roughly one run in forty, and as a confusing one, since the error is
	// correct behavior for the state it actually saw.
	waiting := make(chan struct{})
	done := make(chan *serverv1.WaitResponse, 1)
	go func() {
		sub := sess.subscribeMetadata()
		defer sess.unsubscribeMetadata(sub)
		close(waiting)

		resp, err := svc.awaitState(ctx, sess, sub,
			serverv1.WaitState_WAIT_STATE_IDLE, 10_000, false, 0)
		if err != nil {
			t.Errorf("awaitState() error = %v", err)
			done <- nil
			return
		}
		done <- resp
	}()
	<-waiting

	sess.Close()

	select {
	case resp := <-done:
		if resp == nil {
			t.Fatal("Wait() failed")
		}
		// Not satisfied, because idle is not what happened: the session ended without going idle, and
		// reporting success would tell the caller its command finished.
		if resp.Satisfied {
			t.Errorf("awaitState(idle) = %+v, want unsatisfied when the session ended instead", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitState() did not return when the session ended")
	}
}

// send --wait idle must wait for the command it sent, not for the prompt it started at.
//
// This is the bug the combined request exists to avoid, and it is not subtle: a shell at a prompt is
// already idle, so checking on arrival succeeds before the command has started. Measured against a real
// zsh, the gap between input landing and the shell reporting a command is about 300ms, which is long
// enough to lose every single time rather than occasionally. A caller would then read output from before
// its own input and see the previous command's results.
func TestSendWaitDoesNotResolveBeforeTheCommandStarts(t *testing.T) {
	svc, sess := waitFixture(t, "sendwait", "sleep 5")
	ctx := context.Background()

	// Idle to begin with, which is the whole hazard.
	if sess.Command().Running {
		t.Fatal("setup: want an idle session")
	}

	done := make(chan *serverv1.SendResponse, 1)
	go func() {
		resp, err := svc.Send(ctx, &serverv1.SendRequest{
			Session:       "sendwait",
			Data:          []byte("make\r"),
			WaitUntil:     serverv1.WaitState_WAIT_STATE_IDLE,
			WaitTimeoutMs: 10_000,
		})
		if err != nil {
			t.Errorf("Send() error = %v", err)
			done <- nil
			return
		}
		done <- resp
	}()

	// The session is idle and has been sent input, but no command has started. A wait that resolves here
	// is the bug.
	select {
	case resp := <-done:
		t.Fatalf("Send(--wait idle) returned %+v before any command started", resp.GetWait())
	case <-time.After(300 * time.Millisecond):
	}

	// A metadata event that is not the command starting, which is what a shell echoing the input it was
	// sent produces: output, and therefore an event, while still idle.
	//
	// This is the second form of the same bug and needs its own step. Treating any event as evidence the
	// command began resolves here, before the command exists, and a test that only ever delivers busy
	// transitions cannot tell the two implementations apart.
	setTitle(sess, "echoing the input")
	select {
	case resp := <-done:
		t.Fatalf("Send(--wait idle) returned %+v on an event that was not the command starting: "+
			"a shell echoes its input before running it", resp.GetWait())
	case <-time.After(200 * time.Millisecond):
	}

	// Now the shell reports the command, and then finishes it.
	setBusy(sess, true, "make")
	select {
	case resp := <-done:
		t.Fatalf("Send(--wait idle) returned %+v while the command was running", resp.GetWait())
	case <-time.After(200 * time.Millisecond):
	}

	setBusy(sess, false, "")
	select {
	case resp := <-done:
		if resp == nil || !resp.GetWait().GetSatisfied() {
			t.Errorf("Send(--wait idle) = %+v, want satisfied once the command finished", resp.GetWait())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send(--wait idle) did not resolve after the command finished")
	}
}

// Waiting for busy after sending needs no start detection, since becoming busy is itself the change.
func TestSendWaitBusyResolvesOnTheCommandStarting(t *testing.T) {
	svc, sess := waitFixture(t, "sendbusy", "sleep 5")
	ctx := context.Background()

	done := make(chan *serverv1.SendResponse, 1)
	go func() {
		resp, err := svc.Send(ctx, &serverv1.SendRequest{
			Session:       "sendbusy",
			Data:          []byte("make\r"),
			WaitUntil:     serverv1.WaitState_WAIT_STATE_BUSY,
			WaitTimeoutMs: 10_000,
		})
		if err != nil {
			t.Errorf("Send() error = %v", err)
			done <- nil
			return
		}
		done <- resp
	}()

	select {
	case resp := <-done:
		t.Fatalf("Send(--wait busy) returned %+v before the command started", resp.GetWait())
	case <-time.After(200 * time.Millisecond):
	}

	setBusy(sess, true, "make")
	select {
	case resp := <-done:
		if resp == nil || !resp.GetWait().GetSatisfied() {
			t.Errorf("Send(--wait busy) = %+v, want satisfied", resp.GetWait())
		} else if got := resp.GetWait().GetCommand(); got != "make" {
			t.Errorf("command = %q, want %q", got, "make")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Send(--wait busy) did not resolve when the command started")
	}
}

// A send with no wait must not block, which is the existing behavior and the common case.
func TestSendWithoutWaitReturnsImmediately(t *testing.T) {
	svc, _ := waitFixture(t, "nowait", "sleep 5")
	ctx := context.Background()

	start := time.Now()
	resp, err := svc.Send(ctx, &serverv1.SendRequest{
		Session: "nowait",
		Data:    []byte("make\r"),
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if resp.GetWait() != nil {
		t.Errorf("Send() returned a wait result %+v without being asked for one", resp.GetWait())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Send() took %s without a wait, want it to return at once", elapsed)
	}
}

// A command too fast to observe running must still satisfy a wait for idle.
//
// The subscription that carries state changes coalesces to a depth of one, so a command like `true`
// starts and finishes between two reads of the channel and arrives as a single event. An implementation
// watching for the session to *be* busy sees nothing and waits out its timeout, reporting
// "waiting for idle; it is idle" -- true, and useless. Comparing a monotonic count of reported commands
// instead asks whether one ran at all, which survives the collapse.
//
// Reproduced by publishing both transitions before the waiter can read either, which is what coalescing
// does in practice rather than an approximation of it.
func TestSendWaitResolvesWhenBusyIsNeverObserved(t *testing.T) {
	svc, sess := waitFixture(t, "toofast", "sleep 5")
	ctx := context.Background()

	// The state changes happen while Send is between writing input and reading its subscription. A
	// goroutine racing it is not enough to guarantee that, so the transitions are published first and
	// the wait is then given the state it would actually find: idle, with a command having run.
	setBusy(sess, true, "true")
	setBusy(sess, false, "")

	// The session is idle and reports nothing running, exactly as it would after a fast command.
	if sess.Command().Running {
		t.Fatal("setup: want an idle session")
	}
	if sess.CommandRuns() == 0 {
		t.Fatal("setup: want a session that has reported running a command")
	}

	// Waiting from a count taken *before* those runs is what Send does when its input causes a command
	// that finishes immediately.
	sub := sess.subscribeMetadata()
	defer sess.unsubscribeMetadata(sub)

	done := make(chan *serverv1.WaitResponse, 1)
	go func() {
		resp, err := svc.awaitState(ctx, sess, sub,
			serverv1.WaitState_WAIT_STATE_IDLE, 2_000, true, 0)
		if err != nil {
			t.Errorf("awaitState() error = %v", err)
			done <- nil
			return
		}
		done <- resp
	}()

	select {
	case resp := <-done:
		if resp == nil || !resp.Satisfied {
			t.Errorf("awaitState() = %+v, want satisfied: a command ran and finished, and the count "+
				"shows it even though the session was never observed busy", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitState() never returned")
	}
}

// The counter must move once per command, not once per report.
//
// A shell repeats its markers, and a count that moved on every report would make an unrelated wait think
// a new command had started.
func TestCommandRunsCountsEachCommandOnce(t *testing.T) {
	_, sess := waitFixture(t, "counting", "sleep 5")

	if got := sess.CommandRuns(); got != 0 {
		t.Fatalf("CommandRuns() = %d on a fresh session, want 0", got)
	}

	setBusy(sess, true, "make")
	after := sess.CommandRuns()
	if after != 1 {
		t.Errorf("CommandRuns() = %d after one command started, want 1", after)
	}
	// The same report again is not a new command.
	setBusy(sess, true, "make")
	if got := sess.CommandRuns(); got != after {
		t.Errorf("CommandRuns() = %d after a repeated report, want it unchanged at %d", got, after)
	}

	setBusy(sess, false, "")
	if got := sess.CommandRuns(); got != after {
		t.Errorf("CommandRuns() = %d after the command ended, want it unchanged at %d", got, after)
	}

	setBusy(sess, true, "test")
	if got := sess.CommandRuns(); got != after+1 {
		t.Errorf("CommandRuns() = %d after a second command, want %d", got, after+1)
	}
}

// A `send --wait` against a session driven only by reports must resolve when the work finishes.
//
// The bug: the "has the caller's own work started" check counted OSC 133 commands, which a program
// reporting its own state never produces. So a wait after sending input could never observe a start, and
// every such call burned its entire timeout on work that had already completed. Measured before the fix at
// a full 8s timeout for 1s of work, reporting satisfied=false.
//
// The distinction the counter exists for is real, which is why the fix is to count reports too rather than
// to drop the check: an agent sitting at "idle" when the input arrives would otherwise satisfy a wait for
// idle immediately, and the caller would read the previous turn's output.
func TestSendWaitResolvesForAReportingSession(t *testing.T) {
	svc, sess := waitFixture(t, "reporter", "sleep 30")
	ctx := context.Background()

	// Idle before the input, which is the state that made this fail: satisfied() is already true, so only
	// the started check stands between the caller and a wrong answer.
	sess.setReported(Reported{State: "idle", Source: "agent"})

	// The work: busy then idle again, as an agent reporting for itself does, both landing in quick
	// succession once the input has been written.
	//
	// The two transitions are deliberately not spaced out. A fast turn's busy and idle coalesce into one
	// event, so nothing ever observes Running and only the counter shows that anything happened. That is the
	// same property the OSC 133 tests rely on, and it is what makes counting reports necessary: without it
	// the wait has no evidence the work began and sits until its timeout.
	go func() {
		time.Sleep(20 * time.Millisecond)
		sess.setReported(Reported{State: "busy", Source: "agent"})
		sess.setReported(Reported{State: "idle", Source: "agent"})
	}()

	resp, err := svc.Send(ctx, &serverv1.SendRequest{
		Session:       "reporter",
		Data:          []byte("work\r"),
		WaitUntil:     serverv1.WaitState_WAIT_STATE_IDLE,
		WaitTimeoutMs: 5000,
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if !resp.GetWait().Satisfied {
		t.Error("wait was not satisfied, so a reporting session cannot be driven with send --wait")
	}
	// And the session must not be described as reporting nothing, or callers get a warning telling them to
	// load a shell integration they do not need.
	if !resp.ShellReports {
		t.Error("ShellReports = false for a session that reports its own state")
	}
}

// The started check must still do its job: an already-idle session must not satisfy a wait for idle before
// the work it was sent has begun.
//
// The counterpart of the test above, and the reason the fix counts reports rather than ignoring the check.
// Without this, "fixing" the bug by treating every wait as started would pass the test above while making
// send --wait return the previous turn's state.
func TestSendWaitDoesNotResolveBeforeTheWorkStarts(t *testing.T) {
	svc, sess := waitFixture(t, "notyet", "sleep 30")
	ctx := context.Background()

	// Idle and staying idle: nothing reports, so the work never starts.
	sess.setReported(Reported{State: "idle", Source: "agent"})

	resp, err := svc.Send(ctx, &serverv1.SendRequest{
		Session:       "notyet",
		Data:          []byte("work\r"),
		WaitUntil:     serverv1.WaitState_WAIT_STATE_IDLE,
		WaitTimeoutMs: 300,
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if resp.GetWait().Satisfied {
		t.Error("wait was satisfied by the state the session was already in, before its work began")
	}
}

// A wait must report how far the server has consumed the session's output.
//
// A follower needs this to know when it has caught up. The wait returning means the command finished, which is
// not the same as its output having reached the client: the bytes are still travelling over the stream, and a
// caller that stops following on the reply truncates them. `cm run` on a reused session lost the command's
// output that way about a third of the time, presenting as a line containing only the shell's echo of the
// input.
//
// Asserted on both paths, since `cm send --wait` is the one that needed it and a bare `cm wait` returning zero
// would give a follower no position to drain to.
func TestWaitReportsTheConsumedPosition(t *testing.T) {
	svc, sess := waitFixture(t, "seq", "sleep 30")
	ctx := context.Background()

	// A consumed position, as the pump sets after reading from the shim. Written directly because the pump
	// is what advances it and this test is about the value being reported, not about how it got there.
	const want = 4242
	sess.mu.Lock()
	sess.lastSeq = want
	sess.mu.Unlock()

	// Idle, so the wait is satisfied and returns a reply to inspect.
	setBusy(sess, false, "")

	resp, err := svc.Wait(ctx, &serverv1.WaitRequest{
		Session:   "seq",
		Until:     serverv1.WaitState_WAIT_STATE_IDLE,
		TimeoutMs: 2000,
	})
	if err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if !resp.Satisfied {
		t.Fatal("wait was not satisfied")
	}
	if resp.LastSeq != want {
		t.Errorf("LastSeq = %d, want %d: without it a follower has no position to drain to and cuts the "+
			"stream off mid-output", resp.LastSeq, want)
	}
}
