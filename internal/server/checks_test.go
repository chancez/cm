package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
)

// A client and server from different builds is reported, and a matching pair is not.
//
// Worth a check because the failure is silent rather than loud. Protobuf ignores unknown fields, so a newer
// client asking an older server for something it does not implement gets a zero value: `cm wait --until
// blocked` against a server that predates reporting waits forever instead of failing, which looks like a
// broken feature.
func TestCheckVersionSkew(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client string
		want   bool
	}{
		{name: "matching versions", client: paths.Version(), want: false},
		{name: "different versions", client: "some-other-build", want: true},
		// A client too old to send the field at all, which is itself the mismatch being looked for.
		{name: "client reported nothing", client: "", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkVersionSkew(tc.client)
			if (len(got) > 0) != tc.want {
				t.Errorf("checkVersionSkew(%q) = %+v, want a finding = %v", tc.client, got, tc.want)
			}
			if len(got) > 0 && got[0].Kind != FindingVersionSkew {
				t.Errorf("kind = %q, want %q", got[0].Kind, FindingVersionSkew)
			}
		})
	}
}

// Errors in the server log are reported, since nothing else surfaces them.
//
// The log is where cm records what it could not act on: a terminal model that failed and disabled restore for
// a session, a store write that did not land. Those are deliberately not shown in the terminal, and the
// consequence is that nobody looks.
func TestCheckServerLogReportsErrors(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)

	logPath := dirs.ServerLog()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	// Real log lines, in the format slog writes, including one non-error so the check has to discriminate.
	lines := strings.Join([]string{
		`time=2026-08-08T00:00:00Z level=INFO msg="server starting"`,
		`time=2026-08-08T00:00:01Z level=ERROR msg="terminal model failed" session=work`,
		`time=2026-08-08T00:00:02Z level=WARN msg="expiring sessions failed"`,
		`time=2026-08-08T00:00:03Z level=ERROR msg="recording resume point failed" session=other`,
	}, "\n") + "\n"
	if err := os.WriteFile(logPath, []byte(lines), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got := mgr.checkServerLog()
	if len(got) != 1 {
		t.Fatalf("checkServerLog() = %+v, want one finding", got)
	}
	// Both errors counted, and neither the INFO nor the WARN line.
	if !strings.Contains(got[0].Detail, "2 error(s)") {
		t.Errorf("detail = %q, want it to count exactly the two ERROR lines", got[0].Detail)
	}
	// The messages are quoted, since a count alone does not help anyone debug.
	if !strings.Contains(got[0].Detail, "terminal model failed") {
		t.Errorf("detail = %q, want it to quote the error lines", got[0].Detail)
	}
}

// A log with no errors, or no log at all, is not a finding.
//
// A server that has just started may not have written one, and reporting that would be noise on a healthy
// installation, which trains a reader to ignore the command.
func TestCheckServerLogQuietWhenClean(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)

	if got := mgr.checkServerLog(); len(got) != 0 {
		t.Errorf("checkServerLog() with no log = %+v, want nothing", got)
	}

	logPath := dirs.ServerLog()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	// Including a session whose name contains "error", which is why the check matches on level= rather than
	// on the word appearing anywhere in the line.
	clean := `time=2026-08-08T00:00:00Z level=INFO msg="created session" session=error-repro` + "\n"
	if err := os.WriteFile(logPath, []byte(clean), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if got := mgr.checkServerLog(); len(got) != 0 {
		t.Errorf("checkServerLog() = %+v, want nothing: a session named error-repro is not an error", got)
	}
}

// A quoted error list is bounded, since a long-running server can accumulate hundreds.
func TestCheckServerLogBoundsWhatItQuotes(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)

	logPath := dirs.ServerLog()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	var b strings.Builder
	const total = maxLoggedErrors + 7
	for i := range total {
		b.WriteString("time=2026-08-08T00:00:00Z level=ERROR msg=\"failure number ")
		b.WriteString(itoa(i))
		b.WriteString("\"\n")
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got := mgr.checkServerLog()
	if len(got) != 1 {
		t.Fatalf("checkServerLog() = %+v, want one finding", got)
	}
	// The full count is reported even though only some lines are shown, so the reader knows the scale.
	if !strings.Contains(got[0].Detail, itoa(total)+" error(s)") {
		t.Errorf("detail = %q, want the full count of %d", got[0].Detail, total)
	}
	if n := strings.Count(got[0].Detail, "level=ERROR"); n > maxLoggedErrors {
		t.Errorf("detail quoted %d lines, want at most %d", n, maxLoggedErrors)
	}
}

// itoa avoids importing strconv for test messages.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// A build with no emulator is reported, since the symptom is a blank screen on reattach.
//
// The server logs this once at startup, which is the sort of message nobody sees, and a blank restore is
// indistinguishable from a bug in restore.
func TestCheckTerminal(t *testing.T) {
	withoutTerminal, _, _ := newTestManager(t, nil)
	if got := withoutTerminal.checkTerminal(); len(got) != 1 {
		t.Errorf("checkTerminal() with no emulator = %+v, want one finding", got)
	} else if got[0].Kind != FindingNoTerminal {
		t.Errorf("kind = %q, want %q", got[0].Kind, FindingNoTerminal)
	}

	withTerminal, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return &fakeTerminal{}, nil
	})
	if got := withTerminal.checkTerminal(); len(got) != 0 {
		t.Errorf("checkTerminal() with an emulator = %+v, want nothing", got)
	}
}

