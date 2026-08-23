package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/paths"
)

// A server that failed to start has usually said why, and that has to reach the user.
//
// It did not: the spawned server's stderr went to /dev/null, so a database from a newer build, an
// unwritable runtime directory, and a genuinely slow start were all reported as the same timeout. The
// message named the deadline and discarded the cause.
func TestServerStartErrorReportsWhatTheServerSaid(t *testing.T) {
	dirs := paths.Dirs{Runtime: t.TempDir(), State: t.TempDir()}
	said := "cm: /state/cm.db is at schema version 99 and this build knows 7"
	if err := os.WriteFile(dirs.ServerStartErr(), []byte(said+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := serverStartError(dirs)
	if err == nil {
		t.Fatal("serverStartError() = nil, want an error")
	}
	if !strings.Contains(err.Error(), said) {
		t.Errorf("serverStartError() = %v, want it to carry what the server said", err)
	}
	// Still says the wait failed, since that is what the caller observed.
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Errorf("serverStartError() = %v, want it to name the timeout as well", err)
	}
}

// With nothing captured, the next best thing is naming the command that shows the rest. A bare timeout
// leaves a user with no next step, which is what sent them looking for `cm server` by guesswork.
func TestServerStartErrorPointsAtTheForegroundServer(t *testing.T) {
	dirs := paths.Dirs{Runtime: t.TempDir(), State: t.TempDir()}

	err := serverStartError(dirs)
	if err == nil {
		t.Fatal("serverStartError() = nil, want an error")
	}
	if !strings.Contains(err.Error(), paths.Name+" server") {
		t.Errorf("serverStartError() = %v, want it to name `%s server`", err, paths.Name)
	}
}

// An empty file is the same as no file: the server was killed or is merely slow, and there is nothing to
// quote. Worth pinning because quoting emptiness would produce an error ending in a colon and nothing.
func TestServerStartErrorIgnoresAnEmptyCapture(t *testing.T) {
	dirs := paths.Dirs{Runtime: t.TempDir(), State: t.TempDir()}
	if err := os.WriteFile(dirs.ServerStartErr(), []byte("\n\n  \n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	err := serverStartError(dirs)
	if err == nil {
		t.Fatal("serverStartError() = nil, want an error")
	}
	if strings.HasSuffix(strings.TrimSpace(err.Error()), ":") {
		t.Errorf("serverStartError() = %v, want no dangling colon for an empty capture", err)
	}
	if !strings.Contains(err.Error(), paths.Name+" server") {
		t.Errorf("serverStartError() = %v, want it to name `%s server`", err, paths.Name)
	}
}

// A start nobody notices must stay silent, so the ordinary first command in a shell prints nothing.
func TestStartNoticeSaysNothingAboutAFastStart(t *testing.T) {
	base := time.Unix(0, 0)
	var buf bytes.Buffer
	n := &startNotice{w: &buf, began: base}

	n.waiting(base.Add(serverStartQuiet - time.Millisecond))
	n.ready(base.Add(serverStartQuiet))

	if buf.Len() > 0 {
		t.Errorf("output = %q, want nothing for a start inside the quiet period", buf.String())
	}
}

// A start that takes a while has to say so, keep saying so, and say when it ended.
//
// The whole transcript is asserted rather than a line at a time: what made this worth writing is a wait
// that printed nothing for ten seconds and then failed, which reads as a hang, and the surrounding order
// is what tells a reader the difference between waiting and stuck.
func TestStartNoticeReportsAWaitAndItsEnd(t *testing.T) {
	base := time.Unix(0, 0)
	var buf bytes.Buffer
	n := &startNotice{w: &buf, began: base}

	n.waiting(base.Add(10 * time.Millisecond))                        // quiet
	n.waiting(base.Add(serverStartQuiet))                             // first line
	n.waiting(base.Add(serverStartQuiet + 100*time.Millisecond))      // too soon to repeat
	n.waiting(base.Add(serverStartQuiet + serverStartNoticeInterval)) // reminder
	n.ready(base.Add(3 * time.Second))

	want := fmt.Sprintf("%[1]s: waiting for the server to start...\n"+
		"%[1]s: still waiting for the server, 2.3s of %s\n"+
		"%[1]s: server ready after 3s\n", paths.Name, serverStartTimeout)
	if got := buf.String(); got != want {
		t.Errorf("output =\n%q\nwant\n%q", got, want)
	}
}

// The capture lives in the runtime directory, which is where an attempt to start belongs: it is about one
// attempt rather than a running server, and logs/server is scanned by doctor for diagnostic logs.
func TestServerStartErrLivesInTheRuntimeDir(t *testing.T) {
	dirs := paths.Dirs{Runtime: "/run/cm", State: "/state/cm"}
	if got, want := filepath.Dir(dirs.ServerStartErr()), "/run/cm"; got != want {
		t.Errorf("ServerStartErr() is in %q, want %q", got, want)
	}
}
