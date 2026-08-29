// Package shim owns one session's pty and an append-only log of its output.
//
// It deliberately knows nothing about terminal emulation, scrollback, or session policy.
// That belongs to the server, which is a subscriber like any other. Keeping the shim
// ignorant is what lets the server be upgraded or restarted while the shell keeps
// running: a shim carries no state that a new server cannot rediscover.
package shim

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seq"
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
	// Session is what this shim serves, as the server names it. Used for the socket path.
	//
	// An ID from a current server, a name from an older one. The shim does not care which and must not
	// guess: the two overlap, since a name of only lowercase letters also satisfies the ID rules.
	Session string
	// SessionRef is what to export as CM_SESSION, or empty to export Session unchanged.
	//
	// Carried separately because only the server knows whether the value it passed is an identity, and
	// the sigil that makes it one cannot be added by inspection. Building it here from Session exported
	// `@kitty.325` under an older server, an ID that is not one, and every cm command inside such a
	// session then reported "no session given" because the reference failed to resolve.
	SessionRef string
	// Command is the program to run. Empty means the user's login shell.
	Command []string
	// Dir is the working directory. Empty means inherit.
	Dir string
	// Env holds extra KEY=VALUE entries layered onto the shim's environment.
	Env []string
	// Rows and Cols are the initial window size.
	Rows, Cols uint16
	// XPixel and YPixel are the initial window size in pixels, zero when the client did not say.
	//
	// Carried separately from Rows and Cols because a program reads them separately, and because
	// leaving them zero is not cosmetic: `kitten icat` reads the pty's ws_xpixel/ws_ypixel before it
	// sends anything, and zero there means "this terminal cannot report pixel sizes", so it refuses
	// to transmit an image at all rather than falling back. The wire and Resize have always carried
	// these; only session creation dropped them, so images worked after any resize and not before.
	XPixel, YPixel uint16
	// LogBytes overrides DefaultLogBytes when non-zero.
	LogBytes int
	// PersistPath, when set, is a file the output log is also written to, so a session's content
	// survives this process and a reboot.
	PersistPath string
	// PersistLimits bounds the persisted file. Ignored when PersistPath is empty.
	PersistLimits seqlog.FileLimits
}

