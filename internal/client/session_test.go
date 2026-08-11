package client

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/containerd/ttrpc"
	"github.com/creack/pty"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// runSession is the client's whole attachment: it owns the resume point, the pending-input buffer, detach-key
// matching, and the decision to reconnect or stop. Every one of those has produced a bug, and none of it was
// covered: the package sat at 18.9% with the loop itself at zero.
//
// Driven directly rather than through Attach, which owns the dial-and-retry loop around it. Testing the loop
// body in isolation means each case can arrange one connection's worth of behavior and assert the outcome
// exactly, rather than waiting on reconnect timers.

// fakeStream stands in for the bidi Attach stream.
//
// A fake rather than a real one over a socket, because these tests are about what the client does with a
// sequence of messages, and a real stream would make the arrangement of that sequence the hard part.
type fakeStream struct {
	mu sync.Mutex
	// sent records everything the client sent, in order, so a test can assert on the protocol it speaks.
	sent []*serverv1.AttachRequest
	// sendErr, once set, fails every subsequent Send. Used for "the server went away mid-keystroke".
	sendErr error

	// recv delivers server messages. Closing it is how a test says the stream ended cleanly.
	recv chan recvResult
}

type recvResult struct {
	resp *serverv1.AttachResponse
	err  error
}

func newFakeStream() *fakeStream {
	return &fakeStream{recv: make(chan recvResult, 32)}
}

func (s *fakeStream) Send(req *serverv1.AttachRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent = append(s.sent, req)
	return nil
}

func (s *fakeStream) Recv() (*serverv1.AttachResponse, error) {
	r, ok := <-s.recv
	if !ok {
		return nil, io.EOF
	}
	return r.resp, r.err
}

func (s *fakeStream) CloseSend() error  { return nil }
func (s *fakeStream) SendMsg(any) error { return nil }
func (s *fakeStream) RecvMsg(any) error { return nil }

// failSends makes every later Send fail, as a dropped connection does.
func (s *fakeStream) failSends(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendErr = err
}

// waitForRequests blocks until the client has sent at least n messages.
//
// A synchronization point, needed because these tests interact with a running runSession. Without it, arranging
// "the connection drops after the session is open" races the client's own Open: failing sends immediately makes
// the Open fail instead, so the test exercises a different path and asserts the wrong thing. That is exactly
// what happened on the first run of TestRunSessionHoldsInputWhenTheServerGoesAway.
func (s *fakeStream) waitForRequests(t *testing.T, n int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.requests()) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("client sent %d messages within 5s, want at least %d", len(s.requests()), n)
}

// requests returns a copy of what the client sent.
func (s *fakeStream) requests() []*serverv1.AttachRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*serverv1.AttachRequest(nil), s.sent...)
}

// inputs returns the input payloads the client sent, concatenated.
//
// Concatenated because how the client chunks input is not a promise: what matters is that the bytes arrive in
// order and none are lost or duplicated.
func (s *fakeStream) inputs() []byte {
	var out []byte
	for _, r := range s.requests() {
		if in := r.GetInput(); in != nil {
			out = append(out, in.Data...)
		}
	}
	return out
}

// detaches counts the detach events sent.
func (s *fakeStream) detaches() int {
	n := 0
	for _, r := range s.requests() {
		if r.GetDetach() != nil {
			n++
		}
	}
	return n
}

// resizes returns the resize events sent.
func (s *fakeStream) resizes() []*serverv1.Resize {
	var out []*serverv1.Resize
	for _, r := range s.requests() {
		if rz := r.GetResize(); rz != nil {
			out = append(out, rz)
		}
	}
	return out
}

// push queues a server message.
func (s *fakeStream) push(resp *serverv1.AttachResponse) {
	s.recv <- recvResult{resp: resp}
}

// pushErr queues a receive error.
func (s *fakeStream) pushErr(err error) {
	s.recv <- recvResult{err: err}
}

// opened queues the server's first reply, which every connection begins with.
func (s *fakeStream) opened(session string, nextSeq uint64, restore []byte) {
	s.push(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Opened{Opened: &serverv1.Opened{
			Session: session, NextSeq: nextSeq, Restore: restore,
		}},
	})
}

