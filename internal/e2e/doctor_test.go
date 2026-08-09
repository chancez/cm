package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// doctorFinding is one finding in the JSON report.
//
// Named rather than anonymous so ofKind can return it and a test can hold one in a variable. It was written
// inline three times before, which is the sort of duplication that makes adding a field a three-place edit.
type doctorFinding struct {
	Kind     string `json:"kind"`
	Session  string `json:"session"`
	ShimPID  int    `json:"shim_pid"`
	ShellPID int    `json:"shell_pid"`
	Detail   string `json:"detail"`
	Fixable  bool   `json:"fixable"`
}

// doctorResult is the JSON shape cm doctor prints.
type doctorResult struct {
	ClientVersion string          `json:"client_version"`
	ServerVersion string          `json:"server_version"`
	Findings      []doctorFinding `json:"findings"`
	Repaired      []string        `json:"repaired"`
}

// doctor runs the command and parses its report.
func (e *env) doctor(args ...string) (doctorResult, int) {
	e.t.Helper()

	r := e.run(append([]string{"doctor", "--json"}, args...)...)
	var out doctorResult
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		e.t.Fatalf("parsing doctor output %q: %v", r.stdout, err)
	}
	return out, r.code
}

// ofKind returns the findings of one kind.
func (d doctorResult) ofKind(kind string) []doctorFinding {
	var out []doctorFinding
	for _, f := range d.Findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}

// kinds lists every finding kind reported, for an assertion message that says what was actually found.
//
// Worth having because the useful failure output is the whole set: "want version-skew, got [server-errors
// loose-dir-perms]" points at the problem, while "want version-skew, got nothing" does not say whether the
// command ran.
func (d doctorResult) kinds() []string {
	out := make([]string, 0, len(d.Findings))
	for _, f := range d.Findings {
		out = append(out, f.Kind)
	}
	return out
}

// doctor reports both versions and does not invent problems on a working installation.
//
// The version line is printed on every run, including a clean one, because the first question about any report
// is which builds produced it. Asserted here rather than only in a unit test because it crosses the wire: the
// client sends its own version and the server answers with its.
func TestDoctorReportsVersionsAndAHealthySetup(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	got, _ := e.doctor()
	if got.ClientVersion == "" || got.ServerVersion == "" {
		t.Errorf("doctor = %+v, want both versions reported", got)
	}
	// Same binary on both sides here, so there is nothing to warn about.
	if got.ClientVersion != got.ServerVersion {
		t.Errorf("versions differ (%q vs %q) for one binary, want them equal",
			got.ClientVersion, got.ServerVersion)
	}
	if len(got.ofKind("version-skew")) != 0 {
		t.Errorf("doctor reported version skew talking to itself: %+v", got.Findings)
	}
	// Only findings about the installation's own state are asserted, not the exit code.
	//
	// Several checks are legitimately true of a test environment and have nothing to do with what this test
	// is about: a deep temp directory can report long-socket-path, and a session whose shell emits no OSC 133
	// reports no-shell-integration.
	// Asserting a zero exit made this fail on the Linux image for a correct reason, which is a test asserting
	// its environment rather than the code.
	environmental := map[string]bool{
		"long-socket-path":     true,
		"no-shell-integration": true,
	}
	for _, f := range got.Findings {
		if !environmental[f.Kind] {
			t.Errorf("doctor reported %q on a fresh installation: %s", f.Kind, f.Detail)
		}
	}
}

