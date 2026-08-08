package server

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// logAt formats a log line the way slog's TextHandler does.
//
// Built from a time rather than hardcoded, because the check now filters on age: a test with a literal
// timestamp would pass today and start failing a day later, which is the worst kind of test.
func logAt(when time.Time, level, msg string) string {
	return "time=" + when.Format(time.RFC3339Nano) + " level=" + level + " msg=" + `"` + msg + `"`
}

// writeLog writes lines to a log file, creating the directory.
func writeLog(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

// Errors and warnings are reported separately, and INFO is not reported at all.
//
// Separate findings rather than one so a script can gate on errors while merely noticing warnings, and so a
// pile of warnings cannot crowd a single error out of a truncated list.
//
// Warnings matter more here than the count suggests. cm logs 22 of them against 9 errors, and the warnings are
// the substantive ones: "adopting session failed", "rebuilding the screen for an adopted session failed",
// "replaying persisted session failed". Those are the silent failures behind the blank-screen-on-reattach
// bug, and the previous version of this check could not see any of them.
func TestCheckLogsSeparatesErrorsFromWarnings(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	now := time.Now()

	writeLog(t, dirs.ServerLog(),
		logAt(now.Add(-time.Minute), "INFO", "server starting"),
		logAt(now.Add(-2*time.Minute), "ERROR", "terminal model failed"),
		logAt(now.Add(-3*time.Minute), "WARN", "adopting session failed"),
		logAt(now.Add(-4*time.Minute), "ERROR", "recording resume point failed"),
	)

	got := mgr.checkLogs(now)
	if len(got) != 2 {
		t.Fatalf("checkLogs() = %+v, want two findings, one per level", got)
	}

	byKind := map[string]Finding{}
	for _, f := range got {
		byKind[f.Kind] = f
	}
	errs, ok := byKind[FindingServerErrors]
	if !ok {
		t.Fatalf("no %s finding in %+v", FindingServerErrors, got)
	}
	warns, ok := byKind[FindingLogWarnings]
	if !ok {
		t.Fatalf("no %s finding in %+v", FindingLogWarnings, got)
	}

	if !strings.Contains(errs.Detail, "2 errors") {
		t.Errorf("error detail = %q, want it to count exactly the two ERROR lines", errs.Detail)
	}
	if !strings.Contains(warns.Detail, "1 warning") {
		t.Errorf("warning detail = %q, want it to count exactly the one WARN line", warns.Detail)
	}
	// The messages are quoted, since a count alone does not help anyone debug.
	if !strings.Contains(errs.Detail, "terminal model failed") {
		t.Errorf("error detail does not quote the line: %q", errs.Detail)
	}
	if !strings.Contains(warns.Detail, "adopting session failed") {
		t.Errorf("warning detail does not quote the line: %q", warns.Detail)
	}
	// Levels do not bleed across findings, which is the whole point of separating them.
	if strings.Contains(errs.Detail, "adopting session failed") {
		t.Errorf("the error finding includes a warning: %q", errs.Detail)
	}
	if strings.Contains(warns.Detail, "terminal model failed") {
		t.Errorf("the warning finding includes an error: %q", warns.Detail)
	}
	// INFO is never reported: a healthy server logs plenty of it, and reporting it would make the command
	// useless on every installation.
	for _, f := range got {
		if strings.Contains(f.Detail, "server starting") {
			t.Errorf("an INFO line was reported: %q", f.Detail)
		}
	}
}

// Entries older than the staleness window are not reported.
//
// This came from running the command: it reported three errors about sessions named kitty.1 and kitty.5 that
// had been dead for nineteen hours, from a file six server generations had appended to. That is history, not a
// diagnosis, and a check that reports last week's resolved problems on every run is one people learn to
// ignore.
func TestCheckLogsIgnoresOldEntries(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	now := time.Now()

	writeLog(t, dirs.ServerLog(),
		// Just inside the window, and just outside it. Testing the boundary rather than a comfortable margin,
		// since an off-by-one in the comparison is the plausible mistake.
		logAt(now.Add(-logStaleAfter+time.Minute), "ERROR", "recent enough to matter"),
		logAt(now.Add(-logStaleAfter-time.Minute), "ERROR", "old news from a previous server"),
	)

	got := mgr.checkLogs(now)
	if len(got) != 1 {
		t.Fatalf("checkLogs() = %+v, want one finding", got)
	}
	if !strings.Contains(got[0].Detail, "1 error") {
		t.Errorf("detail = %q, want it to count only the recent error", got[0].Detail)
	}
	if strings.Contains(got[0].Detail, "old news") {
		t.Errorf("an entry outside the window was reported: %q", got[0].Detail)
	}
}

// A shim's log is scanned, not just the server's.
//
// The previous version read only server.log, so a shim's "persisting output failed, session will not survive a
// reboot" went into a file nothing opened. A shim is the process holding the pty, which makes its failures the
// ones a user actually feels.
func TestCheckLogsReadsShimLogs(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	now := time.Now()

	// A clean server log, so anything found has to have come from the shim's.
	writeLog(t, dirs.ServerLog(), logAt(now, "INFO", "server starting"))
	writeLog(t, dirs.ShimLog("work"),
		logAt(now, "ERROR", "persisting output failed, session will not survive a reboot"))

	got := mgr.checkLogs(now)
	if len(got) != 1 {
		t.Fatalf("checkLogs() = %+v, want the shim's error reported", got)
	}
	if !strings.Contains(got[0].Detail, "persisting output failed") {
		t.Errorf("detail does not include the shim's error: %q", got[0].Detail)
	}
	// The file is named, and named in a way that says what kind of log it is: a shim log and a session's output
	// log share a base name now that the logs are split by directory, so "work.log" alone would be ambiguous.
	if !strings.Contains(got[0].Detail, "shim/work.log") {
		t.Errorf("detail does not say which log it came from: %q", got[0].Detail)
	}
}

// The newest entries are the ones quoted when the list is truncated.
//
// The old check quoted the first few in file order, which after a rotation are the least relevant lines in the
// file. What explains a problem happening now is what was logged most recently.
func TestCheckLogsQuotesTheNewestAndBoundsTheList(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	now := time.Now()

	var lines []string
	// Written oldest first, so file order is the opposite of the wanted order and a check that ignored
	// timestamps would quote the wrong ones.
	for i := maxLoggedLines * 2; i >= 1; i-- {
		lines = append(lines, logAt(now.Add(-time.Duration(i)*time.Minute), "ERROR", "failure "+strconv.Itoa(i)))
	}
	writeLog(t, dirs.ServerLog(), lines...)

	got := mgr.checkLogs(now)
	if len(got) != 1 {
		t.Fatalf("checkLogs() = %+v, want one finding", got)
	}

	// Every error is counted even though only some are shown, so the count is not silently capped.
	if !strings.Contains(got[0].Detail, "10 errors") {
		t.Errorf("detail = %q, want all 10 counted", got[0].Detail)
	}
	if n := strings.Count(got[0].Detail, "level=ERROR"); n != maxLoggedLines {
		t.Errorf("detail quoted %d lines, want %d", n, maxLoggedLines)
	}
	// The truncation is stated, so a reader is not left thinking five is all there was.
	if !strings.Contains(got[0].Detail, "showing 5") {
		t.Errorf("detail does not say the list was truncated: %q", got[0].Detail)
	}
	// "failure 1" is the most recent, one minute old. "failure 10" is the oldest and must not be shown.
	if !strings.Contains(got[0].Detail, "failure 1 ") && !strings.Contains(got[0].Detail, `failure 1"`) {
		t.Errorf("detail does not quote the newest entry: %q", got[0].Detail)
	}
	if strings.Contains(got[0].Detail, "failure 10") {
		t.Errorf("detail quotes the oldest entry rather than the newest: %q", got[0].Detail)
	}
}

// A clean log, or no log at all, is not a finding.
//
// A server that has just started may not have written one, and reporting that would be noise on a healthy
// installation, which is what trains a reader to ignore the command.
func TestCheckLogsQuietWhenClean(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	now := time.Now()

	if got := mgr.checkLogs(now); len(got) != 0 {
		t.Errorf("checkLogs() with no log = %+v, want nothing", got)
	}

	// Including a session whose name contains "error", which is why the check matches on the level attribute
	// rather than on the word appearing anywhere in the line.
	writeLog(t, dirs.ServerLog(),
		logAt(now, "INFO", "created session")+" session=error-repro",
		logAt(now, "INFO", "an error occurred")+" note=this-is-info-not-error",
	)
	if got := mgr.checkLogs(now); len(got) != 0 {
		t.Errorf("checkLogs() with only INFO = %+v, want nothing", got)
	}
}

// An entry whose timestamp cannot be parsed is still reported.
//
// Reported rather than dropped, because the level is the part that says it matters. Silently discarding a real
// error over a malformed timestamp is the worse failure: it is the same as not having the check.
func TestCheckLogsKeepsEntriesWithUnparseableTimestamps(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	now := time.Now()

	writeLog(t, dirs.ServerLog(),
		`level=ERROR msg="no timestamp at all"`,
		`time=not-a-timestamp level=ERROR msg="unparseable timestamp"`,
	)

	got := mgr.checkLogs(now)
	if len(got) != 1 {
		t.Fatalf("checkLogs() = %+v, want the undated errors reported", got)
	}
	if !strings.Contains(got[0].Detail, "2 errors") {
		t.Errorf("detail = %q, want both undated errors counted", got[0].Detail)
	}
}

// The scanner looks in the directories the path helpers actually name.
//
// The failure mode here is silence: a scanner pointed at the wrong directory finds nothing and reports a clean
// installation, which is worse than an error. That is not hypothetical -- the previous version read the
// directory containing the server's log and matched shim logs by filename prefix, so splitting the logs into
// per-type subdirectories broke it without breaking any test that existed at the time.
//
// Asserted against the helpers rather than against literal paths, so the two cannot drift.
func TestLogFilesCoversEveryLogDirectory(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	now := time.Now()

	// One log of each kind, with a distinguishable error in it.
	writeLog(t, dirs.ServerLog(), logAt(now, "ERROR", "from the server"))
	writeLog(t, dirs.ClientLog(), logAt(now, "ERROR", "from a client"))
	writeLog(t, dirs.ShimLog("work"), logAt(now, "ERROR", "from a shim"))

	got := mgr.checkLogs(now)
	if len(got) != 1 {
		t.Fatalf("checkLogs() = %+v, want one finding", got)
	}
	// All three sources, or a whole class of log is invisible.
	for _, want := range []string{"from the server", "from a client", "from a shim"} {
		if !strings.Contains(got[0].Detail, want) {
			t.Errorf("detail does not include %q, so that log directory is not being scanned:\n%s",
				want, got[0].Detail)
		}
	}
	// Counted once each, not twice: overlapping directories would double every entry.
	if !strings.Contains(got[0].Detail, "3 errors") {
		t.Errorf("detail = %q, want exactly 3 errors", got[0].Detail)
	}
}

// Session output is not scanned as a diagnostic log.
//
// It lives directly in logs/ while diagnostics live in subdirectories, and it holds whatever the shell printed.
// A build that prints the word ERROR would otherwise be reported as a cm fault.
func TestLogFilesIgnoresSessionOutput(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	now := time.Now()

	// Output that looks exactly like a log line, which is the case that would fool a scanner reading logs/
	// indiscriminately.
	writeLog(t, dirs.SessionLog("work"), logAt(now, "ERROR", "printed by the shell, not by cm"))

	if got := mgr.checkLogs(now); len(got) != 0 {
		t.Errorf("checkLogs() = %+v, want nothing: session output is not a diagnostic log", got)
	}
}
