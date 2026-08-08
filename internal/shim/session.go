// Package shim owns one session's pty and an append-only log of its output.
//
// It deliberately knows nothing about terminal emulation, scrollback, or session policy.
// That belongs to the server, which is a subscriber like any other. Keeping the shim
// ignorant is what lets the server be upgraded or restarted while the shell keeps
// running: a shim carries no state that a new server cannot rediscover.
package shim

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/creack/pty"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seqlog"
)

// ptyReadSize bounds a single read from the pty master.
//
// The kernel will not return more than this from one read of a pty master regardless of
// the buffer size, so a larger buffer buys nothing. (zmx documents the same constant and
// reasoning.)
const ptyReadSize = 4096

// DefaultLogBytes bounds retained output per session.
//
// This serves a different purpose from a scrollback limit, which the server enforces
// with a real terminal model. This only has to cover the gap while no server is
// subscribed, plus enough history to rebuild a screen after one restarts.
const DefaultLogBytes = 4 << 20 // 4 MiB

// Config describes a session to run.
type Config struct {
	// Session is the name this shim serves. Used for the socket path and exported to
	// the shell.
	Session string
	// Command is the program to run. Empty means the user's login shell.
	Command []string
	// Dir is the working directory. Empty means inherit.
	Dir string
	// Env holds extra KEY=VALUE entries layered onto the shim's environment.
	Env []string
	// Rows and Cols are the initial window size.
	Rows, Cols uint16
	// LogBytes overrides DefaultLogBytes when non-zero.
	LogBytes int
}

// Session is a running shell attached to a pty, with its output accumulating in a Log.
type Session struct {
	cfg Config
	log *seqlog.Log

	// ptmx is the pty master. Reads drain shell output; writes deliver input.
	ptmx *os.File
	cmd  *exec.Cmd

	mu       sync.Mutex
	exited   bool
	exitCode int
	// closeOnce guards releasing the pty, which both Wait and Shutdown can reach.
	closeOnce sync.Once
}

// Start allocates a pty, spawns the command, and begins draining output into the log.
func Start(cfg Config) (*Session, error) {
	if err := paths.ValidateSessionName(cfg.Session); err != nil {
		return nil, err
	}

	argv, err := commandFor(cfg.Command)
	if err != nil {
		return nil, err
	}

	logBytes := cfg.LogBytes
	if logBytes == 0 {
		logBytes = DefaultLogBytes
	}

	s := &Session{cfg: cfg, log: seqlog.New(logBytes)}

	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("allocating pty: %w", err)
	}
	// The child needs the tty; this process does not, and holding it open would keep
	// reads from the master alive after the shell exits, so EOF would never arrive.
	defer tty.Close()

	if cfg.Rows > 0 && cfg.Cols > 0 {
		if err := pty.Setsize(ptmx, &pty.Winsize{Rows: cfg.Rows, Cols: cfg.Cols}); err != nil {
			ptmx.Close()
			return nil, fmt.Errorf("setting initial pty size: %w", err)
		}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cfg.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.Env = append(os.Environ(), cfg.Env...)
	// The shell learns its session name the same way it learns anything else about its
	// environment. Programs use its presence to detect that they are inside cm.
	cmd.Env = append(cmd.Env, paths.SessionEnv()+"="+cfg.Session)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Setsid plus Setctty makes the pty the child's controlling terminal, which is
		// what gives it job control and delivers SIGWINCH on resize. Ctty is an index
		// into the child's fds, and tty is fd 0 there.
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}
	// argv[0] is set to a login-shell name below when running the user's shell, so the
	// path and argv[0] can differ.
	if len(cfg.Command) == 0 {
		cmd.Args = []string{loginArgv0(argv[0])}
	}

	if err := cmd.Start(); err != nil {
		ptmx.Close()
		return nil, fmt.Errorf("starting %s: %w", argv[0], err)
	}

	s.ptmx = ptmx
	s.cmd = cmd

	go s.pump()

	return s, nil
}

// commandFor resolves what to execute: an explicit command, or the user's shell.
func commandFor(command []string) ([]string, error) {
	if len(command) > 0 {
		return command, nil
	}
	if sh := os.Getenv("SHELL"); sh != "" {
		return []string{sh}, nil
	}
	// A session with no shell is useless, but /bin/sh is a better outcome than refusing
	// to start when SHELL is unset, which happens in cron and some launchd contexts.
	return []string{"/bin/sh"}, nil
}