// Session is a running shell attached to a pty, with its output accumulating in a Log.
type Session struct {
	cfg Config
	// outputLog holds the session's terminal output. Distinct from log, which records what the shim
	// itself did.
	outputLog *seqlog.Log[seq.Shim]
	// persist mirrors output to disk when the session is configured to survive this process. Nil
	// otherwise, which is the common case.
	persist *seqlog.File[seq.Shim]

	// ptmx is the pty master. Reads drain shell output; writes deliver input.
	ptmx *os.File
	cmd  *exec.Cmd

	// log records what the shim does. Never nil.
	log *slog.Logger

	mu       sync.Mutex
	exited   bool
	exitCode int

	// ptyMu guards the pty fd's existence against the ioctl callers below, which are RPC handlers
	// and so run on their own goroutines.
	//
	// Separate from mu, and a read-write lock, for two reasons. It must not be held across a
	// blocking pty read, or closing the fd could no longer interrupt one, and that is exactly how
	// Shutdown unblocks the pump. And several ioctls at once are harmless, while a close must
	// exclude all of them.
	//
	// Read and Write need no such guard: os.File refcounts its descriptor internally, so those are
	// safe against a concurrent Close. The ioctl helpers are not, because they go through
	// os.File.Fd(), which hands out the raw descriptor with no synchronization at all. After a close
	// that number is stale and the kernel may have handed it to something else, so the ioctl lands
	// on an unrelated file.
	ptyMu   sync.RWMutex
	ptyGone bool
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

	s := &Session{cfg: cfg, log: cmlog.Discard(), outputLog: seqlog.New[seq.Shim](logBytes)}

	if cfg.PersistPath != "" {
		limits := cfg.PersistLimits
		if limits.MaxLines == 0 && limits.MaxBytes == 0 {
			limits = seqlog.DefaultFileLimits
		}
		// A log that cannot be opened is reported rather than ignored: the caller asked for a
		// session whose content survives, and silently not doing that would be discovered only
		// after a reboot, when it is too late to matter.
		pf, err := seqlog.OpenFile[seq.Shim](cfg.PersistPath, limits)
		if err != nil {
			return nil, err
		}
		s.persist = pf

		// Continue the numbering the file already holds, so a sequence number recorded before this
		// process started still means the same byte. Starting from zero would make every stored
		// position wrong.
		_, next := pf.Bounds()
		s.outputLog = seqlog.NewAt(logBytes, next)
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("allocating pty: %w", err)
	}
	// The child needs the tty; this process does not, and holding it open would keep
	// reads from the master alive after the shell exits, so EOF would never arrive.
	defer tty.Close()

	if cfg.Rows > 0 && cfg.Cols > 0 {
		// Pixels are set alongside rows and cols rather than in a second ioctl: TIOCSWINSZ writes the
		// whole struct, so a later call carrying only pixels would zero the cell size.
		if err := pty.Setsize(ptmx, &pty.Winsize{
			Rows: cfg.Rows, Cols: cfg.Cols, X: cfg.XPixel, Y: cfg.YPixel,
		}); err != nil {
			ptmx.Close()
			return nil, fmt.Errorf("setting initial pty size: %w", err)
		}
	}

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cfg.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.Env = append(os.Environ(), cfg.Env...)
	// The shell learns which session it is in the same way it learns anything else about its
	// environment. Programs use its presence to detect that they are inside cm.
	//
	// The session's ID, as a reference with the sigil, so `cm read $CM_SESSION` works. A name would be
	// the friendlier value and would be wrong: names are bindings, so the one this session was created
	// under can be pointed at a different session while this shell runs, and every script in here that
	// had captured it would then be reading somewhere else. An ID cannot be reassigned, which is the
	// only property that makes a variable captured once at shell startup safe to keep using.
	//
	// Taken from the server rather than built here. See Config.SessionRef: an older server passes a name,
	// and turning that into a reference produced one that resolves to nothing.
	sessionRef := cfg.SessionRef
	if sessionRef == "" {
		sessionRef = cfg.Session
	}
	cmd.Env = append(cmd.Env, paths.SessionEnv()+"="+sessionRef)
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
			s.outputLog.Append(buf[:n])
			if s.persist != nil {
				// A failed write stops persisting rather than ending the session. The shell and
				// its live output are the valuable part; the file is a cache that improves the
				// next reboot.
				if perr := s.persist.Append(buf[:n]); perr != nil {
					// The session continues without persisting, so a reboot will lose it. That is a
					// silent downgrade of something the user asked for, hence the log.
					s.log.Error("persisting output failed, session will not survive a reboot",
						"session", s.cfg.Session, "error", perr)
					s.persist.Close()
					s.mu.Lock()
					s.persist = nil
					s.mu.Unlock()
				}
			}
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
	s.outputLog.Close()

	// Flush here rather than on every append: the tail is what a restore needs most, and this is
	// the point where losing it would matter.
	if s.persist != nil {
		s.persist.Close()
	}

	s.releasePty()
}

func (s *Session) releasePty() {
	s.closeOnce.Do(func() {
		// Marked gone and closed under the write lock, so no ioctl can be mid-flight on the
		// descriptor while it is being closed or reused.
		s.ptyMu.Lock()
		s.ptyGone = true
		s.ptmx.Close()
		s.ptyMu.Unlock()
	})
}

// ErrSessionOver reports that a session's pty is gone, so there is nothing left to act on.
//
// Its own error rather than reusing seqlog.ErrClosed, which is about the output log. Sharing one
// produced a real confusion: a resize arriving just after a shell exited failed with "output log is
// closed", the server could not tell that from a genuine failure, and `cm run` reported
// "sizing session x: output log is closed" for a command that had completed successfully.
var ErrSessionOver = errors.New("session is over")

// withPty runs fn on the pty fd, or reports that the session is over.
//
// For the ioctl callers only. See ptyMu on why Read and Write do not use this.
func (s *Session) withPty(fn func(*os.File) error) error {
	s.ptyMu.RLock()
	defer s.ptyMu.RUnlock()
	if s.ptyGone {
		return ErrSessionOver
	}
	return fn(s.ptmx)
}

// SetLogger installs a logger. A discarding logger is used until one is set.
func (s *Session) SetLogger(l *slog.Logger) {
	if l != nil {
		s.log = l
	}
}

