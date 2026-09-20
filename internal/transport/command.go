package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/ttrpc"
)

// ProxyProtocol is the version of the stdio proxy handshake this build speaks.
//
// Sent by the proxy and checked by the dialer, so a mismatch is a refusal naming both numbers rather
// than a stream that decodes into nonsense. It is deliberately separate from cm's version: two builds
// differing does not mean the handshake changed, and pinning remote access to an exact build would make
// upgrading one machine at a time impossible.
const ProxyProtocol = 1

// proxyBannerPrefix opens the line a proxy writes before it forwards anything.
const proxyBannerPrefix = "cm-proxy "

// ProxyBanner is the line a proxy writes once it has a server to forward to.
//
// A banner exists so that dialing means "there is a server on the other end" rather than "a process
// started". Without one, the only signal is the first RPC failing, and the caller cannot tell an
// unreachable host from a server that is merely slow to appear: the client's reconnect loop treats a
// successful dial as proof it should keep waiting, so a mistyped hostname would be waited on forever
// instead of reported.
//
// It carries the remote's version because this is the one moment both ends are known at once, and a
// remote cm too old to have a proxy at all fails here, where the message can say so.
func ProxyBanner(version string) string {
	return fmt.Sprintf("%s%d %s\n", proxyBannerPrefix, ProxyProtocol, version)
}

// proxyReadyTimeout bounds how long a proxy has to write its banner.
//
// Generous, because everything slow happens before the banner: an ssh connection and its
// authentication, then a remote server that may have to be started, which has its own ten-second
// readiness timeout. A number below the sum of those turns a working remote into a failure under load.
const proxyReadyTimeout = 30 * time.Second

// diagTail is how much of a child's stderr is kept for reporting.
//
// Bounded because the child lives as long as the connection: an ssh logging on every keepalive, over an
// attachment left open for days, would otherwise grow this without limit.
const diagTail = 4096

// DialCommand runs a program and speaks to a cm server through its standard input and output.
//
// The program is the transport. `ssh host cm server proxy` reaches a server on another machine without
// cm listening on any network and without credentials of its own, borrowing ssh's authentication
// instead: docs/rpc.md records why that is the only remote access cm offers.
//
// This costs no protocol change, which is the measured fact the whole approach rests on. ttrpc.NewClient
// takes a net.Conn, and the only unix-specific code in ttrpc v1.2.9 is a server-side credentials
// handshaker cm does not use, so a pair of pipes is as good a client connection as a socket.
//
// ctx bounds the life of the process, not just the dial: cancelling it kills the program, which is what
// makes a cancelled attach take its ssh with it.
//
// Returns the concrete *ttrpc.Client for the same reason DialTTRPC does, and reports the remote's
// version so a caller can log which build answered.
func DialCommand(ctx context.Context, name string, args ...string) (*ttrpc.Client, string, error) {
	conn, version, err := dialCommand(ctx, name, args...)
	if err != nil {
		return nil, "", err
	}
	return ttrpc.NewClient(conn), version, nil
}

