package shim

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/ttrpc"

	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// startShim runs a real shim over a real unix socket and returns a connected client.
// Exercising the socket rather than calling the service directly is the point: the socket
// lifecycle is where a shim leaks or wedges.
func startShim(t *testing.T, cfg Config) (shimv1.ShimClient, *Session) {
	t.Helper()

	socket := filepath.Join(shortTempDir(t), "s.sock")
	l, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}

	session, err := Start(cfg)
	if err != nil {
		l.Close()
		t.Fatalf("Start() error = %v", err)
	}
	svc := NewService(session)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, l, svc) }()

	conn, err := net.Dial("unix", socket)
	if err != nil {
		cancel()
		t.Fatalf("Dial() error = %v", err)
	}
	cl := ttrpc.NewClient(conn)

	t.Cleanup(func() {
		cl.Close()
		cancel()
		// Make sure the shell cannot outlive the test.
		_ = session.Signal(syscall.SIGKILL, true)
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Error("Serve() did not return after cancellation")
		}
	})

	return shimv1.NewShimClient(cl), session
}

// collect reads from a subscribe stream until want appears.
func collect(t *testing.T, sub shimv1.Shim_SubscribeClient, want string) (string, bool) {
	t.Helper()
	var sb strings.Builder
	gap := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := sub.Recv()
		if err != nil {
			t.Fatalf("Recv() error = %v (got %q so far)", err, sb.String())
		}
		sb.Write(out.Data)
		if out.Gap {
			gap = true
		}
		if strings.Contains(sb.String(), want) {
			return sb.String(), gap
		}
	}
	t.Fatalf("timed out waiting for %q, got %q", want, sb.String())
	return "", false
}

func TestServiceStateReportsSession(t *testing.T) {
	cl, session := startShim(t, Config{
		Session: "statetest",
		Command: []string{"/bin/sh", "-c", "echo READY; i=0; while [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done"},
		Rows:    30, Cols: 100,
	})

	sub, err := cl.Subscribe(context.Background(), &shimv1.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	collect(t, sub, "READY")

	got, err := cl.State(context.Background(), &shimv1.StateRequest{})
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}

	if got.Session != "statetest" {
		t.Errorf("Session = %q, want %q", got.Session, "statetest")
	}
	if got.ShellPid != int32(session.ShellPID()) || got.ShellPid == 0 {
		t.Errorf("ShellPid = %d, want the live shell pid %d", got.ShellPid, session.ShellPID())
	}
	if got.Rows != 30 || got.Cols != 100 {
		t.Errorf("size = (%d, %d), want (30, 100)", got.Rows, got.Cols)
	}
	if got.Exited {
		t.Error("Exited = true, want false while the shell is running")
	}
	if got.NextSeq == 0 {
		t.Error("NextSeq = 0, want it to advance after output")
	}
}