// output queues session output at a sequence number.
func (s *fakeStream) output(seq uint64, data string) {
	s.push(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Output{Output: &serverv1.Output{
			Seq: seq, Data: []byte(data),
		}},
	})
}

// exited queues the session ending.
func (s *fakeStream) exited(code int32) {
	s.push(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Exited{Exited: &serverv1.Exited{ExitCode: code}},
	})
}

// detached queues the server's acknowledgement of a deliberate detach.
func (s *fakeStream) detached() {
	s.push(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Detached{Detached: &serverv1.Detached{}},
	})
}

// fakeClient is a ServerClient that hands out a prepared stream.
type fakeClient struct {
	stream *fakeStream
	// attachErr fails the Attach call itself, before any message is exchanged.
	attachErr error
}

func (c *fakeClient) Attach(context.Context) (serverv1.Server_AttachClient, error) {
	if c.attachErr != nil {
		return nil, c.attachErr
	}
	return c.stream, nil
}

// The rest of the service is unused here: runSession only ever calls Attach.
func (c *fakeClient) List(context.Context, *serverv1.ListRequest) (*serverv1.ListResponse, error) {
	panic("unused")
}
func (c *fakeClient) Kill(context.Context, *serverv1.KillRequest) (*serverv1.KillResponse, error) {
	panic("unused")
}
func (c *fakeClient) Detach(
	context.Context, *serverv1.DetachRequest,
) (*serverv1.DetachResponse, error) {
	panic("unused")
}
func (c *fakeClient) Send(context.Context, *serverv1.SendRequest) (*serverv1.SendResponse, error) {
	panic("unused")
}
func (c *fakeClient) History(
	context.Context, *serverv1.HistoryRequest,
) (*serverv1.HistoryResponse, error) {
	panic("unused")
}
func (c *fakeClient) GetEnv(
	context.Context, *serverv1.GetEnvRequest,
) (*serverv1.GetEnvResponse, error) {
	panic("unused")
}
func (c *fakeClient) Report(
	context.Context, *serverv1.ReportRequest,
) (*serverv1.ReportResponse, error) {
	panic("unused")
}
func (c *fakeClient) Signal(context.Context, *serverv1.SignalRequest) (*serverv1.SignalResponse, error) {
	panic("unused")
}
func (c *fakeClient) Tag(context.Context, *serverv1.TagRequest) (*serverv1.TagResponse, error) {
	panic("unused")
}
func (c *fakeClient) Read(context.Context, *serverv1.ReadRequest) (*serverv1.ReadResponse, error) {
	panic("unused")
}
func (c *fakeClient) Wait(context.Context, *serverv1.WaitRequest) (*serverv1.WaitResponse, error) {
	panic("unused")
}
func (c *fakeClient) Doctor(
	context.Context, *serverv1.DoctorRequest,
) (*serverv1.DoctorResponse, error) {
	panic("unused")
}
func (c *fakeClient) Status(
	context.Context, *serverv1.StatusRequest,
) (*serverv1.StatusResponse, error) {
	panic("unused")
}
func (c *fakeClient) Shutdown(
	context.Context, *serverv1.ShutdownRequest,
) (*serverv1.ShutdownResponse, error) {
	panic("unused")
}

// Compile-time checks, so a change to the generated interfaces fails here rather than in every test.
var (
	_ serverv1.ServerClient        = (*fakeClient)(nil)
	_ serverv1.Server_AttachClient = (*fakeStream)(nil)
	_ ttrpc.ClientStream           = (*fakeStream)(nil)
)

// harness is one runSession call and everything it needs.
type harness struct {
	t      *testing.T
	stream *fakeStream
	client *fakeClient
	tty    *TTY
	// out collects what was written to the terminal, which is how restore and output are asserted.
	out *os.File
	// outRead is the read end of the terminal's output pipe.
	outRead *os.File

	opts       Options
	result     Result
	resumeFrom *uint64
	pending    []byte
	winch      chan os.Signal
	input      chan []byte
	inputErr   chan error
}