// loginArgv0 returns the argv[0] a login shell expects: its own name prefixed with '-'.
// Shells check this to decide whether to read profile files, so getting it wrong means a
// session that silently skips the user's configuration.
func loginArgv0(path string) string {
	return "-" + filepath.Base(path)
}

// pump drains the pty into the log until the shell exits.
//
// Read errors are not distinguished from EOF: when a pty's child exits, the master read
// fails with EIO on Linux and returns EOF on macOS. Both mean the same thing here, and
// treating them differently would only add a platform branch with no behavioral
// difference.
func (s *Session) pump() {
	buf := make([]byte, ptyReadSize)
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			s.log.Append(buf[:n])
		}
		if err != nil {
			break
		}
	}

	// Reap the child so it does not linger as a zombie, and record why it ended.
	code := 0
	if err := s.cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}

	s.mu.Lock()
	s.exited = true
	s.exitCode = code
	s.mu.Unlock()

	// Closing the log releases subscribers once they have drained the final output,
	// which is how the server learns the session is over.
	s.log.Close()
	s.releasePty()
}

func (s *Session) releasePty() {
	s.closeOnce.Do(func() { s.ptmx.Close() })
}

// Log exposes the output log for subscribers.
func (s *Session) Log() *seqlog.Log { return s.log }

// Write sends input to the pty.
//
// Short writes are reported rather than retried internally: the caller holds the RPC and
// can decide, and a blocking retry here would stall the whole shim on a wedged shell.
func (s *Session) Write(p []byte) (int, error) {
	s.mu.Lock()
	exited := s.exited
	s.mu.Unlock()
	if exited {
		return 0, seqlog.ErrClosed
	}
	return s.ptmx.Write(p)
}

// Resize sets the pty window size. The kernel signals the child, so a shell redraws
// without the shim knowing anything about terminals.
func (s *Session) Resize(rows, cols, xpixel, ypixel uint16) error {
	return pty.Setsize(s.ptmx, &pty.Winsize{
		Rows: rows, Cols: cols, X: xpixel, Y: ypixel,
	})
}

// ResizeSignal sets the window size and guarantees the child sees a change.
//
// The kernel raises SIGWINCH only when the size actually differs from the current one, so a client
// reattaching at the size the session already has gets no signal, and a program that repaints only
// on SIGWINCH keeps updating a screen that is now a replayed snapshot.
//
// When the requested size is already current, the size is briefly set one row shorter and then
// restored, which produces two real changes and therefore two real signals.
//
// Rows rather than columns is deliberate: a narrower width makes the terminal re-wrap every line
// to a width the client never had, and that reflow is visible and sometimes lossy. One row less is
// only ever a scroll.
func (s *Session) ResizeSignal(rows, cols, xpixel, ypixel uint16) error {
	cur, curCols, err := s.Size()
	if err == nil && cur == rows && curCols == cols && rows > 1 {
		nudge := pty.Winsize{Rows: rows - 1, Cols: cols, X: xpixel, Y: ypixel}
		if err := pty.Setsize(s.ptmx, &nudge); err != nil {
			return err
		}
	}
	return s.Resize(rows, cols, xpixel, ypixel)
}

// Size reports the pty's current window size.
func (s *Session) Size() (rows, cols uint16, err error) {
	ws, err := pty.GetsizeFull(s.ptmx)
	if err != nil {
		return 0, 0, err
	}
	return ws.Rows, ws.Cols, nil
}

// ShellPID reports the shell's process id, or 0 once it has exited.
func (s *Session) ShellPID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Exited reports whether the shell has exited and, if so, its status.
func (s *Session) Exited() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited, s.exitCode
}

// Signal sends a signal to the shell, or to its process group when group is set.
//
// Signaling the group is usually what a caller wants, so that a foreground job and its
// children are included rather than just the shell.
func (s *Session) Signal(sig syscall.Signal, group bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited || s.cmd.Process == nil {
		return seqlog.ErrClosed
	}
	pid := s.cmd.Process.Pid
	if group {
		// Negating the pid targets the process group. The child called setsid, so it
		// leads its own group and the negation cannot reach the shim.
		pid = -pid
	}
	return syscall.Kill(pid, sig)
}