// dialCommand starts the program and completes the handshake.
func dialCommand(ctx context.Context, name string, args ...string) (*commandConn, string, error) {
	// os.Pipe rather than cmd.StdinPipe and cmd.StdoutPipe, for two reasons. This keeps ownership of the
	// descriptors it holds, where cmd.Wait closes the ones it handed out, and reading a pipe after Wait
	// closed it is "file already closed" rather than the EOF a caller expects. And an *os.File is
	// pollable, so SetDeadline is real on this connection rather than something it has to refuse.
	childIn, ourWrite, err := os.Pipe()
	if err != nil {
		return nil, "", fmt.Errorf("creating a pipe to %s: %w", name, err)
	}
	ourRead, childOut, err := os.Pipe()
	if err != nil {
		childIn.Close()
		ourWrite.Close()
		return nil, "", fmt.Errorf("creating a pipe from %s: %w", name, err)
	}

	// Captured rather than inherited, and that is load-bearing twice over. ssh writes "Permission denied
	// (publickey)" and host key warnings to stderr, and an inherited stderr is a second writer to the
	// terminal an attached client is painting: see the one-writer-per-stream rule in docs/architecture.md,
	// where a dependency logging to stderr printed itself into a live session. Discarding it instead is the
	// other mistake, the one ensureServer records against sending a server's startup errors to /dev/null,
	// which made every way it could fail look identical.
	ourDiag, childErr, err := os.Pipe()
	if err != nil {
		childIn.Close()
		childOut.Close()
		ourWrite.Close()
		ourRead.Close()
		return nil, "", fmt.Errorf("creating a pipe for %s's diagnostics: %w", name, err)
	}

	diag := &tailBuffer{}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = childIn
	cmd.Stdout = childOut
	// An *os.File rather than the tailBuffer directly, which is the whole reason that pipe is made by hand.
	// os/exec creates a pipe and a copying goroutine for any Stderr that is not a file, and Wait then waits
	// for that goroutine, which ends only when every process holding the write end has closed it. That is
	// not just the child: measured at 8.01s for a grandchild with eight seconds left to live, after the
	// child itself had already been killed. An ssh with a ProxyCommand or a ProxyJump is exactly that
	// shape, so closing such a connection would have blocked for as long as the jump host's ssh lived.
	cmd.Stderr = childErr
	// A second guard on the same hazard, for anything else that might leave Wait blocked on I/O after the
	// process is gone. Without a WaitDelay, Wait waits forever.
	cmd.WaitDelay = reapGrace

	if err := cmd.Start(); err != nil {
		childIn.Close()
		childOut.Close()
		childErr.Close()
		ourWrite.Close()
		ourRead.Close()
		ourDiag.Close()
		return nil, "", fmt.Errorf("running %s: %w", name, err)
	}
	// The child holds its own copies now, and these must go or no pipe ever reports EOF: a read would
	// block forever on a descriptor this process is still keeping open.
	childIn.Close()
	childOut.Close()
	childErr.Close()

	conn := &commandConn{name: name, cmd: cmd, w: ourWrite, r: ourRead, diagPipe: ourDiag, diag: diag}
	conn.drainDiag()

	version, err := conn.readBanner(ctx)
	if err != nil {
		// Closed and collected here rather than left to the caller: no connection escapes on this path, so
		// nothing else will ever call Close, and an uncollected child is a zombie per dial attempt.
		conn.Close()
		return nil, "", err
	}
	return conn, version, nil
}

// commandConn is a net.Conn carried by a child process's standard input and output.
type commandConn struct {
	name string
	cmd  *exec.Cmd

	// w is the child's stdin and r is its stdout, both held as *os.File so deadlines work.
	w *os.File
	r *os.File

	// diagPipe is the child's stderr and diag is the tail of what it wrote there, which is where the
	// reason for a failure is.
	diagPipe *os.File
	diag     *tailBuffer
	diagDone chan struct{}

	closeOnce sync.Once
	closeErr  error
}

