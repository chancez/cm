package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/transport"
)

// proxyDirs returns a runtime directory for a proxy test.
//
// os.MkdirTemp rather than t.TempDir, which embeds the test name and blows past the 104-byte cap on a unix
// socket path on darwin. paths.MaxSocketPathLen holds the limit and the failure is a bare EINVAL.
func proxyDirs(t *testing.T) paths.Dirs {
	t.Helper()

	dir, err := os.MkdirTemp("", "cmproxy")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v, want nil", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	dirs := paths.Dirs{Runtime: dir, State: filepath.Join(dir, "state")}
	if err := paths.CheckSocketPath(dirs.ServerSocket()); err != nil {
		t.Fatalf("the test's own socket path is unusable: %v", err)
	}
	return dirs
}

// The proxy announces itself and then carries the conversation in both directions, and a client that
// stops talking does not cut off what the server has already said.
//
// The last part is what CloseWrite is for: a detach is the client saying goodbye, and the acknowledgement
// is on its way back. A full close on the socket would lose it, and the loss would look like a server that
// failed to answer.
func TestServerProxyForwardsBothWaysAndHalfCloses(t *testing.T) {
	dirs := proxyDirs(t)

	l, err := net.Listen("unix", dirs.ServerSocket())
	if err != nil {
		t.Fatalf("Listen() error = %v, want nil", err)
	}
	defer l.Close()

	served := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			served <- "accept: " + err.Error()
			return
		}
		defer conn.Close()

		// Read to EOF, which only arrives if the proxy half-closed rather than simply stopping.
		got, err := io.ReadAll(conn)
		if err != nil {
			served <- "read: " + err.Error()
			return
		}
		// Written after that EOF, so it can only arrive if the socket is still open this way.
		if _, err := io.WriteString(conn, "goodbye"); err != nil {
			served <- "write: " + err.Error()
			return
		}
		served <- string(got)
	}()

	clientRead, clientWrite := io.Pipe()
	go func() {
		_, _ = io.WriteString(clientWrite, "hello")
		clientWrite.Close()
	}()

	// Run with a bound rather than inline, because the failure this guards against is a hang rather than a
	// wrong value: without the half-close the server never reads EOF, so it never answers, so the proxy
	// never sees its side close. Waited on here so the test says that, instead of being killed by go test's
	// own timeout minutes later with a stack dump.
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- runServerProxy(t.Context(), dirs, clientRead, &out, false) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runServerProxy() error = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runServerProxy() did not return, so the server was never told the client had finished")
	}

	if got := <-served; got != "hello" {
		t.Errorf("the server received %q, want %q", got, "hello")
	}
	// The whole of what the client saw: the banner, then everything the server said.
	want := transport.ProxyBanner(paths.Version()) + "goodbye"
	if out.String() != want {
		t.Errorf("the client received %q, want %q", out.String(), want)
	}
}

// Without --start the proxy refuses rather than starting a server, because the decision to start one has a
// policy behind it that lives in the client: it waits out a quiet period, and it leaves a server that was
// stopped on purpose alone. A proxy that always started one would defeat `cm server stop` on the remote
// from any window that reconnected.
func TestServerProxyWithoutStartDoesNotStartAServer(t *testing.T) {
	dirs := proxyDirs(t)

	var out bytes.Buffer
	err := runServerProxy(t.Context(), dirs, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("runServerProxy() error = nil, want an error")
	}
	// Named as a remote fact, since the message is read on the other machine.
	if !strings.Contains(err.Error(), "no cm server is running on ") {
		t.Errorf("error %q does not say which host has no server", err)
	}
	// Nothing announced, because there is nothing behind it to announce: a banner here would tell the
	// dialer the connection was usable.
	if out.Len() != 0 {
		t.Errorf("the client received %q, want nothing", out.String())
	}
	if _, err := os.Stat(dirs.ServerSocket()); err == nil {
		t.Error("a server socket exists, so a server was started after all")
	}
}