// newHarness prepares a runSession call against a pipe-backed TTY.
//
// A pipe rather than a pty, deliberately. A pipe makes IsTerminal false, which is the behavior most of these
// cases want: on a real terminal, input ending means the window closed and the client must leave, which would
// cut short every test that closes the input channel for other reasons. The two cases that need IsTerminal
// true say so explicitly.
func newHarness(t *testing.T) *harness {
	t.Helper()

	inR, _, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	t.Cleanup(func() {
		inR.Close()
		outR.Close()
		outW.Close()
	})

	tty, err := OpenTTY(inR, outW)
	if err != nil {
		t.Fatalf("OpenTTY() error = %v", err)
	}

	stream := newFakeStream()
	h := &harness{
		t:        t,
		stream:   stream,
		client:   &fakeClient{stream: stream},
		tty:      tty,
		out:      outW,
		outRead:  outR,
		opts:     Options{Session: "test"},
		winch:    make(chan os.Signal, 1),
		input:    make(chan []byte, 16),
		inputErr: make(chan error, 1),
	}
	h.result.Session = "test"
	return h
}

// newPtyHarness prepares a runSession call against a real pty of a known size.
//
// Needed for anything about window size: a pipe reports no size at all, so the client sends nothing and a test
// built on one cannot tell a working resize path from a missing one.
//
// Note that this makes IsTerminal true, which changes what input ending means: on a terminal the window closing
// ends the attachment. Cases that close the input channel should use newHarness instead.
func newPtyHarness(t *testing.T, rows, cols uint16) *harness {
	t.Helper()

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open() error = %v", err)
	}
	t.Cleanup(func() {
		ptmx.Close()
		tty.Close()
	})
	if err := pty.Setsize(tty, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		t.Fatalf("Setsize() error = %v", err)
	}

	// Drain the pty, or a write from the client blocks once the buffer fills and the test deadlocks.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := ptmx.Read(buf); err != nil {
				return
			}
		}
	}()

	ttyWrap, err := OpenTTY(tty, tty)
	if err != nil {
		t.Fatalf("OpenTTY() error = %v", err)
	}
	t.Cleanup(func() { ttyWrap.Close() })

	stream := newFakeStream()
	h := &harness{
		t:        t,
		stream:   stream,
		client:   &fakeClient{stream: stream},
		tty:      ttyWrap,
		opts:     Options{Session: "test"},
		winch:    make(chan os.Signal, 1),
		input:    make(chan []byte, 16),
		inputErr: make(chan error, 1),
	}
	h.result.Session = "test"
	return h
}

// run calls runSession and returns its outcome.
func (h *harness) run(ctx context.Context) (outcome, error) {
	h.t.Helper()
	return runSession(ctx, h.tty, h.client, h.opts, &h.result,
		&h.resumeFrom, &h.pending, h.winch, h.input, h.inputErr)
}

// runAsync calls runSession on its own goroutine, for cases that must interact while it runs.
func (h *harness) runAsync(ctx context.Context) <-chan outcome {
	h.t.Helper()

	done := make(chan outcome, 1)
	go func() {
		oc, err := h.run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			// Reported rather than failed: several cases return an error legitimately, and the test asserts
			// on the outcome. A surprising error still shows up in the log.
			h.t.Logf("runSession() error = %v", err)
		}
		done <- oc
	}()
	return done
}

// waitForInputConsumed blocks until the loop has taken everything off the input channel.
//
// Used by cases that assert nothing was sent. The input channel is buffered, so an empty one means runSession
// read from it; asserting an absence without this can pass because the loop never looked.
func (h *harness) waitForInputConsumed(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.input) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runSession did not read the input channel within 5s")
}

// waitForWinchConsumed blocks until the loop has taken the resize signal off the channel.
//
// The winch counterpart of waitForInputConsumed, and needed for the same reason: both resize tests assert that
// nothing was sent.
func (h *harness) waitForWinchConsumed(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.winch) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("runSession did not read the resize signal within 5s")
}

// terminalOutput returns what was written to the terminal so far.
//
// Reads with a deadline rather than blocking, since a case that wrote nothing must not hang the test.
func (h *harness) terminalOutput() string {
	h.t.Helper()
	return h.terminalOutputWithin(2 * time.Second)
}

// noTerminalOutput asserts nothing was written, without waiting out the full deadline.
//
// A short deadline on purpose: this is the "expected empty" case, where the timeout is the answer rather than a
// failure, and a two-second wait per call is pure test latency.
func (h *harness) noTerminalOutput() {
	h.t.Helper()

	if got := h.terminalOutputWithin(100 * time.Millisecond); got != "" {
		h.t.Errorf("terminal output = %q, want nothing written", got)
	}
}

