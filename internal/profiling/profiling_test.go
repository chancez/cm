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
	"time"
)

func serveTest(t *testing.T) string {
	t.Helper()
	return serveWith(t, Options{Profiles: true})
}

func serveWith(t *testing.T, opts Options) string {
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
	stop, err := Serve(ctx, socket, opts, slog.New(slog.DiscardHandler))
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
	if !isTrace(buf.Bytes()) {
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
	stop, err := Serve(context.Background(), socket, Options{Profiles: true, FlightWindow: time.Second},
		slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	stop()
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("Stat(socket) error = %v after stop, want it gone", err)
	}
}

// isTrace reports whether b is what runtime/trace writes, which `go tool trace` reads.
func isTrace(b []byte) bool { return bytes.HasPrefix(b, []byte("go 1.")) }

// The recorder's window comes back as a trace, and it does so without profiles being on.
func TestFetchFlightWithoutProfiles(t *testing.T) {
	socket := serveWith(t, Options{FlightWindow: time.Second})
	var buf bytes.Buffer
	if err := Fetch(context.Background(), socket, Flight, 0, &buf); err != nil {
		t.Fatalf("Fetch(flight) error = %v", err)
	}
	if !isTrace(buf.Bytes()) {
		t.Errorf("flight starts %q, want the runtime/trace header", buf.Bytes()[:min(16, buf.Len())])
	}
}

// Each feature refuses when it is off, naming the setting, whatever the other is set to.
func TestEachFeatureRefusesWhenOff(t *testing.T) {
	for _, tc := range []struct {
		opts Options
		name string
		want error
	}{
		{Options{FlightWindow: time.Second}, "goroutine", errProfilesOff},
		{Options{FlightWindow: time.Second}, "cpu", errProfilesOff},
		{Options{Profiles: true}, Flight, errFlightOff},
	} {
		// A subtest each, so one's recorder is stopped before the next starts: the runtime allows one.
		t.Run(tc.name, func(t *testing.T) {
			socket := serveWith(t, tc.opts)
			var buf bytes.Buffer
			err := Fetch(context.Background(), socket, tc.name, 0, &buf)
			if err == nil || err.Error() != tc.want.Error() || buf.Len() != 0 {
				t.Errorf("%+v: Fetch(%s) = %v with %d bytes, want %q and nothing written",
					tc.opts, tc.name, err, buf.Len(), tc.want)
			}
		})
	}
}

// A live trace and the flight recorder run at once: `cm debug pprof --profile trace --flight` asks for
// both, and the runtime would refuse the second if they could not share.
func TestLiveTraceAlongsideTheRecorder(t *testing.T) {
	socket := serveWith(t, Options{Profiles: true, FlightWindow: time.Second})
	var live, flight bytes.Buffer
	if err := Fetch(context.Background(), socket, "trace", 1, &live); err != nil {
		t.Fatalf("Fetch(trace) error = %v", err)
	}
	if err := Fetch(context.Background(), socket, Flight, 0, &flight); err != nil {
		t.Fatalf("Fetch(flight) error = %v", err)
	}
	if !isTrace(live.Bytes()) || !isTrace(flight.Bytes()) {
		t.Errorf("live %q, flight %q, want both to be traces",
			live.Bytes()[:min(16, live.Len())], flight.Bytes()[:min(16, flight.Len())])
	}
}
