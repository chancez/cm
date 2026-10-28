package profiling

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func serveTest(t *testing.T) string {
	t.Helper()
	// os.MkdirTemp rather than t.TempDir, which embeds the test name and passes darwin's 104-byte socket
	// path limit.
	dir, err := os.MkdirTemp("", "cmprof")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "p.sock")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stop, err := Serve(ctx, socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	t.Cleanup(stop)
	return socket
}

// A fetched profile has to be one `go tool pprof` can read, or the feature produces files nobody can open.
// runtime/pprof writes gzipped protobuf, so a complete gzip stream with content is the check; parsing the
// protobuf would need github.com/google/pprof as a dependency for this test alone.
func TestFetchReturnsAReadableProfile(t *testing.T) {
	socket := serveTest(t)
	for _, name := range []string{"goroutine", "heap", "cpu", "block", "mutex"} {
		var buf bytes.Buffer
		if err := Fetch(context.Background(), socket, name, 0, &buf); err != nil {
			t.Fatalf("Fetch(%s) error = %v", name, err)
		}
		zr, err := gzip.NewReader(&buf)
		if err != nil {
			t.Fatalf("Fetch(%s) returned something that is not a profile: %v", name, err)
		}
		if body, err := io.ReadAll(zr); err != nil || len(body) == 0 {
			t.Errorf("Fetch(%s) profile body = %d bytes, %v, want a complete profile", name, len(body), err)
		}
	}
}

func TestFetchReturnsATrace(t *testing.T) {
	socket := serveTest(t)
	var buf bytes.Buffer
	if err := Fetch(context.Background(), socket, "trace", 0, &buf); err != nil {
		t.Fatalf("Fetch(trace) error = %v", err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("go 1.")) {
		t.Errorf("trace starts %q, want the runtime/trace header", buf.Bytes()[:min(16, buf.Len())])
	}
}

// Errors come back as errors, not as a file that looks like a profile and is not one.
func TestFetchReportsRefusals(t *testing.T) {
	socket := serveTest(t)
	for _, tc := range []struct {
		name    string
		seconds int
		want    string
	}{
		{"nosuch", 0, `no profile "nosuch"; want one of ` + joinNames()},
		{"cpu", MaxSeconds + 1, `seconds must be 0 to 300, got "301"`},
	} {
		var buf bytes.Buffer
		err := Fetch(context.Background(), socket, tc.name, tc.seconds, &buf)
		if err == nil || err.Error() != tc.want || buf.Len() != 0 {
			t.Errorf("Fetch(%s, %d) = %v with %d bytes, want %q and nothing written",
				tc.name, tc.seconds, err, buf.Len(), tc.want)
		}
	}
}

func joinNames() string {
	var b bytes.Buffer
	for i, n := range Names() {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(n)
	}
	return b.String()
}

func TestServeIsOwnerOnly(t *testing.T) {
	socket := serveTest(t)
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("socket mode = %v, want owner-only", perm)
	}
}

// Stopping unlinks the socket. A client serves one per pid, so leaving them would fill the runtime
// directory with a socket for every window ever opened.
func TestStopRemovesTheSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "cmprof")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "p.sock")
	stop, err := Serve(context.Background(), socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	stop()
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("Stat(socket) error = %v after stop, want it gone", err)
	}
}