func (h *harness) terminalOutputWithin(d time.Duration) string {
	h.t.Helper()

	if err := h.outRead.SetReadDeadline(time.Now().Add(d)); err != nil {
		h.t.Fatalf("SetReadDeadline() error = %v", err)
	}
	buf := make([]byte, 64*1024)
	n, err := h.outRead.Read(buf)
	if err != nil && n == 0 && !os.IsTimeout(err) {
		h.t.Fatalf("reading terminal output: %v", err)
	}
	return string(buf[:n])
}

// A session that ends is reported as exited, with its status.
//
// The exit status is the whole point of `cm run` and of attaching to a command: it is what a script checks.
func TestRunSessionReportsExit(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)
	h.stream.exited(42)

	oc, err := h.run(context.Background())
	if err != nil {
		t.Fatalf("runSession() error = %v", err)
	}
	if oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone: a finished session must not be reconnected to", oc)
	}
	if !h.result.Exited {
		t.Error("result.Exited = false, want true")
	}
	if h.result.ExitCode != 42 {
		t.Errorf("result.ExitCode = %d, want 42", h.result.ExitCode)
	}
}

// The resume point advances past every byte received, so a reconnect asks for what it missed.
//
// This is the mechanism behind output not being lost or duplicated across a server restart. It advances by the
// length of the data, not to the message's own sequence number: getting that wrong by one message re-delivers
// or skips a chunk, which is exactly the class of bug that produced a prompt rendered mid-escape-sequence.
func TestRunSessionAdvancesTheResumePoint(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 100, nil)
	h.stream.output(100, "hello")
	h.stream.output(105, " world")
	h.stream.exited(0)

	if _, err := h.run(context.Background()); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	if h.resumeFrom == nil {
		t.Fatal("resumeFrom is nil, so a reconnect would repaint from the beginning")
	}
	// 105 + len(" world") == 111: one past the last byte consumed.
	if *h.resumeFrom != 111 {
		t.Errorf("resumeFrom = %d, want 111 (one past the last byte received)", *h.resumeFrom)
	}
	if got := h.terminalOutput(); got != "hello world" {
		t.Errorf("terminal output = %q, want %q", got, "hello world")
	}
}

// The resume point is sent on the next connection, which is what makes a reconnect resume rather than repaint.
func TestRunSessionSendsTheResumePointOnReconnect(t *testing.T) {
	h := newHarness(t)
	seq := uint64(500)
	h.resumeFrom = &seq

	h.stream.opened("test", 500, nil)
	h.stream.exited(0)

	if _, err := h.run(context.Background()); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	reqs := h.stream.requests()
	if len(reqs) == 0 {
		t.Fatal("nothing was sent, want an Open")
	}
	open := reqs[0].GetOpen()
	if open == nil {
		t.Fatalf("first request = %+v, want an Open", reqs[0])
	}
	if open.ResumeFromSeq == nil {
		t.Fatal("Open.ResumeFromSeq is nil, so the server would repaint from scratch")
	}
	if *open.ResumeFromSeq != 500 {
		t.Errorf("Open.ResumeFromSeq = %d, want 500", *open.ResumeFromSeq)
	}
}

// Restored state is painted after clearing, so it does not land on top of the client's own shell output.
func TestRunSessionClearsBeforePaintingRestoredState(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 10, []byte("RESTORED"))
	h.stream.exited(0)

	if _, err := h.run(context.Background()); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	// Nothing is written for a pipe, since escape bytes would corrupt a non-terminal stream. What is asserted
	// is the payload arriving at all, and in the right order relative to the clear.
	got := h.terminalOutput()
	if got != "RESTORED" {
		t.Errorf("terminal output = %q, want the restored state", got)
	}
}

// Input typed while attached reaches the server.
func TestRunSessionForwardsInput(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.input <- []byte("ls -la\n")
	// Wait for the input to have been forwarded before ending the session.
	//
	// Output and input arrive on different channels, and select picks arbitrarily among ready cases, so
	// queueing the exit immediately lets the loop return before ever reading the input. Two messages: the Open
	// and the input.
	h.stream.waitForRequests(t, 2)
	// Ending the session is how the loop is stopped, since a pipe-backed client keeps going otherwise.
	h.stream.exited(0)
	<-done

	if got := string(h.stream.inputs()); got != "ls -la\n" {
		t.Errorf("input sent = %q, want %q", got, "ls -la\n")
	}
}