// A runtime directory long enough to threaten the socket path limit is reported before it fails.
//
// The failure at the limit is a bare EINVAL from bind with nothing about length in it, which is why this
// warns early rather than waiting for a session name long enough to break.
//
// The manager's directory is overwritten directly rather than through a constructor, since the check reads
// only that field and a real manager on a path this long could not open its store.
func TestCheckSocketPath(t *testing.T) {
	// A short directory has room to spare.
	short, _, _ := newTestManager(t, nil)
	short.dirs.Runtime = "/tmp/cm"
	if got := short.checkSocketPath(); len(got) != 0 {
		t.Errorf("checkSocketPath() for a short dir = %+v, want nothing", got)
	}

	// One that leaves no room for a plausible session name.
	long, _, _ := newTestManager(t, nil)
	long.dirs.Runtime = "/tmp/" + strings.Repeat("d", paths.MaxSocketPathLen)
	got := long.checkSocketPath()
	if len(got) != 1 {
		t.Fatalf("checkSocketPath() for a long dir = %+v, want one finding", got)
	}
	if got[0].Kind != FindingLongSocketPath {
		t.Errorf("kind = %q, want %q", got[0].Kind, FindingLongSocketPath)
	}
	// The message has to name the variable to set, since knowing the path is too long does not say what to do.
	if !strings.Contains(got[0].Detail, paths.Env("RUNTIME_DIR")) {
		t.Errorf("detail = %q, want it to name the environment variable", got[0].Detail)
	}
}

// Sessions whose shells never report OSC 133 are reported, and ones that have are not.
//
// Without those markers cm cannot tell busy from idle, so `cm wait --until idle` returns immediately and
// `cm list` never shows what is running. Nothing errors; the feature silently does nothing.
func TestCheckShellIntegration(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "quiet", "sleep 5")
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// Nothing reported yet, which is the case being looked for.
	got := mgr.checkShellIntegration()
	if len(got) != 1 {
		t.Fatalf("checkShellIntegration() = %+v, want one finding", got)
	}
	if !strings.Contains(got[0].Detail, "quiet") {
		t.Errorf("detail = %q, want it to name the session", got[0].Detail)
	}

	// A session that has reported a command has working integration, and stays fine after the command ends,
	// since the count is monotonic.
	sess, ok := mgr.Get("quiet")
	if !ok {
		t.Fatal("session was not adopted")
	}
	setBusy(sess, true, "make")
	setBusy(sess, false, "")
	if got := mgr.checkShellIntegration(); len(got) != 0 {
		t.Errorf("checkShellIntegration() = %+v, want nothing once a command has been reported", got)
	}
}

// A program reporting its own state counts as integration, since it answers the same question.
func TestCheckShellIntegrationAcceptsAReport(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimInRuntimeDir(t, dirs, "reported", "sleep 5")
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("reported")
	if !ok {
		t.Fatal("session was not adopted")
	}

	sess.setReported(Reported{State: "busy", Detail: "working"})
	if got := mgr.checkShellIntegration(); len(got) != 0 {
		t.Errorf("checkShellIntegration() = %+v, want nothing when a program reports its own state", got)
	}
}

// A pile of finished records is reported, and a handful is not.
//
// Two things caused this here, both silent: expiry gated on a config flag so a default install never cleaned
// up, and `cm run` records sharing the week-long lifetime of a deliberately persisted session.
func TestCheckSessionBacklog(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	// A few finished records are normal and must not be reported.
	for i := range 3 {
		if err := st.Create(ctx, store.Session{
			Name: "few" + itoa(i), State: store.StateExited,
		}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}
	if got := mgr.checkSessionBacklog(ctx); len(got) != 0 {
		t.Errorf("checkSessionBacklog() = %+v, want nothing for a few records", got)
	}

	for i := range backlogThreshold {
		if err := st.Create(ctx, store.Session{
			Name: "many" + itoa(i), State: store.StateExited,
		}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}

	got := mgr.checkSessionBacklog(ctx)
	if len(got) != 1 {
		t.Fatalf("checkSessionBacklog() = %+v, want one finding", got)
	}
	// With no policy installed, the message says they will never go, which is the actionable part.
	if !strings.Contains(got[0].Detail, "expiry is not configured") {
		t.Errorf("detail = %q, want it to say expiry is not configured", got[0].Detail)
	}

	// With expiry configured the finding stays, since the records are still in the way, but the explanation
	// changes: they are pending rather than permanent.
	policy := testPolicy()
	policy.ExpireAfter = time.Hour
	mgr.SetPersistPolicy(policy)
	got = mgr.checkSessionBacklog(ctx)
	if len(got) != 1 {
		t.Fatalf("checkSessionBacklog() = %+v, want one finding", got)
	}
	if strings.Contains(got[0].Detail, "expiry is not configured") {
		t.Errorf("detail = %q, want it to reflect the configured expiry", got[0].Detail)
	}
}
