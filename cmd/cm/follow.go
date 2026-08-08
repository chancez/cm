package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/chancez/cm/internal/client"
	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// warnIfTerminal prints followWarning when w is a terminal.
func warnIfTerminal(w *os.File) {
	if term.IsTerminal(int(w.Fd())) {
		fmt.Fprintln(os.Stderr, followWarning())
	}
}

// followSession streams a session's raw output until it ends or the caller stops.
//
// Built on a read-only attach rather than a new streaming RPC, because that path already does exactly this:
// with stdout not a terminal it writes raw pty bytes and returns when the session ends. Verified before
// building anything -- piping `cm attach --read-only` already produced the wanted behavior, so what --follow
// adds is a name for it, the right defaults, and a stop condition.
//
// Raw bytes, deliberately, which is what makes this like `tail -f` and unlike `cm read`. The output carries
// whatever the program emitted, escape sequences included, so a build that repaints a progress line looks the
// way it did live. The cost is that it is less clean to pipe into a parser than cm read's rendered lines; use
// cm read for that.
//
// Read-only always. A follower must not be able to disturb the session it is watching, and this is called from
// commands whose stdin is not the session's input.
func followSession(ctx context.Context, dirs paths.Dirs, session string) error {
	opts := client.Options{
		Session:    session,
		SocketPath: dirs.ServerSocket(),
		ReadOnly:   true,
		// No detach key: this is not an interactive attachment, and reserving a keystroke from a stream being
		// piped would swallow a byte of output.
		DetachKey: client.DetachKeySpec{Name: "none", Disabled: true},
		// No repaint. A follower streams what happens next; the screen as it stands now is either already
		// printed by the caller or deliberately not wanted.
		NoRestore: true,
	}

	tty, err := client.OpenTTY(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}

	res, attachErr := client.Attach(ctx, tty, opts)
	closeErr := tty.Close()

	switch {
	case attachErr != nil && errors.Is(attachErr, context.Canceled):
		// Interrupted, which is how a follower normally stops. Not an error: the caller asked to watch and
		// then stopped watching.
		return nil
	case attachErr != nil:
		return attachErr
	case closeErr != nil:
		return closeErr
	}

	// The exit status is propagated, so `cm send --follow` can be used in a script the way a local command
	// would be. Reported through exitCodeError rather than printed, since a caller wants the status.
	if res.Exited && res.ExitCode != 0 {
		return &exitCodeError{code: res.ExitCode, reported: true}
	}
	return nil
}

// printTailThenFollow prints a session's recent output and then streams what comes next.
//
// The two halves differ in kind, which is worth stating plainly rather than hiding: the tail is a rendered
// screen with soft-wrapped lines rejoined, and what follows is raw bytes. Rendering the tail is what makes it
// readable, and re-rendering on every new byte would repaint rather than append, which is wrong for something
// being piped to a file.
//
// The consequence is a possible seam where the two meet: the rendered tail may end mid-screen while the raw
// stream picks up at the session's current position. In practice they line up, because the tail ends where the
// session is now, which is where the stream begins.
func printTailThenFollow(
	ctx context.Context, dirs paths.Dirs, session string, tail []byte,
) error {
	if _, err := os.Stdout.Write(tail); err != nil {
		return err
	}
	// A newline when the render lacks one, so the streamed output does not begin on the same line as the last
	// rendered one.
	if n := len(tail); n > 0 && tail[n-1] != '\n' {
		if _, err := fmt.Fprintln(os.Stdout); err != nil {
			return err
		}
	}
	return followSession(ctx, dirs, session)
}

// followWarning is printed when --follow writes straight to a terminal.
//
// A follower emits raw pty output, which for a full-screen program includes alternate-screen and
// cursor-movement sequences aimed at a terminal this process does not own. Piped or redirected, that is
// exactly right. On a terminal it can repaint over the shell, and the command that owns the terminal properly
// is `cm attach --read-only`.
//
// A warning rather than an error, because plenty of sessions emit nothing but plain lines and following one on
// a terminal is then perfectly useful. Refusing would be the tool deciding it knows better; saying so on
// stderr leaves the choice with the caller and keeps stdout clean for the output itself.
func followWarning() string {
	return fmt.Sprintf(
		"warning: --follow writes raw session output, which can disturb a terminal; "+
			"redirect it, or use `%s attach --read-only` to watch interactively", paths.Name)
}