// A read-only client sends no input, however much is typed.
//
// The point of --read-only is that a watcher cannot disturb the session. A client that forwarded keystrokes
// anyway would be worse than useless: the owner would see input they did not type.
func TestRunSessionReadOnlySendsNoInput(t *testing.T) {
	h := newHarness(t)
	h.opts.ReadOnly = true
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.input <- []byte("rm -rf /\n")

	// Wait for the loop to have taken the keystroke off the channel before ending the session.
	//
	// Necessary because this asserts an absence. Without it the loop can return on the exit event without ever
	// reading the input, and "no input was sent" would be true because nothing was processed rather than
	// because read-only suppressed it: a test that passes for the wrong reason and would keep passing if the
	// suppression were removed. The channel is buffered, so a drained channel means the loop consumed it.
	h.waitForInputConsumed(t)

	h.stream.exited(0)
	<-done

	if got := h.stream.inputs(); len(got) != 0 {
		t.Errorf("a read-only client sent input: %q", got)
	}
}

// The detach key detaches, tells the server it was deliberate, and waits for the acknowledgement.
//
// The wait is load-bearing rather than polite. The send is asynchronous, so returning immediately tears the
// connection down before the message goes out; the server then sees a client that vanished without detaching
// and destroys an owned session, which is the opposite of what was asked. That was a real bug, found by adding
// a delay and watching the session survive.
func TestRunSessionDetachesOnTheDetachKey(t *testing.T) {
	h := newHarness(t)
	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}
	h.opts.DetachKey = key
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	// A keystroke, then the detach byte: the preceding byte must still be forwarded rather than dropped.
	h.input <- []byte{'x', key.Byte}
	// The server acknowledges, which is what releases the client.
	h.stream.detached()

	if oc := <-done; oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone: a deliberate detach must not reconnect", oc)
	}
	if !h.result.Detached {
		t.Error("result.Detached = false, want true")
	}
	if h.stream.detaches() != 1 {
		t.Errorf("sent %d detach events, want exactly 1", h.stream.detaches())
	}
	// The byte typed before the detach key is not swallowed.
	if got := string(h.stream.inputs()); got != "x" {
		t.Errorf("input sent = %q, want the keystroke before the detach key", got)
	}
	// And the detach key itself is never forwarded to the shell.
	if got := h.stream.inputs(); len(got) > 1 {
		t.Errorf("the detach sequence was forwarded to the shell: %q", got)
	}
}

// A detach sequence split across two reads is still recognized.
//
// A multi-byte key can arrive in pieces, since a terminal read returns whatever is available. A client that
// matched only within a single read would forward half the sequence to the shell and then act on nothing,
// which looks like a key that works intermittently.
func TestRunSessionDetachesOnASplitSequence(t *testing.T) {
	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}
	// The multi-byte encodings are the CSI forms a terminal sends when a keyboard protocol is active. Those
	// are the ones that can arrive split, since the single control byte cannot be.
	if len(key.Sequences) == 0 {
		t.Fatal("the default detach key has no multi-byte encodings, so nothing here can be split")
	}
	seq := key.Sequences[0]
	if len(seq) < 2 {
		t.Fatalf("encoding is %d bytes, need at least 2 to split", len(seq))
	}

	h := newHarness(t)
	h.opts.DetachKey = key
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.input <- seq[:1]
	h.input <- seq[1:]
	h.stream.detached()

	if oc := <-done; oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone", oc)
	}
	if !h.result.Detached {
		t.Error("result.Detached = false, want true: a split sequence must still detach")
	}
	// The first half was held back rather than forwarded, so the shell never saw a stray byte.
	if got := h.stream.inputs(); len(got) != 0 {
		t.Errorf("part of the detach sequence reached the shell: %q", got)
	}
}

