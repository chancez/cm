package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// `cm debug pprof` reaches the shim behind a session and the server, through real processes. Each serves
// its own socket only if it read `[debug] pprof` when it started, which is the wiring a unit test of the
// profiling package cannot see.
func TestDebugPprofProfilesTheShimAndTheServer(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.writeConfig("[debug]\npprof = true\n")
	// The harness started its server before the setting existed, and a server reads it once at startup.
	e.restartServer()
	e.mustRun("run", "-d", "--session", "prof", "--", "/bin/sh", "-c", "sleep 30")
	defer e.run("kill", "prof")

	out := t.TempDir()
	for name, args := range map[string][]string{
		"shim":   {"prof"},
		"server": {"--server"},
	} {
		path := filepath.Join(out, name+".pprof")
		if r := e.run(append([]string{"debug", "pprof", "--profile", "goroutine", "-o", path}, args...)...); r.code != 0 {
			t.Fatalf("%s: debug pprof exited %d: %s", name, r.code, r.stderr)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: ReadFile() error = %v", name, err)
		}
		// runtime/pprof writes gzipped protobuf.
		if !bytes.HasPrefix(got, []byte{0x1f, 0x8b}) {
			t.Errorf("%s profile starts %q, want a gzipped pprof profile", name, got[:min(8, len(got))])
		}
	}
}

// Off by default, and asking anyway says what to set rather than failing with a bare dial error.
func TestDebugPprofSaysHowToEnableIt(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("run", "-d", "--session", "noprof", "--", "/bin/sh", "-c", "sleep 30")
	defer e.run("kill", "noprof")

	path := filepath.Join(t.TempDir(), "x.pprof")
	r := e.run("debug", "pprof", "--profile", "goroutine", "-o", path, "--server")
	if r.code == 0 || !strings.Contains(r.stderr, "[debug] pprof = true") {
		t.Errorf("debug pprof without the setting = exit %d, stderr %q, want a failure naming the setting",
			r.code, r.stderr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Stat(%s) error = %v, want no file left from a failed fetch", path, err)
	}
}

// The flight recorder works with profiles off, and profiles stay refused: the two settings are independent.
func TestDebugFlightWithoutPprof(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.writeConfig("[debug]\nflight_recorder = true\n")
	e.restartServer()
	e.mustRun("run", "-d", "--session", "fl", "--", "/bin/sh", "-c", "sleep 30")
	defer e.run("kill", "fl")

	out := t.TempDir()
	for name, args := range map[string][]string{
		"shim":   {"fl"},
		"server": {"--server"},
	} {
		path := filepath.Join(out, name+".trace")
		if r := e.run(append([]string{"debug", "flight", "-o", path}, args...)...); r.code != 0 {
			t.Fatalf("%s: debug flight exited %d: %s", name, r.code, r.stderr)
		}
		if got := readTrace(t, path); got != "" {
			t.Errorf("%s: %s", name, got)
		}
	}

	r := e.run("debug", "pprof", "--profile", "goroutine", "-o", filepath.Join(out, "x.pprof"), "--server")
	if r.code == 0 || !strings.Contains(r.stderr, "[debug] pprof = true") {
		t.Errorf("debug pprof with only the recorder on = exit %d, stderr %q, want a refusal naming pprof",
			r.code, r.stderr)
	}
}

// --flight writes the recorder beside the profile, after it.
func TestDebugPprofWithFlight(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.writeConfig("[debug]\npprof = true\nflight_recorder = true\n")
	e.restartServer()
	// Debug commands never start a server, since a fresh one has nothing to show; this does.
	e.mustRun("list")

	path := filepath.Join(t.TempDir(), "server.pprof")
	if r := e.run("debug", "pprof", "--server", "--profile", "goroutine", "--flight", "-o", path); r.code != 0 {
		t.Fatalf("debug pprof --flight exited %d: %s", r.code, r.stderr)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.HasPrefix(got, []byte{0x1f, 0x8b}) {
		t.Errorf("profile at %s: err %v, want a gzipped pprof profile", path, err)
	}
	if got := readTrace(t, strings.TrimSuffix(path, ".pprof")+"-flight.trace"); got != "" {
		t.Error(got)
	}
}

// readTrace returns why the file at path is not a runtime/trace, or "" if it is one.
func readTrace(t *testing.T, path string) string {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	if !bytes.HasPrefix(got, []byte("go 1.")) {
		return "trace at " + path + " starts " + strconvQuote(got[:min(8, len(got))]) + ", want a runtime/trace"
	}
	return ""
}

func strconvQuote(b []byte) string { return strconv.Quote(string(b)) }