// An orphaned shim is found, and --clean stops it while leaving a healthy session alone.
//
// The whole point of the command. An orphan holds a pty and a shell nothing can reattach to, and it is absent
// from `cm list` by definition, so nothing else would ever show it.
func TestDoctorFindsAndCleansAnOrphan(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "keeper", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.mustRun("run", "--session", "orphan", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("both sessions to be running", 15*time.Second, func() bool {
		a, aok := e.session("keeper")
		b, bok := e.session("orphan")
		return aok && bok && a.State == "running" && b.State == "running"
	})

	// An orphan needs the shim alive and the record gone. `kill --force` does not produce one, since it
	// shuts the shim down too, so the record is removed behind the server's back: stop the server, delete
	// the row, and let the next command start a server that has never heard of the session.
	e.mustRun("server", "stop")
	e.waitServerGone()
	e.deleteSessionRecord("orphan")

	got, code := e.doctor()
	orphans := got.ofKind("orphan-shim")
	if len(orphans) != 1 {
		t.Fatalf("doctor = %+v, want one orphan-shim", got.Findings)
	}
	if orphans[0].Session != "orphan" {
		t.Errorf("orphan session = %q, want %q", orphans[0].Session, "orphan")
	}
	// The pids are what let a reader confirm what is being reported before acting on it.
	if orphans[0].ShimPID == 0 {
		t.Errorf("finding = %+v, want the shim pid reported", orphans[0])
	}
	if code == 0 {
		t.Error("doctor exited 0 with an orphan present, want non-zero so it can gate a script")
	}

	cleaned, _ := e.doctor("--clean")
	if len(cleaned.Repaired) != 1 {
		t.Errorf("doctor --clean repaired %v, want exactly the orphan", cleaned.Repaired)
	}
	// The exit status is not asserted here. The healthy session still trips no-shell-integration, since
	// /bin/sh reports no OSC 133, so a non-zero exit is correct: an unfixable finding stands. What matters
	// is that the orphan was repaired, which the check below confirms.

	// Nothing left to find, and the healthy session survived, which is the property that makes --clean safe
	// to run on a live machine.
	after, _ := e.doctor()
	if len(after.ofKind("orphan-shim")) != 0 {
		t.Errorf("an orphan survived --clean: %+v", after.Findings)
	}
	if s, ok := e.session("keeper"); !ok || s.State != "running" {
		t.Errorf("healthy session = %+v (found=%v), want it untouched by --clean", s, ok)
	}
}

// doctor surfaces errors and warnings the server and its shims logged, and only recent ones.
//
// An e2e test because the reading crosses a process boundary: one process appends to the log and another parses
// it, and the format they agree on is slog's text output, which nothing in the unit tests actually produces.
// The unit tests build log lines by hand, so a change to how cm logs would leave them passing.
//
// The entries are written directly rather than provoked. An earlier version of this test ran a command it hoped
// would log an error and then looped over the findings asserting things only if any existed, which meant it
// passed whether or not anything was found. Writing known lines makes the assertion unconditional.
func TestDoctorSurfacesLoggedErrorsAndWarnings(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// A real session, so a shim log exists at the path the scan derives, rather than a file invented by the
	// test at a path nothing else would use.
	e.mustRun("run", "--session", "logged", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("logged")
		return ok && s.State == "running"
	})

	now := time.Now()
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }

	// Appended, not overwritten: the server is running and writing to this file, and truncating it under a live
	// server would be a different test with a worse failure mode.
	e.appendLog(e.serverLogPath(),
		"time="+stamp(-time.Minute)+` level=ERROR msg="a recent error"`,
		"time="+stamp(-2*time.Minute)+` level=WARN msg="a recent warning"`,
		// Outside the 24-hour window, so this one must not be reported however loud it is.
		"time="+stamp(-48*time.Hour)+` level=ERROR msg="an ancient error"`,
	)
	// A shim log, which the check previously never opened.
	e.appendLog(e.shimLogPath("logged"),
		"time="+stamp(-3*time.Minute)+` level=ERROR msg="a shim error"`)

	got, code := e.doctor()

	errs := got.ofKind("server-errors")
	if len(errs) != 1 {
		t.Fatalf("findings = %v, want one server-errors finding", got.kinds())
	}
	warns := got.ofKind("log-warnings")
	if len(warns) != 1 {
		t.Fatalf("findings = %v, want one log-warnings finding", got.kinds())
	}

	// The recent error from each file, and the warning, each in the right finding.
	if !strings.Contains(errs[0].Detail, "a recent error") {
		t.Errorf("server-errors does not include the server's error: %q", errs[0].Detail)
	}
	if !strings.Contains(errs[0].Detail, "a shim error") {
		t.Errorf("server-errors does not include the shim's error: %q", errs[0].Detail)
	}
	if !strings.Contains(warns[0].Detail, "a recent warning") {
		t.Errorf("log-warnings does not include the warning: %q", warns[0].Detail)
	}
	// The old entry is not reported, which is the whole reason for the window.
	if strings.Contains(errs[0].Detail, "an ancient error") {
		t.Errorf("an entry from 48 hours ago was reported: %q", errs[0].Detail)
	}
	// Nor does the warning leak into the errors, or the reverse.
	if strings.Contains(errs[0].Detail, "a recent warning") {
		t.Errorf("the warning was reported as an error: %q", errs[0].Detail)
	}
	if strings.Contains(warns[0].Detail, "a recent error") {
		t.Errorf("the error was reported as a warning: %q", warns[0].Detail)
	}
	// Neither is fixable: both record something that already happened, and deleting a log destroys the
	// evidence rather than fixing anything.
	for _, f := range append(errs, warns...) {
		if f.Fixable {
			t.Errorf("%s was reported as fixable", f.Kind)
		}
	}
	if code == 0 {
		t.Error("exit code = 0 with findings, want non-zero")
	}
}

// A session whose shell sends no OSC 133 is reported, and one that does is not.
//
// Without those markers cm cannot tell busy from idle, so `cm wait --until idle` returns immediately and
// `cm list` never shows what is running. Nothing errors, which is why it needs a check.
func TestDoctorReportsMissingShellIntegration(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// /bin/sh with no integration loaded, which is the case being detected.
	e.mustRun("run", "--session", "silent", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("silent")
		return ok && s.State == "running"
	})

	got, _ := e.doctor()
	found := got.ofKind("no-shell-integration")
	if len(found) != 1 {
		t.Fatalf("doctor = %+v, want one no-shell-integration", got.Findings)
	}
	if !strings.Contains(found[0].Detail, "silent") {
		t.Errorf("detail = %q, want it to name the session", found[0].Detail)
	}

	// A program reporting its own state answers the same question, so the finding goes away.
	e.mustRun("report", "silent", "--state", "busy", "--detail", "working")
	after, _ := e.doctor()
	if len(after.ofKind("no-shell-integration")) != 0 {
		t.Errorf("doctor = %+v, want nothing once the session reports its own state", after.Findings)
	}
}