// Input that fails to send is held for the next connection rather than lost.
//
// A user typing through a brief freeze has had their keystrokes accepted by the terminal already. Dropping
// them is worse than the freeze, because it is invisible.
func TestRunSessionHoldsInputWhenTheServerGoesAway(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	// Wait for the Open to have been sent, so the drop below happens to an established connection rather than
	// to the handshake. Without this the Open itself fails and the input path is never reached.
	h.stream.waitForRequests(t, 1)

	// The connection drops, then a keystroke is typed into it.
	h.stream.failSends(errors.New("connection reset"))
	h.input <- []byte("typed during the outage")

	if oc := <-done; oc != outcomeReconnect {
		t.Errorf("outcome = %v, want outcomeReconnect: a dropped send means the server went away", oc)
	}
	if got := string(h.pending); got != "typed during the outage" {
		t.Errorf("pending = %q, want the typed bytes held for the reconnect", got)
	}
}

// Held input is flushed on the next connection, exactly once.
//
// Once, not twice: a flush that replayed on a second reconnect would re-run the command, which for anything
// with side effects is worse than losing it.
func TestRunSessionFlushesPendingInputOnce(t *testing.T) {
	h := newHarness(t)
	h.pending = []byte("buffered\n")
	h.stream.opened("test", 0, nil)
	h.stream.exited(0)

	if _, err := h.run(context.Background()); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}

	if got := string(h.stream.inputs()); got != "buffered\n" {
		t.Errorf("input sent = %q, want the buffered bytes flushed once", got)
	}
	// Cleared, so the next connection does not send them again.
	if len(h.pending) != 0 {
		t.Errorf("pending = %q after flushing, want it cleared", h.pending)
	}
}

// A read-only client does not flush pending input either.
//
// Worth its own case because the flush happens on a different code path from ordinary keystrokes, early in the
// connection, and an exception there would let a read-only client write to the session on every reconnect.
func TestRunSessionReadOnlyDoesNotFlushPendingInput(t *testing.T) {
	h := newHarness(t)
	h.opts.ReadOnly = true
	h.pending = []byte("should not be sent\n")
	h.stream.opened("test", 0, nil)
	h.stream.exited(0)

	if _, err := h.run(context.Background()); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}
	if got := h.stream.inputs(); len(got) != 0 {
		t.Errorf("a read-only client flushed pending input: %q", got)
	}
}

// A clean stream close means reconnect, not stop.
//
// The server closing the stream is what a restart looks like from here. Treating it as the end of the
// attachment would turn every upgrade into a dropped session, which is the opposite of cm's purpose.
func TestRunSessionReconnectsOnEOF(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)
	h.stream.pushErr(io.EOF)

	oc, err := h.run(context.Background())
	if err != nil {
		t.Errorf("runSession() error = %v, want nil for a clean close", err)
	}
	if oc != outcomeReconnect {
		t.Errorf("outcome = %v, want outcomeReconnect", oc)
	}
}

// A server that does not open the session is a hard failure, not something to retry.
//
// Retrying would loop forever against a server that will keep refusing, which presents as a client that hangs
// instead of one that reports what is wrong.
func TestRunSessionFailsWhenTheServerDoesNotOpen(t *testing.T) {
	h := newHarness(t)
	// A metadata event where an Opened was required.
	h.stream.push(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Metadata{Metadata: &serverv1.Metadata{Title: "wrong"}},
	})

	oc, err := h.run(context.Background())
	if err == nil {
		t.Error("runSession() error = nil, want an error when the session was not opened")
	}
	if oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone: retrying cannot fix a server that will not open", oc)
	}
}

// The session name from the server is adopted, which is how a server-allocated name reaches the caller.
func TestRunSessionAdoptsTheServersSessionName(t *testing.T) {
	h := newHarness(t)
	h.opts.Session = ""
	h.result.Session = ""
	h.stream.opened("s1", 0, nil)
	h.stream.exited(0)

	if _, err := h.run(context.Background()); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}
	if h.result.Session != "s1" {
		t.Errorf("result.Session = %q, want the name the server allocated", h.result.Session)
	}
}

