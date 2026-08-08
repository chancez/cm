package e2e

import (
	"encoding/json"
	"strings"
	"testing"
)

// versionResult is the JSON shape `cm version` prints.
type versionResult struct {
	Client        string `json:"client"`
	Server        string `json:"server"`
	ServerRunning bool   `json:"server_running"`
	Terminal      bool   `json:"terminal"`
	Go            string `json:"go"`
	Platform      string `json:"platform"`
}

// cmVersion runs the command and parses its report.
func (e *env) cmVersion() versionResult {
	e.t.Helper()

	out := e.mustRun("version", "--json")
	var v versionResult
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		e.t.Fatalf("parsing version output %q: %v", out, err)
	}
	return v
}

// `cm version` reports both builds and whether this one has the emulator.
//
// Asked for because there was no way to tell what was running. Both versions matter because they are routinely
// different: a session outlives its server, so after an upgrade a new binary talks to a server the old one
// started, and a feature missing from one side waits forever rather than failing.
func TestVersionReportsClientAndServer(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	got := e.cmVersion()
	if got.Client == "" {
		t.Error("client version is empty")
	}
	// newEnv starts a server, so it must be reported as running rather than absent.
	if !got.ServerRunning {
		t.Error("server_running = false, want true: newEnv started one")
	}
	if got.Server == "" {
		t.Error("server version is empty while a server is running")
	}
	// Both sides are the same binary here, so they must agree. A mismatch would mean the client is reporting
	// something other than what it sent, or the server is echoing it back.
	if got.Client != got.Server {
		t.Errorf("client %q and server %q differ for one binary", got.Client, got.Server)
	}
	if got.Go == "" || got.Platform == "" {
		t.Errorf("version = %+v, want the Go version and platform filled in", got)
	}
}

// With no server running, the report says so rather than starting one.
//
// A diagnostic should not change what it reports on, and unlike `cm doctor` -- which needs a server to ask
// anything at all -- this has a useful answer without one. It is also the state right after `cm server stop`,
// where the next question is usually which binary is about to take over.
func TestVersionDoesNotStartAServer(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("server", "stop")
	e.waitServerGone()

	got := e.cmVersion()
	if got.ServerRunning {
		t.Errorf("server_running = true after stopping it, want false; version = %+v", got)
	}
	if got.Client == "" {
		t.Error("client version is empty")
	}

	// And no server was started as a side effect, which is the actual claim.
	e.waitServerGone()
	if e.serverIsRunning() {
		t.Error("`cm version` started a server")
	}
}

// The text output names both builds and flags a mismatch.
//
// The JSON is for scripts; this is what a person reads, and the mismatch line is the reason the command exists.
// Printing two versions and leaving the reader to compare them defeats the purpose, since the failure it warns
// about is silent.
func TestVersionTextFlagsAMismatch(t *testing.T) {
	skipIfShort(t)

	// The instrumented build, so the two sides can disagree without building from two tags.
	e := newEnvWith(t, cmVersionBinary(t), "v1.0.0")

	// A client claiming a newer build than the server that is running.
	e.version = "v2.0.0"
	out := e.mustRun("version")

	for _, want := range []string{"v2.0.0", "v1.0.0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not name %s:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "differs from this binary") {
		t.Errorf("output does not flag the mismatch:\n%s", out)
	}
	// And the advice is actionable rather than just a warning.
	if !strings.Contains(out, "server stop") {
		t.Errorf("output does not say how to resolve it:\n%s", out)
	}
}

// Matching versions produce no mismatch line.
//
// The complement: a warning on every run is one nobody reads, and this command is meant to be run casually.
func TestVersionQuietWhenVersionsMatch(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := e.mustRun("version")
	if strings.Contains(out, "differs from this binary") {
		t.Errorf("a mismatch was reported for one binary:\n%s", out)
	}
}

// --version prints something identifying the build.
//
// A flag as well as a subcommand because both are what people try first. Less detail than the subcommand, which
// is fine: this answers "what is this binary" without needing a server.
func TestVersionFlag(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := e.mustRun("--version")
	if strings.TrimSpace(out) == "" {
		t.Fatal("--version printed nothing")
	}
	// The same version the subcommand reports for the client, or the two disagree about one binary.
	if client := e.cmVersion().Client; !strings.Contains(out, client) {
		t.Errorf("--version printed %q, want it to contain the client version %q", out, client)
	}
}