// sendAndFollow sends input, streams the session's output, and returns when the command finishes.
//
// The ordering is the whole design. The follower is attached before the input is sent, because a command can
// start and finish faster than a second connection can be made: attaching afterwards loses whatever it printed
// in between, which for something quick is all of it. This is the same reasoning that made the server arm a
// --wait before writing the input, and it has the same failure mode -- a race that looks like missing output
// rather than an error.
//
// The send and the wait stay one request, which took a wrong turn to establish. Splitting them so the send
// returns immediately looked tidier and reintroduced the exact race the combined call exists to prevent: a bare
// Wait for idle is satisfied by a session that is already idle, so it resolves before the command has begun and
// the stream is cut off. Only the combined request carries the "after this input" qualifier, which is
// server-side because that is the only place the ordering can be guaranteed.
//
// So the request runs on its own goroutine while the stream carries the output, and its return is what says the
// command is done.
func sendAndFollow(
	ctx context.Context,
	dirs paths.Dirs,
	session, data string,
	until serverv1.WaitState,
	timeout time.Duration,
) error {
	if err := ensureServer(ctx, dirs); err != nil {
		return err
	}

	// The stream is stopped by cancelling its context once the send-and-wait returns, so it needs its own.
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()

	streamed := make(chan error, 1)
	attached := make(chan struct{})
	go func() {
		streamed <- followSessionSignalling(streamCtx, dirs, session, attached)
	}()

	// Wait for the attachment before sending, or attaching first buys nothing.
	select {
	case <-attached:
	case err := <-streamed:
		// The follower ended before it was ready, which is a real failure: the session may not exist.
		if err != nil {
			return err
		}
		return fmt.Errorf("following %s ended before the input was sent", session)
	case <-ctx.Done():
		return ctx.Err()
	}

	conn, cl, err := dialServer(dirs)
	if err != nil {
		return err
	}
	defer conn.Close()

	// One request, carrying both the input and the state to wait for. It returns when the command is done.
	resp, sendErr := cl.Send(ctx, &serverv1.SendRequest{
		Session:       session,
		Data:          []byte(data),
		WaitUntil:     until,
		WaitTimeoutMs: uint64(timeout.Milliseconds()),
	})

	// Warn when the session had never reported a command, because the wait then never resolves.
	//
	// cm derives busy and idle from OSC 133, which a shell only sends with terminal integration loaded. Without
	// it a session is permanently idle as far as cm can tell, so waiting for idle after input waits forever.
	// That is existing --wait behavior and defensible on its own, but --follow implies --wait idle, so it turns
	// a documented pitfall into a command that prints nothing and never returns.
	//
	// The flag comes from the server rather than being worked out here, which was the first attempt and was
	// wrong: the fields a client can read -- busy, and the current command -- are cleared once a command
	// finishes, so a session that had just run something fast looked like it never reported. The server has a
	// monotonic count of commands seen, which is the signal that trap was introduced for.
	//
	// Printed after the call rather than before, since that is when the answer is available, and regardless of
	// --timeout: a command that times out for no visible reason is still confusing, and a bound is not an
	// explanation.
	if resp != nil && !resp.ShellReports {
		fmt.Fprintf(os.Stderr,
			"warning: %s had not reported a command via OSC 133, so --follow can wait indefinitely; "+
				"load your shell integration, use --timeout, or report state with `%s report`\n",
			session, paths.Name)
	}

	// Stopped whether or not the send failed, so a failure does not leave the stream running.
	stopStream()
	streamErr := <-streamed

	if sendErr != nil {
		return sendErr
	}
	return streamErr
}

// followSessionSignalling is followSession, closing ready once the attachment is established.
//
// Needed so the caller can order a send after the stream is live, rather than guessing with a sleep.
//
// Signalled from OnAttached rather than OnMetadata, which was the first attempt and hung: metadata arrives only
// when the session reports a title or directory, so a session sitting quietly never fired it and the send never
// happened. It presented as `cm send --follow` producing no output at all and never returning, which is worse
// than a visible failure. OnAttached fires on the server's Opened reply, which is unconditional and always
// first.
func followSessionSignalling(
	ctx context.Context, dirs paths.Dirs, session string, ready chan<- struct{},
) error {
	var once sync.Once
	opts := client.Options{
		Session:    session,
		SocketPath: dirs.ServerSocket(),
		ReadOnly:   true,
		DetachKey:  client.DetachKeySpec{Name: "none", Disabled: true},
		NoRestore:  true,
		OnAttached: func() {
			once.Do(func() { close(ready) })
		},
	}

	tty, err := client.OpenTTY(os.Stdin, os.Stdout)
	if err != nil {
		return err
	}

	res, attachErr := client.Attach(ctx, tty, opts)
	closeErr := tty.Close()

	// Closed here too, so a session that reports no metadata still releases the caller rather than deadlocking
	// it until the context is cancelled.
	once.Do(func() { close(ready) })

	switch {
	case attachErr != nil && errors.Is(attachErr, context.Canceled):
		return nil
	case attachErr != nil:
		return attachErr
	case closeErr != nil:
		return closeErr
	}
	if res.Exited && res.ExitCode != 0 {
		return &exitCodeError{code: res.ExitCode, reported: true}
	}
	return nil
}