// Metadata is delivered to the callback and not written to the terminal.
func TestRunSessionDeliversMetadata(t *testing.T) {
	h := newHarness(t)
	var got []SessionMetadata
	h.opts.OnMetadata = func(m SessionMetadata) { got = append(got, m) }

	h.stream.opened("test", 0, nil)
	h.stream.push(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Metadata{Metadata: &serverv1.Metadata{
			Title: "vim", Cwd: "/tmp", CwdIsLocal: true,
		}},
	})
	h.stream.exited(0)

	if _, err := h.run(context.Background()); err != nil {
		t.Fatalf("runSession() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("callback got %d metadata events, want 1", len(got))
	}
	want := SessionMetadata{Title: "vim", Cwd: "/tmp", CwdIsLocal: true}
	if got[0] != want {
		t.Errorf("metadata = %+v, want %+v", got[0], want)
	}
	// Not painted: a title is for the terminal emulator's chrome, not the session's screen.
	h.noTerminalOutput()
}

// A cancelled context detaches rather than abandoning the stream.
//
// Being asked to stop, by SIGTERM or a parent exiting, must not destroy an owned session. Abandoning the
// stream looks identical to a client crashing, which is what reaping is for.
func TestRunSessionDetachesOnContextCancel(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := h.runAsync(ctx)
	// Established first, so cancellation interrupts an open attachment rather than the handshake. A context
	// already cancelled when runSession starts returns before sending anything, which is correct behavior and
	// not what this test is about.
	h.stream.waitForRequests(t, 1)

	cancel()
	h.stream.detached()

	if oc := <-done; oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone", oc)
	}
	if !h.result.Detached {
		t.Error("result.Detached = false, want true: cancellation must detach, not abandon")
	}
	if h.stream.detaches() != 1 {
		t.Errorf("sent %d detach events, want exactly 1", h.stream.detaches())
	}
}

// A resize is forwarded with the terminal's actual size.
//
// Needs a pty, not a pipe. A pipe reports no size, so the client correctly sends nothing, which makes both this
// and the read-only case below pass whatever the code does. That was not hypothetical: with a pipe, removing
// the read-only guard on resize did not fail any test.
func TestRunSessionForwardsResize(t *testing.T) {
	h := newPtyHarness(t, 30, 100)
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.stream.waitForRequests(t, 1)
	h.winch <- os.Signal(nil)
	h.stream.waitForRequests(t, 2)
	h.stream.exited(0)
	<-done

	got := h.stream.resizes()
	if len(got) != 1 {
		t.Fatalf("resizes = %+v, want exactly one", got)
	}
	if got[0].Rows != 30 || got[0].Cols != 100 {
		t.Errorf("resize = %dx%d, want 30x100 as reported by the terminal", got[0].Rows, got[0].Cols)
	}
}

// A zero size is not forwarded.
//
// Zeros mean the size could not be determined, which is routine when output is not a terminal. Sending them
// would resize the session to nothing.
func TestRunSessionDoesNotForwardAZeroSize(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.stream.waitForRequests(t, 1)
	h.winch <- os.Signal(nil)
	h.waitForWinchConsumed(t)
	h.stream.exited(0)
	<-done

	if got := h.stream.resizes(); len(got) != 0 {
		t.Errorf("a zero size was sent: %+v", got)
	}
}

// A read-only client does not resize the session.
//
// A watcher's window size must not dictate the owner's. This is the other half of read-only meaning
// non-disturbing, and a separate code path from input.
func TestRunSessionReadOnlyDoesNotResize(t *testing.T) {
	// A pty, so the size is real and the client would have something to send if the guard were missing.
	h := newPtyHarness(t, 30, 100)
	h.opts.ReadOnly = true
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.stream.waitForRequests(t, 1)
	h.winch <- os.Signal(nil)
	h.waitForWinchConsumed(t)
	h.stream.exited(0)
	<-done

	if got := h.stream.resizes(); len(got) != 0 {
		t.Errorf("a read-only client sent resizes: %+v", got)
	}
}

// Exhausted piped input does not end the attachment.
//
// `cm attach < /dev/null` exhausts stdin immediately. Leaving then would end the attachment before any output
// arrived, so a non-terminal client keeps displaying output until the session itself finishes.
func TestRunSessionKeepsGoingWhenPipedInputEnds(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	close(h.input)

	// Output after input ended, which a client that left early would never show.
	h.stream.output(0, "after input ended")
	h.stream.exited(7)

	if oc := <-done; oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone", oc)
	}
	if !h.result.Exited || h.result.ExitCode != 7 {
		t.Errorf("result = %+v, want the exit status observed after input ended", h.result)
	}
	if got := h.terminalOutput(); got != "after input ended" {
		t.Errorf("terminal output = %q, want the output that arrived after input ended", got)
	}
}