func TestServiceWriteAndSubscribe(t *testing.T) {
	cl, _ := startShim(t, Config{
		Session: "iotest",
		Command: []string{"/bin/sh", "-c", "read line; printf 'got:%s\\n' \"$line\""},
		Rows:    24, Cols: 80,
	})

	sub, err := cl.Subscribe(context.Background(), &shimv1.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	resp, err := cl.Write(context.Background(), &shimv1.WriteRequest{Data: []byte("hello\n")})
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if resp.Written != 6 {
		t.Errorf("Written = %d, want 6", resp.Written)
	}

	got, _ := collect(t, sub, "got:hello")
	if !strings.Contains(got, "got:hello") {
		t.Errorf("output = %q, want it to contain %q", got, "got:hello")
	}
}

// The property the layer exists for, over a real socket: output produced while nobody is
// subscribed is still delivered to a later subscriber, in order and without duplication.
func TestServiceResumeFromSeqAcrossReconnect(t *testing.T) {
	cl, _ := startShim(t, Config{
		Session: "resumetest",
		Command: []string{"/bin/sh", "-c", "read a; echo first:$a; read b; echo second:$b; sleep 30"},
		Rows:    24, Cols: 80,
	})
	ctx := context.Background()

	sub1, err := cl.Subscribe(ctx, &shimv1.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	if _, err := cl.Write(ctx, &shimv1.WriteRequest{Data: []byte("one\n")}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	collect(t, sub1, "first:one")

	// Where a restarting server would resume from.
	st, err := cl.State(ctx, &shimv1.StateRequest{})
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	resumeFrom := st.NextSeq

	// Subscriber goes away, output continues.
	if _, err := cl.Write(ctx, &shimv1.WriteRequest{Data: []byte("two\n")}); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	sub2, err := cl.Subscribe(ctx, &shimv1.SubscribeRequest{FromSeq: resumeFrom})
	if err != nil {
		t.Fatalf("resumed Subscribe() error = %v", err)
	}
	got, gap := collect(t, sub2, "second:two")

	if gap {
		t.Error("resumed stream reported a gap, want none: the log retained the resume point")
	}
	// The resumed stream must not repeat what the first subscriber already saw.
	if strings.Contains(got, "first:one") {
		t.Errorf("resumed output = %q, want it to exclude already-delivered output", got)
	}
}

// A subscriber whose resume point was dropped must be told, since replaying across a hole
// cannot reconstruct the state the missing bytes established.
func TestServiceReportsGapWhenResumePointDropped(t *testing.T) {
	cl, _ := startShim(t, Config{
		Session: "gaptest",
		Command: []string{"/bin/sh", "-c", "echo READY; i=0; while [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done"},
		Rows:    24, Cols: 80,
		// Tiny log so a little output overruns it.
		LogBytes: 64,
	})
	ctx := context.Background()

	sub, err := cl.Subscribe(ctx, &shimv1.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	collect(t, sub, "READY")

	// Ask for sequence 0, which a 64-byte log has long since dropped.
	st, err := cl.State(ctx, &shimv1.StateRequest{})
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if st.OldestSeq == 0 {
		t.Skip("log did not overrun; nothing to assert")
	}

	sub2, err := cl.Subscribe(ctx, &shimv1.SubscribeRequest{FromSeq: 0})
	if err != nil {
		t.Fatalf("Subscribe(0) error = %v", err)
	}
	out, err := sub2.Recv()
	if err != nil {
		t.Fatalf("Recv() error = %v", err)
	}
	if !out.Gap {
		t.Error("Gap = false, want true when the requested sequence was already dropped")
	}
}

func TestServiceResize(t *testing.T) {
	cl, _ := startShim(t, Config{
		Session: "resizetest",
		Command: []string{"/bin/sh", "-c", `
			trap 'stty size' WINCH
			echo READY
			i=0
			while [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done
		`},
		Rows: 24, Cols: 80,
	})
	ctx := context.Background()

	sub, err := cl.Subscribe(ctx, &shimv1.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	collect(t, sub, "READY")

	if _, err := cl.Resize(ctx, &shimv1.ResizeRequest{Rows: 50, Cols: 132}); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	got, _ := collect(t, sub, "50 132")
	if !strings.Contains(got, "50 132") {
		t.Errorf("output = %q, want the shell to observe the new size", got)
	}

	st, err := cl.State(ctx, &shimv1.StateRequest{})
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	if st.Rows != 50 || st.Cols != 132 {
		t.Errorf("State size = (%d, %d), want (50, 132)", st.Rows, st.Cols)
	}
}

// Subscribe ends cleanly when the shell exits, rather than erroring, and the exit status
// is available afterwards. That combination is how the server decides to stop instead of
// reconnecting.
func TestServiceSubscribeEndsOnShellExit(t *testing.T) {
	cl, _ := startShim(t, Config{
		Session: "exittest",
		Command: []string{"/bin/sh", "-c", "echo done; exit 7"},
		Rows:    24, Cols: 80,
	})
	ctx := context.Background()

	sub, err := cl.Subscribe(ctx, &shimv1.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}

	var sb strings.Builder
	for {
		out, err := sub.Recv()
		if err != nil {
			break
		}
		sb.Write(out.Data)
	}
	if !strings.Contains(sb.String(), "done") {
		t.Errorf("output = %q, want it to contain %q", sb.String(), "done")
	}

	// State remains answerable after the shell is gone, since the shim is still up until
	// its own shutdown.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := cl.State(ctx, &shimv1.StateRequest{})
		if err != nil {
			break
		}
		if st.Exited {
			if st.ExitCode != 7 {
				t.Errorf("ExitCode = %d, want 7", st.ExitCode)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServiceShutdownStopsShell(t *testing.T) {
	cl, session := startShim(t, Config{
		Session: "shutdowntest",
		Command: []string{"/bin/sh", "-c", "echo READY; i=0; while [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done"},
		Rows:    24, Cols: 80,
	})
	ctx := context.Background()

	sub, err := cl.Subscribe(ctx, &shimv1.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	collect(t, sub, "READY")

	if _, err := cl.Shutdown(ctx, &shimv1.ShutdownRequest{Force: true}); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exited, _ := session.Exited(); exited {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("shell still running after Shutdown")
}

// Two servers must never share one session's socket, or both would drive the same pty.
func TestListenRefusesWhenAlreadyServed(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "s.sock")
	l, err := Listen(socket)
	if err != nil {
		t.Fatalf("first Listen() error = %v", err)
	}
	defer l.Close()

	if _, err := Listen(socket); err == nil {
		t.Error("second Listen() = nil error, want refusal while the first is live")
	}
}

// A live shim whose accept queue was momentarily full must not have its socket taken.
//
// The worst version of this bug, because the damage is unrecoverable. Listen unlinks a socket it
// believes is stale, and it used to believe that on the strength of a single refused dial. But a unix
// listener refuses connections once its accept queue is full, with the same errno a socket nobody
// serves produces, so a shim that was simply busy had its socket removed: the shell keeps running,
// holding a pty, on a path nothing can name again. `cm doctor` cannot even find it, since it
// enumerates sockets.
//
// The busy state is constructed rather than waited for, so the refusal happens every run where under
// real load it happens rarely and looks like a flake. The queue is then allowed to drain, because
// that is what separates this case from a stale socket: a listener that is alive resumes answering,
// and one whose process has gone never does. Only Accept dequeues, so the drain has to accept rather
// than merely close the held connections, which was worth learning the hard way in the server's
// equivalent test.
func TestListenDoesNotStealTheSocketOfABusyShim(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "s.sock")

	l, err := Listen(socket)
	if err != nil {
		t.Fatalf("first Listen() error = %v", err)
	}
	defer l.Close()

	// Fill the accept queue so the listener refuses, without yet calling Accept. Bounded well above
	// any real backlog so a failure to fill is reported rather than looping.
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	// The bound clears Linux's accept queue as well as darwin's, measured at 4097 and 128
	// respectively: 1024 was enough on one platform and silently not on the other.
	const limit = 8192
	for i := 0; i < limit; i++ {
		c, derr := net.Dial("unix", socket)
		if derr != nil {
			break
		}
		held = append(held, c)
	}
	if len(held) == 0 {
		t.Fatal("the listener refused the first dial, so it was never accepting")
	}
	if _, derr := net.Dial("unix", socket); derr == nil {
		t.Fatalf("dialed %s after %d connections without a refusal, so the queue never filled; "+
			"this test proves nothing", socket, len(held))
	}

	// Start accepting shortly, as a busy shim returning to its loop would. Well inside the grace, so
	// a correct Listen retries, finds the shim answering, and refuses to touch its socket.
	go func() {
		time.Sleep(20 * time.Millisecond)
		for {
			c, aerr := l.Accept()
			if aerr != nil {
				return
			}
			c.Close()
		}
	}()

	if _, err := Listen(socket); err == nil {
		t.Error("Listen() = nil error while a live shim held the socket, want refusal: " +
			"reclaiming it orphans that shim and its shell")
	}

	// And the socket still belongs to the original listener, which is the damage being prevented.
	if _, err := os.Stat(socket); err != nil {
		t.Errorf("Stat(%s) = %v after a failed Listen, want the live shim's socket left in place",
			socket, err)
	}
}

// A socket left behind by a shim killed with SIGKILL must still be reclaimable, or the session name
// becomes permanently unusable.
//
// The control for the test above, and the case that decides the tradeoff: a sustained refusal is
// treated as a stale socket rather than a busy shim. Distinct from TestListenReclaimsStaleSocket,
// which closes the listener normally and so unlinks the file, exercising the ENOENT path instead of
// this one.
func TestListenReclaimsASocketLeftByAKilledShim(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "s.sock")

	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		// Without this the file is unlinked on close and this is not the case being tested. A shim
		// killed with SIGKILL runs no cleanup, so its socket file survives.
		ul.SetUnlinkOnClose(false)
	}
	ln.Close()

	l, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen() over a socket left by a killed shim = %v, want reclamation: the session "+
			"name would otherwise be unusable until someone removed the file by hand", err)
	}
	l.Close()
}

// A shim that died without cleaning up leaves a socket behind. Reusing the name must work,
// or the session name would be permanently unusable.
func TestListenReclaimsStaleSocket(t *testing.T) {
	socket := filepath.Join(shortTempDir(t), "s.sock")

	l1, err := Listen(socket)
	if err != nil {
		t.Fatalf("first Listen() error = %v", err)
	}
	// Close without unlinking, imitating a crash.
	l1.Close()

	l2, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen() over a stale socket error = %v, want reclamation", err)
	}
	l2.Close()
}

func TestServiceWriteAfterExitReportsError(t *testing.T) {
	cl, session := startShim(t, Config{
		Session: "writeexit",
		Command: []string{"/bin/sh", "-c", "exit 0"},
		Rows:    24, Cols: 80,
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if exited, _ := session.Exited(); exited {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if _, err := cl.Write(context.Background(), &shimv1.WriteRequest{Data: []byte("x")}); err == nil {
		t.Error("Write() after exit = nil error, want failure")
	} else if errors.Is(err, context.Canceled) {
		t.Errorf("Write() error = %v, want a session error rather than cancellation", err)
	}
}

// shortTempDir returns a temp directory with a short path.
//
// t.TempDir() embeds the test name, which on macOS pushes the resulting socket path past
// the 104-byte sockaddr_un limit and fails at bind with a bare EINVAL. Real deployments
// use a short runtime directory, so this matches production rather than working around a
// test-only quirk.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cm")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
