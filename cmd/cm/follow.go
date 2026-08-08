package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/chancez/cm/internal/ansi"
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
// Escape sequences are stripped unless raw is set, matching `cm read`. The point of following a session is
// usually to see what a command printed, and colour codes and cursor moves in a redirected log are noise at
// best. --raw keeps them for the case where the sequences are the interesting part.
//
// Stripping is a byte filter rather than a terminal model, because a stream cannot re-render a screen per byte.
// So output from a program that repaints in place -- a progress bar, a full-screen TUI -- comes out as every
// frame concatenated rather than overwritten. That is the tradeoff, and why raw stays available.
//
// Read-only always. A follower must not be able to disturb the session it is watching, and this is called from
// commands whose stdin is not the session's input.
func followSession(ctx context.Context, dirs paths.Dirs, session string, raw bool, log *slog.Logger) error {
	opts := client.Options{
		Log:        log,
		Session:    session,
		SocketPath: dirs.ServerSocket(),
		ReadOnly:   true,
		// No detach key: this is not an interactive attachment, and reserving a keystroke from a stream being
		// piped would swallow a byte of output.
		DetachKey: client.DetachKeySpec{Name: "none", Disabled: true},
		// No repaint. A follower streams what happens next; the screen as it stands now is either already
		// printed by the caller or deliberately not wanted.
		NoRestore: true,
		Output:    followWriter(raw),
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
	ctx context.Context, dirs paths.Dirs, session string, tail []byte, raw bool, log *slog.Logger,
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
	return followSession(ctx, dirs, session, raw, log)
}

// followWriter returns where a follower's output goes.
//
// Stripped by default, raw on request. Returned as a writer rather than branching at each write site so both
// followers share one decision.
func followWriter(raw bool) io.Writer {
	if raw {
		return os.Stdout
	}
	return ansi.NewStripper(os.Stdout)
}

// followWarning is printed when --follow writes straight to a terminal.
//
// With --raw, a follower emits the session's escape sequences, which for a full-screen program includes
// alternate-screen and cursor-movement aimed at a terminal this process does not own, so it can repaint over
// the shell. The command that owns a terminal properly is `cm attach --read-only`.
//
// Only for --raw: stripped output is plain text and safe to print anywhere, which is why it is the default.
func followWarning() string {
	return fmt.Sprintf(
		"warning: --raw writes the session's escape sequences, which can disturb a terminal; "+
			"redirect it, drop --raw, or use `%s attach --read-only` to watch interactively", paths.Name)
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
	raw bool,
	log *slog.Logger,
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
		streamed <- followSessionSignalling(streamCtx, dirs, session, attached, raw, log)
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
	ctx context.Context, dirs paths.Dirs, session string, ready chan<- struct{}, raw bool, log *slog.Logger,
) error {
	var once sync.Once
	opts := client.Options{
		Log:        log,
		Session:    session,
		SocketPath: dirs.ServerSocket(),
		ReadOnly:   true,
		DetachKey:  client.DetachKeySpec{Name: "none", Disabled: true},
		NoRestore:  true,
		Output:     followWriter(raw),
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

// createWithoutAttaching opens a session, prints its name, and leaves.
//
// For pre-creating a session something else will attach to: a terminal emulator laying out windows, or a script
// that wants a shell waiting before it sends anything.
//
// Distinct from `cm run -d` in ways that matter. That needs a command, sets CaptureOutput so the output is kept
// for a few minutes, and leaves a session whose lifetime is a finished command's. This creates an ordinary
// session with the caller's own options -- persistence, restore behavior, environment -- so what comes back is
// what `cm attach` would have made.
//
// Detaches explicitly rather than dropping the connection, matching `cm run`. Not load-bearing here, and the
// comment claimed otherwise until it was measured: an abandoned stream only destroys a session that is owned,
// and Own is false below, so removing the detach changes nothing observable. Kept because it states the intent
// -- this client is leaving on purpose -- and because the reason Own is false today is a choice rather than a
// law, so a future change that made these sessions owned would otherwise turn a tidy exit into a destroyed
// session.
func createWithoutAttaching(ctx context.Context, dirs paths.Dirs, opts client.Options) error {
	// opts carries the logger already, since this is called from attach, which opens one.
	if err := ensureServer(ctx, dirs); err != nil {
		return err
	}

	conn, cl, err := dialServer(dirs)
	if err != nil {
		return err
	}
	defer conn.Close()

	stream, err := cl.Attach(ctx)
	if err != nil {
		return err
	}

	if err := stream.Send(&serverv1.AttachRequest{
		Event: &serverv1.AttachRequest_Open{
			Open: &serverv1.Open{
				Session: opts.Session,
				Command: opts.Command,
				Cwd:     opts.Dir,
				// A conventional size, since there is no terminal to ask. A client attaching later resizes
				// it, and a program that checks in the meantime gets a plausible answer rather than zeros.
				Rows: 24,
				Cols: 80,
				// Never owned: an owning client ends its session on disconnect, and this disconnects
				// immediately, which would defeat the point.
				Own:       false,
				Persist:   opts.Persist,
				OnRestore: opts.OnRestore,
				Env:       opts.Env,
				ClientEnv: opts.ClientEnv,
				// No repaint to receive, since nothing is being painted.
				NoRestore: true,
			},
		},
	}); err != nil {
		return err
	}

	resp, err := stream.Recv()
	if err != nil {
		return err
	}
	opened := resp.GetOpened()
	if opened == nil {
		return errors.New("server did not open the session")
	}

	_ = stream.Send(&serverv1.AttachRequest{
		Event: &serverv1.AttachRequest_Detach{Detach: &serverv1.Detach{}},
	})

	// The name, since the caller may not have chosen one and needs it to attach later. Printed even when the
	// session already existed, because the useful answer to "make sure this exists" is its name either way.
	_, err = fmt.Println(opened.Session)
	return err
}
