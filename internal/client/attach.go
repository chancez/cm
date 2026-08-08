package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/containerd/ttrpc"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

const (
	// reconnectTimeout bounds how long to keep trying to reach the server before giving
	// up. Generous enough to cover a restart or an upgrade, short enough that a client
	// whose server is gone for good does not hang indefinitely.
	reconnectTimeout = 30 * time.Second
	// reconnectInterval is how often to retry while the server is away.
	reconnectInterval = 100 * time.Millisecond
	// inputReadSize bounds a single read of keystrokes. Human input is small; this only
	// needs to cover a paste arriving in one read.
	inputReadSize = 4096
)

// Options configures an attachment.
type Options struct {
	// SocketPath is the server's socket.
	SocketPath string
	// Session to attach to. Empty asks the server to allocate a name.
	Session string
	// Own makes the session end if this client disconnects without detaching, which is how
	// a terminal emulator gets a session per window that is cleaned up on close.
	Own bool
	// ReadOnly follows the session without sending input.
	ReadOnly bool
	// Command overrides the shell when creating.
	Command []string
	// Dir is the working directory when creating.
	Dir string
	// Env holds extra KEY=VALUE entries when creating.
	Env []string
	// ClientEnv holds terminal-related variables from this client's environment, recorded by the
	// server so a shell inside the session can refresh them later.
	ClientEnv map[string]string
	// DetachKey is the key that detaches. Zero value means the default.
	DetachKey DetachKeySpec
	// OnMetadata, when set, is called as the session reports its title and directory.
	//
	// This is how a terminal emulator learns values the shell reported to cm rather than to the
	// terminal, so it can retitle a tab or open a new window in the right place.
	OnMetadata func(SessionMetadata)
}

// SessionMetadata is what a session reports about itself.
type SessionMetadata struct {
	Title string
	// Cwd is the decoded working directory, empty if the shell has not reported one.
	Cwd string
	// CwdIsLocal is false once a session has ssh'd elsewhere, in which case acting on Cwd
	// locally is wrong.
	CwdIsLocal bool
}

// Result describes how an attachment ended.
type Result struct {
	// Session is the resolved name, which matters when the server allocated it.
	Session string
	// Detached is true when the user detached rather than the session ending.
	Detached bool
	// Exited is true when the session's shell exited.
	Exited   bool
	ExitCode int
}

// Attach connects a terminal to a session and runs until detach or session end.
//
// The terminal is put into raw mode once and restored once, around the whole attachment
// including reconnects. That is deliberate: a server restart should look like the session
// briefly freezing, not like the client exiting and starting again.
func Attach(ctx context.Context, tty *TTY, opts Options) (Result, error) {
	var result Result
	result.Session = opts.Session

	// resumeFrom tracks how far output has been consumed, so a reconnect asks for exactly
	// what was missed instead of a fresh repaint.
	var resumeFrom *uint64

	// Keystrokes typed while the server is away are held here and flushed on reconnect, so
	// a freeze does not silently swallow input.
	var pending []byte

	// SIGWINCH is the only way a client learns its terminal was resized.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	// Reading the terminal must not block the reconnect loop, so input is read once by a
	// long-lived goroutine and delivered over a channel. A read blocked in the kernel
	// cannot be cancelled, which is why this goroutine outlives individual connections.
	input := make(chan []byte, 16)
	inputErr := make(chan error, 1)
	go readInput(tty, input, inputErr)

	deadline := time.Time{}
	for {
		conn, cl, err := dial(opts.SocketPath)
		if err != nil {
			// The server is unreachable. On a first attempt that is a hard failure; while
			// reconnecting it is expected, so keep trying until the deadline.
			if deadline.IsZero() {
				return result, fmt.Errorf("connecting to server: %w", err)
			}
			if time.Now().After(deadline) {
				return result, fmt.Errorf("server did not return within %s: %w",
					reconnectTimeout, err)
			}
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(reconnectInterval):
			}
			continue
		}

		outcome, err := runSession(ctx, tty, cl, opts, &result, &resumeFrom, &pending, winch, input, inputErr)
		conn.Close()

		switch outcome {
		case outcomeDone:
			return result, err
		case outcomeReconnect:
			// Start the clock on the first failure, so the budget covers the outage rather
			// than resetting on every retry.
			if deadline.IsZero() {
				deadline = time.Now().Add(reconnectTimeout)
			}
			select {
			case <-ctx.Done():
				return result, ctx.Err()
			case <-time.After(reconnectInterval):
			}
		}
	}
}