// Log exposes the output log for subscribers.
func (s *Session) Log() *seqlog.Log[seq.Shim] { return s.outputLog }

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
	return s.withPty(func(f *os.File) error {
		return pty.Setsize(f, &pty.Winsize{
			Rows: rows, Cols: cols, X: xpixel, Y: ypixel,
		})
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
		if err := s.withPty(func(f *os.File) error {
			return pty.Setsize(f, &nudge)
		}); err != nil {
			return err
		}
	}
	return s.Resize(rows, cols, xpixel, ypixel)
}

// Size reports the pty's current window size.
func (s *Session) Size() (rows, cols uint16, err error) {
	var ws *pty.Winsize
	if err := s.withPty(func(f *os.File) error {
		var ferr error
		ws, ferr = pty.GetsizeFull(f)
		return ferr
	}); err != nil {
		return 0, 0, err
	}
	return ws.Rows, ws.Cols, nil
}

// PixelSize reports the pty's current window size in pixels, zero when it has none.
//
// Separate from Size rather than widening it, because Size has a caller that compares only cells and
// widening the return would make every such comparison silently depend on pixels too.
func (s *Session) PixelSize() (xpixel, ypixel uint16, err error) {
	var ws *pty.Winsize
	if err := s.withPty(func(f *os.File) error {
		var ferr error
		ws, ferr = pty.GetsizeFull(f)
		return ferr
	}); err != nil {
		return 0, 0, err
	}
	return ws.X, ws.Y, nil
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

// Signal sends a signal to the shell, or to the pty's foreground process group when group is set.
//
// The foreground group, not the shell's own group, and the difference is the whole point of the flag.
// A shell with job control puts each job it starts in a *new* process group and hands that group the
// terminal, so the shell's group contains only the shell. Signalling it therefore does not reach the
// job a caller means to stop: measured with `sleep 300` under /bin/sh, where the shell sat in group
// 20144 and the sleep in group 20242, and a SIGTERM to the shell's group left the sleep running while
// reporting success.
//
// Asking the pty which group is in the foreground is what a keypress does -- the line discipline
// delivers ctrl-c to exactly that group -- so this makes `cm signal` mean the same thing as the key,
// for the cases where the key cannot get through.
//
// Falls back to the shell's own group when the pty cannot say. That happens when the shell has no job
// running, in which case the shell *is* the foreground group and the two answers agree anyway.
func (s *Session) Signal(sig syscall.Signal, group bool) error {
	s.mu.Lock()
	shellPID := 0
	if !s.exited && s.cmd.Process != nil {
		shellPID = s.cmd.Process.Pid
	}
	s.mu.Unlock()
	if shellPID == 0 {
		return seqlog.ErrClosed
	}

	if !group {
		return syscall.Kill(shellPID, sig)
	}

	target := s.signalTarget(shellPID)
	// Negating targets the process group. The child called setsid, so neither its own group nor a
	// foreground group under its terminal can reach the shim.
	return syscall.Kill(-target, sig)
}

// signalTarget returns the process group a grouped signal should go to.
//
// The pty's foreground group when it can be read, since that is what the line discipline delivers a
// keypress to, and the shell's own group otherwise. See Signal for why the difference matters.
func (s *Session) signalTarget(shellPID int) int {
	if pgrp, err := s.foregroundGroup(); err == nil && pgrp > 0 {
		return pgrp
	}
	return shellPID
}

// SignalAndCheck signals the session's process group and reports which of its processes survived.
//
// Exists because the signal cm sends by default is a request. SIGHUP can be ignored, and when it is,
// `cm kill` reports the session killed while a process keeps holding a pty -- the resource macOS caps at
// 511 system-wide, whose exhaustion surfaces as "device not configured" in something unrelated. The leak
// was silent, and this is where it stops being: the shim is the only place that still knows the pty and
// the process group, since the record is deleted immediately afterwards and nothing else can attribute a
// stray process to cm.
//
// Deliberately reports rather than escalates. A job trapping SIGHUP to finish writing a file is doing
// something legitimate, and a shim that killed it anyway would break that to tidy up. The caller decides,
// which is what `cm kill --signal` is for.
//
// The wait is a fixed short grace rather than a configurable one. It is only here to let a process that
// is going to die actually die, so tuning it would invite treating it as a shutdown timeout, which it is
// not: nothing waits on the result except a warning.
func (s *Session) SignalAndCheck(sig syscall.Signal, grace time.Duration) (pgid int, surviving []int, err error) {
	s.mu.Lock()
	shellPID := 0
	if !s.exited && s.cmd.Process != nil {
		shellPID = s.cmd.Process.Pid
	}
	s.mu.Unlock()
	if shellPID == 0 {
		return 0, nil, seqlog.ErrClosed
	}

	// Read before signalling. Afterwards the job may be gone, and with it the answer to which group was
	// signalled, which is the part a diagnostic needs to name.
	pgid = s.signalTarget(shellPID)
	members := processGroupMembers(pgid)

	if err := syscall.Kill(-pgid, sig); err != nil {
		return pgid, nil, err
	}

	// SIGKILL cannot be caught, so anything still present after it is a kernel-level wait rather than a
	// process declining to leave. Checking anyway rather than special-casing: an unkillable process is
	// worth reporting too, and the grace period is short.
	time.Sleep(grace)

	for _, pid := range members {
		if processLives(pid) {
			surviving = append(surviving, pid)
		}
	}
	return pgid, surviving, nil
}

// processLives reports whether a pid is a process still running, rather than merely a pid that exists.
//
// The distinction is the whole point. Signal 0 asks whether a pid exists without delivering anything, and a
// zombie exists: it has died but its parent has not reaped it yet, so the entry stays until wait() collects
// it. Using signal 0 alone therefore reported a process that had just been SIGKILLed as having survived
// SIGKILL, which is impossible and read as a bug in the signalling rather than in the check.
//
// It surfaced first on Linux, failing TestKillWithSignalWarnsAboutNothing and TestDoctorIsQuietAfterACleanKill
// under `mise run test-linux` while the macOS suite passed. That made it look platform-specific and it is
// not: measured on darwin too, `ps -o stat= -p <pid>` reports Z for half a second after SIGKILL while
// Kill(pid, 0) still returns nil. The macOS e2e tests missed it only because the pty teardown there happens
// to reap the job before the check runs. A /proc-only fix would have left darwin broken and looked correct.
//
// Reading /proc where it exists, since it answers directly with no subprocess, and asking ps elsewhere.
// A pid whose state cannot be determined either way counts as living, so an unparseable answer loses the
// zombie refinement rather than suppressing a real leak warning.
func processLives(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if state, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// The state field is the third, after the comm field, which is parenthesised and may itself contain
		// spaces or parens. Scanning back from the last ')' is the standard way to parse this safely.
		end := bytes.LastIndexByte(state, ')')
		if end < 0 || end+2 >= len(state) {
			return true
		}
		return state[end+2] != 'Z'
	}
	// No /proc, so ask ps. Through a subprocess reluctantly, for the same reason processGroupMembers shells
	// out to pgrep: the alternative is a per-platform sysctl, and this only ever decides the wording of a
	// diagnostic. A missing or failing ps degrades to reporting the process as alive.
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return true
	}
	state := bytes.TrimSpace(out)
	if len(state) == 0 {
		// ps knows nothing about it, so it is gone despite signal 0 having succeeded a moment ago.
		return false
	}
	return state[0] != 'Z'
}