// drainDiag copies the child's stderr into diag until the pipe closes.
//
// A goroutine of cm's own rather than letting os/exec make one, because exec's is waited for by Wait and
// this one is not: see the measurement at cmd.Stderr above. It ends when Close closes the read end, which
// interrupts a blocked Read rather than waiting for whatever still holds the write end.
func (c *commandConn) drainDiag() {
	c.diagDone = make(chan struct{})
	go func() {
		defer close(c.diagDone)

		buf := make([]byte, 512)
		for {
			n, err := c.diagPipe.Read(buf)
			if n > 0 {
				_, _ = c.diag.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
}

// diagFlushGrace is how long an error message waits for the last of a child's stderr.
//
// Small, and only ever reached in one case. By the time this is waited on the child has been collected, so
// everything it wrote is already in the pipe and the copy is a syscall away; what can still delay the EOF
// is another process holding the write end, and the grandchild measurement above is exactly why this does
// not wait for that.
const diagFlushGrace = 200 * time.Millisecond

// said reports what the child wrote to stderr, having waited briefly for the last of it.
func (c *commandConn) said() string {
	select {
	case <-c.diagDone:
	case <-time.After(diagFlushGrace):
	}
	return c.diag.String()
}

// readBanner waits for the proxy to report itself, and returns the version it reported.
func (c *commandConn) readBanner(ctx context.Context) (string, error) {
	deadline := time.Now().Add(proxyReadyTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.r.SetReadDeadline(deadline); err != nil {
		return "", fmt.Errorf("bounding the wait for %s: %w", c.name, err)
	}

	// A byte at a time, to a newline, so not one byte of what follows is consumed. The alternative, a
	// buffered reader kept for the life of the connection, would copy every pty chunk through an extra
	// buffer to save about thirty syscalls once.
	var line strings.Builder
	buf := make([]byte, 1)
	for line.Len() <= len(proxyBannerPrefix)+64 {
		n, err := c.r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				// Clearing the deadline, because from here the connection is ttrpc's and a stream can
				// legitimately sit idle for hours.
				if err := c.r.SetReadDeadline(time.Time{}); err != nil {
					return "", fmt.Errorf("clearing the read deadline on %s: %w", c.name, err)
				}
				return parseProxyBanner(line.String())
			}
			line.WriteByte(buf[0])
			continue
		}
		if err != nil {
			return "", c.startupError(err, line.String())
		}
	}
	return "", c.startupError(errors.New("no banner"), line.String())
}

// startupError explains a handshake that did not complete.
//
// The error a pipe reports is a bare EOF or a timeout, which says nothing about the cause. The cause is
// on the child's stderr, and the ones that actually happen all look identical without it: an ssh that
// could not authenticate, a host that does not resolve, a remote with no cm on its PATH, a remote cm too
// old to have this subcommand. Whatever the child printed before exiting is included for that reason,
// and so is any partial line, since an older cm answers a subcommand it does not know with usage text.
func (c *commandConn) startupError(cause error, partial string) error {
	// Collected before reading diag, so anything written on the way out is included.
	_ = c.reap()

	var sb strings.Builder
	fmt.Fprintf(&sb, "%s did not report a cm server", c.name)
	if state := c.cmd.ProcessState; state != nil && state.Exited() {
		fmt.Fprintf(&sb, " and exited with status %d", state.ExitCode())
	}
	said := c.said()
	if said != "" {
		fmt.Fprintf(&sb, ": %s", said)
	} else if partial = strings.TrimSpace(partial); partial != "" {
		fmt.Fprintf(&sb, ": it said %q", partial)
	}
	if looksTooOld(said) {
		sb.WriteString("\nthe cm on the far end is too old for this: it needs a build with `cm server proxy`")
	}

	// A bare EOF is dropped once the cause is named, because it adds a word that means nothing to whoever
	// reads this: "ssh: Could not resolve hostname work: EOF" ends on the least informative part of itself.
	// Any other cause is kept, since a deadline or a read error is a different failure and the message would
	// otherwise not say which.
	if errors.Is(cause, io.EOF) && said != "" {
		return errors.New(sb.String())
	}
	return fmt.Errorf("%s: %w", sb.String(), cause)
}

// looksTooOld reports whether what the child said is a cm that does not know this subcommand.
//
// Matching on cobra's own words, which is not something to do lightly, and is right here for a reason the
// banner cannot cover: the protocol number exists for two proxies that disagree, and a cm predating the
// proxy *entirely* rejects the argv before any of that code runs. `unknown flag: --start` on its own sends
// the reader looking for a typo in something they did not type.
//
// Additive, so a false positive costs one sentence of advice and nothing else, which is what makes a text
// match acceptable where it would not be if behavior depended on it.
func looksTooOld(said string) bool {
	return strings.Contains(said, "unknown flag") ||
		strings.Contains(said, "unknown command") ||
		strings.Contains(said, "unknown shorthand flag")
}

// parseProxyBanner reads the banner line a proxy opens with.
func parseProxyBanner(line string) (string, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), strings.TrimSpace(proxyBannerPrefix)+" ")
	if !ok {
		return "", fmt.Errorf("expected a cm proxy, got %q", strings.TrimSpace(line))
	}
	protocol, version, _ := strings.Cut(rest, " ")
	n, err := strconv.Atoi(protocol)
	if err != nil {
		return "", fmt.Errorf("expected a cm proxy protocol version, got %q", strings.TrimSpace(line))
	}
	if n != ProxyProtocol {
		return "", fmt.Errorf(
			"the remote cm speaks proxy protocol %d and this one speaks %d; upgrade whichever is older",
			n, ProxyProtocol)
	}
	return strings.TrimSpace(version), nil
}