type outcome int

const (
	// outcomeDone means the attachment is over: detached, session ended, or a real error.
	outcomeDone outcome = iota
	// outcomeReconnect means the connection dropped but the session may still exist.
	outcomeReconnect
)

// runSession runs one connection's worth of attachment.
func runSession(
	ctx context.Context,
	tty *TTY,
	cl serverv1.ServerClient,
	opts Options,
	result *Result,
	resumeFrom **uint64,
	pending *[]byte,
	winch <-chan os.Signal,
	input <-chan []byte,
	inputErr <-chan error,
) (outcome, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := cl.Attach(ctx)
	if err != nil {
		return outcomeReconnect, err
	}

	rows, cols := tty.Size()
	open := &serverv1.Open{
		Session:       result.Session,
		Rows:          uint32(rows),
		Cols:          uint32(cols),
		Own:           opts.Own,
		ReadOnly:      opts.ReadOnly,
		Command:       opts.Command,
		Cwd:           opts.Dir,
		Env:           opts.Env,
		ClientEnv:     opts.ClientEnv,
		ResumeFromSeq: *resumeFrom,
	}
	if err := stream.Send(&serverv1.AttachRequest{
		Event: &serverv1.AttachRequest_Open{Open: open},
	}); err != nil {
		return outcomeReconnect, err
	}

	// The server's first reply names the session and carries any state to repaint.
	first, err := stream.Recv()
	if err != nil {
		return outcomeReconnect, err
	}
	opened := first.GetOpened()
	if opened == nil {
		return outcomeDone, errors.New("server did not open the session")
	}
	result.Session = opened.Session

	if len(opened.Restore) > 0 {
		// Clear first so restored state is not painted over whatever the client's own
		// shell left on screen.
		if err := tty.Clear(); err != nil {
			return outcomeDone, err
		}
		if _, err := tty.Write(opened.Restore); err != nil {
			return outcomeDone, err
		}
	}
	seq := opened.NextSeq
	*resumeFrom = &seq

	// Flush anything typed while the server was away.
	if len(*pending) > 0 && !opts.ReadOnly {
		if err := stream.Send(&serverv1.AttachRequest{
			Event: &serverv1.AttachRequest_Input{Input: &serverv1.Input{Data: *pending}},
		}); err != nil {
			return outcomeReconnect, err
		}
		*pending = nil
	}

	// Output arrives on its own goroutine so input handling is never blocked behind a
	// slow write to the terminal.
	type outMsg struct {
		resp *serverv1.AttachResponse
		err  error
	}
	out := make(chan outMsg, 16)
	go func() {
		defer close(out)
		for {
			resp, err := stream.Recv()
			select {
			case out <- outMsg{resp, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// held buffers a partial detach sequence across reads, so a CSI-encoded detach split
	// between two reads is still recognized rather than forwarded to the shell.
	var held []byte

	detachKey := opts.DetachKey
	if detachKey.Name == "" {
		// Zero value means the caller did not configure one.
		detachKey, _ = ParseDetachKey(DefaultDetachKey)
	}

	for {
		select {
		case msg, ok := <-out:
			if !ok {
				return outcomeReconnect, nil
			}
			if msg.err != nil {
				if errors.Is(msg.err, io.EOF) {
					// The server closed the stream cleanly. Without knowing why, assume
					// the server went away and try to resume.
					return outcomeReconnect, nil
				}
				return outcomeReconnect, msg.err
			}
			if ex := msg.resp.GetExited(); ex != nil {
				result.Exited = true
				result.ExitCode = int(ex.ExitCode)
				return outcomeDone, nil
			}
			if m := msg.resp.GetMetadata(); m != nil {
				if opts.OnMetadata != nil {
					opts.OnMetadata(SessionMetadata{
						Title:      m.Title,
						Cwd:        m.Cwd,
						CwdIsLocal: m.CwdIsLocal,
					})
				}
				continue
			}
			if o := msg.resp.GetOutput(); o != nil {
				if _, err := tty.Write(o.Data); err != nil {
					return outcomeDone, err
				}
				next := o.Seq + uint64(len(o.Data))
				*resumeFrom = &next
			}

		case data, ok := <-input:
			if !ok {
				// Input ended. When stdin is a terminal that means the window is gone, and
				// leaving is right: an owning client disconnecting without detaching is what
				// ends its session.
				//
				// When stdin is not a terminal it just means the input is exhausted, which
				// happens immediately with `< /dev/null` or a short pipe. Leaving then would
				// end the attachment before any output arrived, so keep displaying output and
				// wait for the session itself to finish.
				if tty.IsTerminal() {
					return outcomeDone, nil
				}
				input = nil // stop selecting on a closed channel
				continue
			}

			buf := append(held, data...)
			held = nil

			if i := detachKey.Find(buf); i >= 0 {
				// Forward whatever preceded the detach so a trailing keystroke is not
				// lost, then leave.
				if i > 0 && !opts.ReadOnly {
					_ = stream.Send(&serverv1.AttachRequest{
						Event: &serverv1.AttachRequest_Input{
							Input: &serverv1.Input{Data: buf[:i]},
						},
					})
				}
				// Tell the server this was deliberate, so an owned session survives.
				_ = stream.Send(&serverv1.AttachRequest{
					Event: &serverv1.AttachRequest_Detach{Detach: &serverv1.Detach{}},
				})
				result.Detached = true
				return outcomeDone, nil
			}

			// Hold back a possible partial detach sequence until the rest arrives.
			if keep := detachKey.HoldBack(buf); keep > 0 && keep <= len(buf) {
				held = append(held, buf[len(buf)-keep:]...)
				buf = buf[:len(buf)-keep]
			}

			if len(buf) > 0 && !opts.ReadOnly {
				if err := stream.Send(&serverv1.AttachRequest{
					Event: &serverv1.AttachRequest_Input{Input: &serverv1.Input{Data: buf}},
				}); err != nil {
					// The server went away mid-keystroke. Hold the bytes so they are not
					// lost across the reconnect.
					*pending = append(*pending, buf...)
					return outcomeReconnect, nil
				}
			}

		case err := <-inputErr:
			// EOF on a non-terminal stdin is exhausted input, not a reason to stop: the
			// session's output is still worth displaying until it ends.
			if errors.Is(err, io.EOF) && !tty.IsTerminal() {
				inputErr = nil
				continue
			}
			// Reading the terminal failed, which reconnecting cannot fix.
			if err != nil && !errors.Is(err, io.EOF) {
				return outcomeDone, err
			}
			return outcomeDone, nil

		case <-winch:
			if opts.ReadOnly {
				continue
			}
			rows, cols := tty.Size()
			if rows == 0 || cols == 0 {
				continue
			}
			if err := stream.Send(&serverv1.AttachRequest{
				Event: &serverv1.AttachRequest_Resize{
					Resize: &serverv1.Resize{Rows: uint32(rows), Cols: uint32(cols)},
				},
			}); err != nil {
				return outcomeReconnect, nil
			}

		case <-ctx.Done():
			// Interrupted, such as by SIGTERM. Detach rather than abandoning the stream so
			// an owned session is not destroyed by the client being asked to stop.
			_ = stream.Send(&serverv1.AttachRequest{
				Event: &serverv1.AttachRequest_Detach{Detach: &serverv1.Detach{}},
			})
			result.Detached = true
			return outcomeDone, nil
		}
	}
}

// readInput forwards terminal input until it fails.
//
// A read blocked in the kernel cannot be cancelled, so this goroutine is intentionally
// allowed to outlive a connection and is reused across reconnects.
func readInput(tty *TTY, out chan<- []byte, errc chan<- error) {
	defer close(out)
	buf := make([]byte, inputReadSize)
	for {
		n, err := tty.Read(buf)
		if n > 0 {
			// Copy: the buffer is reused on the next read.
			data := make([]byte, n)
			copy(data, buf[:n])
			out <- data
		}
		if err != nil {
			errc <- err
			return
		}
	}
}

// dial connects to the server's socket.
func dial(socketPath string) (*ttrpc.Client, serverv1.ServerClient, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, nil, err
	}
	cl := ttrpc.NewClient(conn)
	return cl, serverv1.NewServerClient(cl), nil
}