// processGroupMembers returns the pids in a process group.
//
// Through pgrep rather than a sysctl or /proc walk, because those differ per platform and this is only
// ever used to name pids in a warning. A missing or failing pgrep therefore degrades to reporting no
// leak, which loses a diagnostic rather than breaking a kill.
//
// Read before the signal is sent, since afterwards the members are exactly what is being asked about.
func processGroupMembers(pgid int) []int {
	if pgid <= 0 {
		return nil
	}
	out, err := exec.Command("pgrep", "-g", strconv.Itoa(pgid)).Output()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil || pid == self {
			// The shim is never in the signalled group, since the child called setsid, but excluding
			// it costs nothing and a shim reporting itself as a leak would be absurd.
			continue
		}
		pids = append(pids, pid)
	}
	return pids
}

// foregroundGroup asks the pty which process group currently owns the terminal.
//
// Through withPty rather than a bare Fd(), because Fd() is not refcounted the way Read and Write are:
// an ioctl on a descriptor that Close is racing is a real race even though the I/O calls are safe.
func (s *Session) foregroundGroup() (int, error) {
	var pgrp int
	err := s.withPty(func(f *os.File) error {
		var ferr error
		pgrp, ferr = unix.IoctlGetInt(int(f.Fd()), unix.TIOCGPGRP)
		return ferr
	})
	return pgrp, err
}