func (c *commandConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *commandConn) Write(p []byte) (int, error) { return c.w.Write(p) }

// Close shuts the connection down and collects the child.
func (c *commandConn) Close() error {
	c.closeOnce.Do(func() {
		// The write half first, because that is how a proxy is asked to stop: it reads EOF on its stdin
		// and exits, which lets ssh close its channel rather than being killed out from under it.
		werr := c.w.Close()
		rerr := c.r.Close()
		reapErr := c.reap()
		// Last, and after the reap, so a message the child wrote on its way out is still collected.
		// Closing it is what ends drainDiag, since the read it is blocked on cannot otherwise return
		// while anything that inherited the write end is alive.
		derr := c.diagPipe.Close()
		c.closeErr = errors.Join(werr, rerr, reapErr, derr)
	})
	return c.closeErr
}

// reapGrace is how long a child gets to exit on its own after its pipes close.
//
// Short because a well-behaved proxy exits as soon as it reads EOF, and the case this bounds is an ssh
// whose far end has gone: closing stdin tells a live ssh to stop and tells an unreachable one nothing,
// and a dropped link is exactly when a connection gets closed.
const reapGrace = 2 * time.Second

// reap waits for the child to exit, killing it if it will not.
//
// Not optional, and not only tidiness: every dial starts a process, and a client's reconnect loop dials
// once per attempt for however long an outage lasts, so a connection that does not collect its child
// leaves a zombie per retry.
//
// The exit status is deliberately not reported as an error. ssh exits 255 when a link drops, which is the
// ordinary end of a remote attachment rather than a failure of Close, and the reason is in diag for
// whoever wants it.
func (c *commandConn) reap() error {
	done := make(chan struct{})
	go func() {
		_ = c.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(reapGrace):
	}
	// A process that finished between the grace expiring and this kill is not an error, and saying so is
	// what a naive version got wrong: os.ErrProcessDone means the thing this wanted has already happened,
	// so reporting it would make a perfectly ordinary Close fail. It is reachable whenever Wait is delayed
	// by anything other than the process itself still running.
	if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("killing %s after it would not exit: %w", c.name, err)
	}
	<-done
	return nil
}

func (c *commandConn) LocalAddr() net.Addr  { return commandAddr("") }
func (c *commandConn) RemoteAddr() net.Addr { return commandAddr(c.name) }

func (c *commandConn) SetDeadline(t time.Time) error {
	return errors.Join(c.r.SetReadDeadline(t), c.w.SetWriteDeadline(t))
}

func (c *commandConn) SetReadDeadline(t time.Time) error  { return c.r.SetReadDeadline(t) }
func (c *commandConn) SetWriteDeadline(t time.Time) error { return c.w.SetWriteDeadline(t) }

// commandAddr names the program on the far end of a commandConn.
//
// net.Conn requires addresses and this connection has none: there is no socket and no port, only a
// process. The program's name is what a log line about this connection should say, so that is what this
// reports.
type commandAddr string

func (a commandAddr) Network() string { return "command" }
func (a commandAddr) String() string  { return string(a) }

// tailBuffer keeps the last diagTail bytes written to it.
//
// The tail rather than the head, because the last thing a failing program says is why it failed.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if excess := len(b.buf) - diagTail; excess > 0 {
		b.buf = b.buf[excess:]
	}
	return len(p), nil
}

// String reports what the program said, as one line.
//
// Joined with "; " rather than kept as written, because this goes into an error message: ssh answers a
// bad host with several lines, and an error that spans them is unreadable wherever errors are printed
// one per line.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	var lines []string
	for _, line := range strings.Split(string(b.buf), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "; ")
}
