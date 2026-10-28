package e2e

import (
	"bytes"
	"os"
	"path/filepath"
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