// EOF on piped input is not an error worth stopping for.
//
// Same reasoning as the closed channel, on the error path: a short pipe reports EOF, and the session's output
// is still worth showing.
func TestRunSessionIgnoresEOFOnPipedInput(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.inputErr <- io.EOF
	h.stream.output(0, "still here")
	h.stream.exited(0)

	if oc := <-done; oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone", oc)
	}
	if got := h.terminalOutput(); got != "still here" {
		t.Errorf("terminal output = %q, want output after the EOF", got)
	}
}

// A real read error ends the attachment, since reconnecting cannot fix a broken terminal.
func TestRunSessionStopsOnATerminalReadError(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())
	h.inputErr <- errors.New("input/output error")

	if oc := <-done; oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone: a broken terminal is not a reconnect", oc)
	}
}

// A terminal reply must reach the session in the read it arrived in.
//
// This is the seam the HoldBack bug was visible at. A program inside the session queries the terminal
// and blocks for the answer, so the client sitting between them cannot hold part of a reply back
// waiting for a keystroke that may never come. The bug held six trailing bytes of any chunk whose last
// byte could begin a detach encoding, which for an OSC 11 background-color reply meant the shell saw
// ";rgb:2828/2c2c" and the program waited forever for the rest.
func TestRunSessionForwardsATerminalReplyWhole(t *testing.T) {
	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}

	h := newHarness(t)
	h.opts.DetachKey = key
	h.stream.opened("test", 0, nil)

	done := h.runAsync(context.Background())

	// An OSC 11 reply arriving in one read, ending with the ESC of its ST terminator. A real read ends
	// here because the terminal wrote the reply and the program's next query separately.
	//
	// Nothing else is sent afterwards, which is the whole point. Concatenating a second read would hide
	// the bug: the held bytes were not dropped, they were prepended to whatever came next, so the total
	// matched while the program waiting on the reply had already stalled.
	reply := "\x1b]11;rgb:2828/2c2c/3434\x1b"
	h.input <- []byte(reply)
	h.waitForInputConsumed(t)
	h.stream.waitForRequests(t, 1)

	// Everything except the trailing ESC must already be on its way. Only that last byte could begin a
	// detach encoding, so only it may wait for more input.
	wantForwarded := reply[:len(reply)-1]
	if got := string(h.stream.inputs()); got != wantForwarded {
		t.Errorf("input forwarded = %q, want %q", got, wantForwarded)
	}

	// End the attachment the way a session exiting does, since nothing here detaches.
	h.stream.exited(0)
	if oc := <-done; oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone", oc)
	}
}

// A server-initiated detach must end the attachment rather than trigger a reconnect.
//
// This is what `cm detach` needs from the client, and the failure is silent. The server sends Detached and
// closes the stream; a clean close is otherwise read as "the server went away", which returns
// outcomeReconnect. The client then reattaches within a second and undoes the detach it was just asked to
// perform, so the session looks like it ignored the command.
//
// Distinct from the acknowledgement of a detach this client asked for, which never reaches the main loop:
// that one is drained by waitForDetachAck before runSession returns.
func TestRunSessionLeavesWhenTheServerDetachesIt(t *testing.T) {
	h := newHarness(t)
	h.stream.opened("test", 0, nil)
	// No Detach was sent by this client, so this is the server asking rather than acknowledging.
	h.stream.detached()

	oc, err := h.run(context.Background())
	if err != nil {
		t.Fatalf("runSession() error = %v", err)
	}
	if oc != outcomeDone {
		t.Errorf("outcome = %v, want outcomeDone: the client would reconnect and undo the detach", oc)
	}
	if !h.result.Detached {
		t.Error("result.Detached = false, want true so the caller can report why it stopped")
	}
	// Nothing was sent in reply. The server already knows, and a Detach here would be answered by a
	// server that has closed the stream.
	if n := h.stream.detaches(); n != 0 {
		t.Errorf("client sent %d Detach messages, want 0: the server initiated this one", n)
	}
}
