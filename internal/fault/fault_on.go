//go:build cm_testhooks

package fault

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chancez/cm/internal/paths"
)

// ErrInjected is what Err returns at a point configured to fail.
//
// One sentinel rather than one per point, since a caller cannot do anything different about a fault at one
// place than at another: the value exists so a test can tell an injected failure from a real one.
var ErrInjected = errors.New("injected fault")

// fault is what to do at one point.
type fault struct {
	kind kind
	// arg is the duration for a delay, or the path a pause waits for.
	dur  time.Duration
	path string
	// remaining counts down when the spec set a limit, and is negative for no limit. Atomic because
	// several goroutines reach the same point, and a limit that only sometimes held would make a test
	// nondeterministic in exactly the way this package exists to remove.
	remaining atomic.Int64
}

type kind int

const (
	kindDelay kind = iota
	kindPause
	kindError
)

// configured maps a point to its fault, built once from the environment.
//
// Read-only after init, so no lock is needed on lookup. The parse happens once rather than per call
// because a point can be in the pump's hot path, and a test that changed the spec mid-run would be
// describing a state no production build can be in.
var (
	once       sync.Once
	configured map[Point]*fault
)

// load parses the spec, reporting anything it cannot use.
//
// Reported to stderr rather than returned, because there is no caller: this runs from the first fault
// point reached, deep inside whatever was executing. Silence is the one option ruled out. A spec naming a
// point that does not exist injects nothing, and a test built on it passes while proving nothing, which is
// the failure mode this whole package is meant to expose rather than reproduce.
func load() {
	configured = map[Point]*fault{}
	spec := os.Getenv(paths.Env(SpecEnvSuffix))
	if spec == "" {
		return
	}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		f, p, err := parse(entry)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cm: ignoring fault %q: %v\n", entry, err)
			continue
		}
		configured[p] = f
	}
}

// parse reads one `point:type[=arg][:count=n]` entry.
func parse(entry string) (*fault, Point, error) {
	parts := strings.Split(entry, ":")
	if len(parts) < 2 {
		return nil, "", errors.New("want point:type[=arg][:count=n]")
	}
	p := Point(parts[0])
	if !points[p] {
		// Named rather than just rejected, since the usual cause is a typo and the list is short enough to
		// print.
		return nil, "", fmt.Errorf("unknown point, want one of %s", known())
	}

	f := &fault{}
	f.remaining.Store(-1)

	name, arg, _ := strings.Cut(parts[1], "=")
	switch name {
	case "delay":
		d, err := time.ParseDuration(arg)
		if err != nil || d <= 0 {
			return nil, "", fmt.Errorf("delay wants a positive duration, got %q", arg)
		}
		f.kind, f.dur = kindDelay, d
	case "pause":
		if arg == "" {
			return nil, "", errors.New("pause wants a path to wait for")
		}
		f.kind, f.path = kindPause, arg
	case "error":
		f.kind = kindError
	default:
		return nil, "", fmt.Errorf("unknown type %q, want delay, pause or error", name)
	}

	for _, extra := range parts[2:] {
		k, v, ok := strings.Cut(extra, "=")
		if !ok || k != "count" {
			return nil, "", fmt.Errorf("unknown option %q, want count=n", extra)
		}
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
			return nil, "", fmt.Errorf("count wants a positive number, got %q", v)
		}
		f.remaining.Store(n)
	}
	return f, p, nil
}

func known() string {
	names := make([]string, 0, len(points))
	for p := range points {
		names = append(names, string(p))
	}
	return strings.Join(names, ", ")
}

// take reports whether this fault should fire now, consuming one of its allowance.
func (f *fault) take() bool {
	for {
		n := f.remaining.Load()
		if n < 0 {
			return true
		}
		if n == 0 {
			return false
		}
		if f.remaining.CompareAndSwap(n, n-1) {
			return true
		}
	}
}

// lookup returns the fault at a point, or nil.
func lookup(p Point) *fault {
	once.Do(load)
	f := configured[p]
	if f == nil || !f.take() {
		return nil
	}
	return f
}

// At applies a delay or a pause configured at this point.
//
// An error configured here is ignored, deliberately: a site calling At cannot report one, and quietly
// swallowing it would be worse than saying so. Reported once rather than silently, for the same reason
// load reports a bad spec.
func At(p Point) {
	f := lookup(p)
	if f == nil {
		return
	}
	switch f.kind {
	case kindDelay:
		time.Sleep(f.dur)
	case kindPause:
		waitFor(f.path)
	case kindError:
		fmt.Fprintf(os.Stderr, "cm: fault %q is an error but %s cannot fail; use a point reached "+
			"through fault.Err\n", "error", p)
	}
}

// Err applies the fault at this point and reports whether it should fail.
func Err(p Point) error {
	f := lookup(p)
	if f == nil {
		return nil
	}
	switch f.kind {
	case kindDelay:
		time.Sleep(f.dur)
	case kindPause:
		waitFor(f.path)
	case kindError:
		return fmt.Errorf("%s: %w", p, ErrInjected)
	}
	return nil
}

// Enabled reports whether any fault is configured.
func Enabled() bool {
	once.Do(load)
	return len(configured) > 0
}

// resetForTest discards the parsed spec so the next call re-reads the environment.
//
// Only for this package's own tests, which set the variable per case. Production parses once, deliberately:
// a point can sit in the pump's hot path, and a spec that changed mid-run would describe a state no
// released build can be in.
func resetForTest() {
	once = sync.Once{}
	configured = nil
}

// touch creates an empty file, for releasing a pause from a test.
func touch(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	return f.Close()
}

// waitFor blocks until path exists.
//
// A file rather than a signal or a socket, because the coordination has to cross a process boundary: the
// test is a Go test and the paused code is inside a server or a shim that the test spawned. A path both
// sides already agree on is the cheapest thing that works, and creating it is one line in a test.
//
// Bounded, because a test that forgets to release a pause should fail rather than hang until the whole
// suite's timeout, where the failure names neither the test nor the point. Polled rather than watched,
// since a filesystem watcher is a dependency and a platform difference for something that waits once.
func waitFor(path string) {
	const limit = 30 * time.Second
	deadline := time.Now().Add(limit)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "cm: fault pause gave up after %s waiting for %s; "+
				"the test did not release it\n", limit, path)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}
