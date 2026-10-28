// Package profiling serves a cm process's runtime profiles on a unix socket, for `cm debug pprof`.
//
// Off unless `[debug] pprof = true`. The server, every shim, and every attached client serve their own,
// because a slowdown can live in any of the three and the symptom does not say which: scrolling nvim
// under cm was slow because the *shim* rewrote its persisted log on every pty read, which nothing on the
// server or client side would have shown.
//
// One request line and the raw profile back, rather than net/http/pprof. net/http is not otherwise in
// the binary, and linking it was measured at 3.5 MB (28.2 to 31.7 MB) and about 1ms on every cm
// invocation, for a feature that is off by default. The profiles are what runtime/pprof and
// runtime/trace write, so `go tool pprof` and `go tool trace` read the result unchanged.
package profiling

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"runtime/pprof"
	"runtime/trace"
	"strconv"
	"strings"
	"sync"
	"time"
)

// MaxSeconds bounds a sampled profile, so a typo cannot leave a process profiling for a day.
const MaxSeconds = 300

// Sampled lists the profiles that are collected over a duration rather than read as a snapshot.
//
// block and mutex are here because their rates are zero unless something sets them, and leaving them
// on costs every lock in the process. They are switched on for the duration asked and off afterwards,
// so the profile covers that window and nothing else pays for it.
var Sampled = []string{"cpu", "trace", "block", "mutex"}

// Serve answers profile requests on socket until ctx ends or the returned func is called, and returns once
// it is listening. The func closes the listener, which unlinks the socket, so a process that calls it on the
// way out leaves nothing behind.
//
// A stale socket at the path is removed first. The name is unique to this process's role (the server,
// one session's ID, one client's pid), so whatever is there belonged to a predecessor that is gone.
func Serve(ctx context.Context, socket string, log *slog.Logger) (func(), error) {
	_ = os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("listening for profile requests: %w", err)
	}
	// Owner-only, as for every socket cm binds. The runtime directory is 0700 already; this holds even
	// when CM_RUNTIME_DIR points somewhere shared, which loose-dir-perms exists to catch.
	if err := os.Chmod(socket, 0o600); err != nil {
		l.Close()
		return nil, fmt.Errorf("restricting profile socket: %w", err)
	}
	var once sync.Once
	stop := func() { once.Do(func() { l.Close() }) }
	go func() {
		<-ctx.Done()
		stop()
	}()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if err := answer(ctx, conn); err != nil {
					log.Warn("profile request failed", "error", err)
				}
			}()
		}
	}()
	log.Info("serving profiles", "socket", socket)
	return stop, nil
}

// answer reads one request, `<profile> <seconds>`, and writes `ok` or `error: ...` on a line, then the
// profile.
func answer(ctx context.Context, conn net.Conn) error {
	line, err := bufio.NewReader(io.LimitReader(conn, 256)).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading request: %w", err)
	}
	name, seconds, err := parseRequest(line)
	if err != nil {
		fmt.Fprintf(conn, "error: %v\n", err)
		return err
	}
	collect, err := collector(name)
	if err != nil {
		fmt.Fprintf(conn, "error: %v\n", err)
		return err
	}
	// Status before the profile, so a refusal from the runtime (a CPU profile already running) is told
	// apart from a profile that happens to start with "error". The runtime reports that refusal only when
	// starting, which is before any profile bytes, hence the deferred header.
	w := &statusWriter{w: conn}
	if err := collect(ctx, w, time.Duration(seconds)*time.Second); err != nil {
		if !w.started {
			fmt.Fprintf(conn, "error: %v\n", err)
		}
		return err
	}
	return w.start()
}

func parseRequest(line string) (string, int, error) {
	fields := strings.Fields(line)
	if len(fields) != 2 {
		return "", 0, fmt.Errorf("want `<profile> <seconds>`, got %q", strings.TrimSpace(line))
	}
	seconds, err := strconv.Atoi(fields[1])
	if err != nil || seconds < 0 || seconds > MaxSeconds {
		return "", 0, fmt.Errorf("seconds must be 0 to %d, got %q", MaxSeconds, fields[1])
	}
	return fields[0], seconds, nil
}

// statusWriter writes the `ok` line ahead of the first profile byte.
type statusWriter struct {
	w       io.Writer
	started bool
}

func (s *statusWriter) start() error {
	if s.started {
		return nil
	}
	s.started = true
	_, err := io.WriteString(s.w, "ok\n")
	return err
}

func (s *statusWriter) Write(p []byte) (int, error) {
	if err := s.start(); err != nil {
		return 0, err
	}
	return s.w.Write(p)
}

type collectFunc func(ctx context.Context, w *statusWriter, d time.Duration) error

func collector(name string) (collectFunc, error) {
	switch name {
	case "cpu":
		return func(ctx context.Context, w *statusWriter, d time.Duration) error {
			if err := pprof.StartCPUProfile(w); err != nil {
				return err
			}
			wait(ctx, d)
			pprof.StopCPUProfile()
			return nil
		}, nil
	case "trace":
		return func(ctx context.Context, w *statusWriter, d time.Duration) error {
			if err := trace.Start(w); err != nil {
				return err
			}
			wait(ctx, d)
			trace.Stop()
			return nil
		}, nil
	case "block":
		return windowed(name, func(on bool) { runtime.SetBlockProfileRate(boolRate(on)) }), nil
	case "mutex":
		return windowed(name, func(on bool) { runtime.SetMutexProfileFraction(boolRate(on)) }), nil
	}
	if pprof.Lookup(name) == nil {
		return nil, fmt.Errorf("no profile %q; want one of %s", name, strings.Join(Names(), ", "))
	}
	return func(_ context.Context, w *statusWriter, _ time.Duration) error {
		return pprof.Lookup(name).WriteTo(w, 0)
	}, nil
}

// Names lists every profile a request can name.
func Names() []string {
	names := []string{"cpu", "trace"}
	for _, p := range pprof.Profiles() {
		names = append(names, p.Name())
	}
	return names
}

func boolRate(on bool) int {
	if on {
		return 1
	}
	return 0
}

// windowed switches a profile's sampling on for the duration, then writes what it recorded.
func windowed(name string, enable func(bool)) collectFunc {
	return func(ctx context.Context, w *statusWriter, d time.Duration) error {
		enable(true)
		wait(ctx, d)
		enable(false)
		return pprof.Lookup(name).WriteTo(w, 0)
	}
}

func wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// Fetch asks the process serving socket for a profile and copies it to w.
func Fetch(ctx context.Context, socket, name string, seconds int, w io.Writer) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("nothing is serving profiles at %s; is `[debug] pprof = true` set, "+
				"and was the process started after it was?", socket)
		}
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if _, err := fmt.Fprintf(conn, "%s %d\n", name, seconds); err != nil {
		return err
	}
	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading the profile: %w", err)
	}
	if status = strings.TrimSuffix(status, "\n"); status != "ok" {
		return errors.New(strings.TrimPrefix(status, "error: "))
	}
	_, err = io.Copy(w, r)
	return err
}
