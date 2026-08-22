package shim

import (
	"context"
	"errors"

	"github.com/chancez/cm/internal/seqlog"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readUntil accumulates output until want appears or the deadline passes. Terminal output
// arrives in arbitrarily sized pieces, so tests cannot assume one read per write.
func readUntil(t *testing.T, r *seqlog.Reader, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sb strings.Builder
	for {
		c, err := r.Next(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v (got %q)", want, err, sb.String())
		}
		sb.Write(c.Data)
		if strings.Contains(sb.String(), want) {
			return sb.String()
		}
	}
}

// waitExit polls for shell exit. The pump goroutine records it asynchronously, so there
// is no channel to wait on from outside the package.
func waitExit(t *testing.T, s *Session) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exited, code := s.Exited(); exited {
			return code
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("shell did not exit before deadline")
	return 0
}

func TestSessionRunsCommandAndCapturesOutput(t *testing.T) {
	s, err := Start(Config{
		Session: "test",
		Command: []string{"/bin/sh", "-c", "printf 'hello from shell\\n'"},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	r := s.Log().Subscribe(0)
	defer r.Close()

	got := readUntil(t, r, "hello from shell")
	if !strings.Contains(got, "hello from shell") {
		t.Errorf("output = %q, want it to contain %q", got, "hello from shell")
	}
	if code := waitExit(t, s); code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
}

func TestSessionWriteReachesShell(t *testing.T) {
	s, err := Start(Config{
		Session: "test",
		// Echo one line back so the test does not depend on shell prompt behavior.
		Command: []string{"/bin/sh", "-c", "read line; printf 'got:%s\\n' \"$line\""},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	r := s.Log().Subscribe(0)
	defer r.Close()

	if _, err := s.Write([]byte("ping\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got := readUntil(t, r, "got:ping")
	if !strings.Contains(got, "got:ping") {
		t.Errorf("output = %q, want it to contain %q", got, "got:ping")
	}
}

// A shell must see the pty as its controlling terminal, or job control and SIGWINCH do
// not work. `tty` failing is the symptom of getting Setsid/Setctty wrong.
func TestSessionShellHasControllingTerminal(t *testing.T) {
	s, err := Start(Config{
		Session: "test",
		Command: []string{"/bin/sh", "-c", "tty; test -t 0 && echo IS_TTY"},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	r := s.Log().Subscribe(0)
	defer r.Close()

	got := readUntil(t, r, "IS_TTY")
	if !strings.Contains(got, "/dev/") {
		t.Errorf("output = %q, want a tty device path", got)
	}
}

func TestSessionReportsInitialSize(t *testing.T) {
	// The shell stays alive after printing, so Size() below queries a live pty.
	//
	// `sh -c "stty size"` exits as soon as it has printed, and the pty is released with it, so asking for
	// the size afterwards raced the teardown. That used to appear to work: the ioctl went to a
	// already-closed descriptor and happened to return a plausible answer, which is precisely the
	// use-after-close this package now guards against. The guard turned a silent bug into a visible
	// failure, so the test has to stop depending on it.
	s, err := Start(Config{
		Session: "test",
		Command: []string{"/bin/sh", "-c", "stty size; sleep 30"},
		Rows:    30, Cols: 100,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	r := s.Log().Subscribe(0)
	defer r.Close()

	got := readUntil(t, r, "30 100")
	if !strings.Contains(got, "30 100") {
		t.Errorf("stty size = %q, want it to contain %q", got, "30 100")
	}

	rows, cols, err := s.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if rows != 30 || cols != 100 {
		t.Errorf("Size() = (%d, %d), want (30, 100)", rows, cols)
	}
}

func TestSessionReportsInitialPixelSize(t *testing.T) {
	// A pty created without pixel dimensions reports zero for them, and `kitten icat` reads exactly that
	// before it transmits anything: zero means "this terminal cannot report pixel sizes", so it refuses
	// to send the image and names kitty as a terminal to use instead. The values reached the server on
	// the wire and were dropped on the way to the shim, so the symptom was that an image worked in a
	// session that had been resized and never in a fresh one.
	//
	// Asserted at this seam rather than end to end because this is where they were lost, and a pty's
	// winsize is directly observable. The shell sleeps so the pty is still live when it is queried, for
	// the reason the test above records.
	s, err := Start(Config{
		Session: "test",
		Command: []string{"/bin/sh", "-c", "sleep 30"},
		Rows:    30, Cols: 100,
		XPixel: 800, YPixel: 600,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	xpixel, ypixel, err := s.PixelSize()
	if err != nil {
		t.Fatalf("PixelSize() error = %v", err)
	}
	if xpixel != 800 || ypixel != 600 {
		t.Errorf("PixelSize() = (%d, %d), want (800, 600)", xpixel, ypixel)
	}

	// Cells must survive setting pixels. TIOCSWINSZ writes the whole struct, so an implementation that
	// set pixels in a second ioctl would zero these, and a test asserting only pixels would pass.
	rows, cols, err := s.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if rows != 30 || cols != 100 {
		t.Errorf("Size() = (%d, %d), want (30, 100)", rows, cols)
	}
}

// Resizing must reach the shell as SIGWINCH, which is what makes a reattached client at a
// different size get a correct repaint.
func TestSessionResizeSignalsShell(t *testing.T) {
	s, err := Start(Config{
		Session: "test",
		Command: []string{"/bin/sh", "-c", `
			trap 'stty size' WINCH
			echo READY
			# Sleep in short bursts: a signal interrupts sleep, and the loop keeps the
			# shell alive long enough to observe the trap firing.
			i=0
			while [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done
		`},
		Rows: 24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer s.Signal(syscall.SIGKILL, true)

	r := s.Log().Subscribe(0)
	defer r.Close()

	readUntil(t, r, "READY")

	if err := s.Resize(40, 120, 0, 0); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	got := readUntil(t, r, "40 120")
	if !strings.Contains(got, "40 120") {
		t.Errorf("after resize output = %q, want it to contain %q", got, "40 120")
	}
}

// When the shell exits, subscribers must reach ErrLogClosed after draining. That is how
// the server learns to stop rather than reconnect.
func TestSessionClosesLogOnShellExit(t *testing.T) {
	s, err := Start(Config{
		Session: "test",
		Command: []string{"/bin/sh", "-c", "printf 'bye\\n'; exit 3"},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	r := s.Log().Subscribe(0)
	defer r.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sb strings.Builder
	var lastErr error
	for {
		c, err := r.Next(ctx)
		if err != nil {
			lastErr = err
			break
		}
		sb.Write(c.Data)
	}

	if !errors.Is(lastErr, seqlog.ErrClosed) {
		t.Errorf("final error = %v, want seqlog.ErrClosed", lastErr)
	}
	if !strings.Contains(sb.String(), "bye") {
		t.Errorf("output = %q, want it to contain %q: output before exit must not be lost",
			sb.String(), "bye")
	}
	if code := waitExit(t, s); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
}

func TestSessionWriteAfterExitFails(t *testing.T) {
	s, err := Start(Config{
		Session: "test",
		Command: []string{"/bin/sh", "-c", "exit 0"},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitExit(t, s)

	if _, err := s.Write([]byte("x")); err == nil {
		t.Error("Write() after exit = nil error, want failure")
	}
	if pid := s.ShellPID(); pid != 0 {
		t.Errorf("ShellPID() after exit = %d, want 0", pid)
	}
}

// The session's identity must reach the shell's environment: it is how programs inside a session detect
// they are in one, and how anything running in there refers to the session it is in.
//
// Exported with the ID sigil, so the value is a reference `cm read $CM_SESSION` accepts. Without it the
// bare ID would be read as a name, and there is no session of that name.
func TestSessionExportsSessionEnv(t *testing.T) {
	s, err := Start(Config{
		Session: "mysession",
		Command: []string{"/bin/sh", "-c", "printf 'env=%s\\n' \"$CM_SESSION\""},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	r := s.Log().Subscribe(0)
	defer r.Close()

	got := readUntil(t, r, "env=")
	if !strings.Contains(got, "env=@mysession") {
		t.Errorf("output = %q, want it to contain %q", got, "env=@mysession")
	}
}

func TestSessionRunsInConfiguredDir(t *testing.T) {
	dir := t.TempDir()

	s, err := Start(Config{
		Session: "test",
		Command: []string{"/bin/sh", "-c", "pwd -P"},
		Dir:     dir,
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	r := s.Log().Subscribe(0)
	defer r.Close()

	// Compare on the final path element rather than the whole path: macOS hands out
	// temp dirs under /var, which is a symlink to /private/var, so `pwd -P` reports a
	// different prefix than TempDir returned.
	base := filepath.Base(dir)
	got := readUntil(t, r, base)
	if !strings.Contains(got, base) {
		t.Errorf("pwd = %q, want it to contain %q", got, base)
	}
}

func TestSessionSignalTerminatesShell(t *testing.T) {
	s, err := Start(Config{
		Session: "test",
		Command: []string{"/bin/sh", "-c", "echo READY; i=0; while [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done"},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	r := s.Log().Subscribe(0)
	defer r.Close()
	readUntil(t, r, "READY")

	if err := s.Signal(syscall.SIGKILL, true); err != nil {
		t.Fatalf("Signal() error = %v", err)
	}
	waitExit(t, s)
}

func TestStartRejectsInvalidSessionName(t *testing.T) {
	if _, err := Start(Config{Session: "../evil", Rows: 24, Cols: 80}); err == nil {
		t.Error("Start() with traversal name = nil error, want rejection")
	}
}
