package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// A kill that leaves a process behind warns, rather than reporting plain success.
//
// The failure this closes was silent in the way that matters most: `cm kill` printed "killed NAME" and
// exited 0 while a process kept holding a pty. Those are capped at 511 system-wide on macOS, and the
// symptom of running out is an unrelated program failing to allocate a terminal, so nothing connected the
// two.
func TestKillWarnsAboutSurvivingProcesses(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	pid := e.startStubbornJob("leaky", 1401)

	r := e.run("kill", "leaky")
	if r.code != 0 {
		t.Fatalf("kill exited %d, want 0: a leak is a warning rather than a failed request: %s",
			r.code, r.stderr)
	}
	// Still reported as killed, since the session is gone from cm's view and its record deleted. The
	// leak is a separate fact about a process, not a failure of the request.
	if !strings.Contains(r.stdout, "killed leaky") {
		t.Errorf("stdout = %q, want the session reported killed", r.stdout)
	}

	// The warning names the pids, which is what makes it actionable: a warning that says only "something
	// survived" leaves the reader nothing to look at.
	if !strings.Contains(r.stderr, "warning") {
		t.Fatalf("stderr = %q, want a warning about the surviving process", r.stderr)
	}
	if !strings.Contains(r.stderr, "--signal") {
		t.Errorf("stderr = %q, want it to name the fix", r.stderr)
	}
	// And the process really did survive, so the warning is not spurious.
	if !jobAlive(pid) {
		t.Error("the job is gone, so the warning above was reporting a leak that did not happen")
	}
}

// A clean kill warns about nothing.
//
// The control for the test above. Without it, a warning printed unconditionally would pass that one and
// be useless, which is the failure mode of a diagnostic that cries wolf.
func TestKillDoesNotWarnWhenNothingSurvives(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("run", "--session", "tidy", "-d", "--", "/bin/sh", "-c", "sleep 300")

	r := e.run("kill", "tidy")
	if r.code != 0 {
		t.Fatalf("kill exited %d, want 0: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, "warning") {
		t.Errorf("stderr = %q, want no warning for a session that stopped cleanly", r.stderr)
	}
}

// Escalating past the default reaps the job, so no warning is produced.
func TestKillWithSignalWarnsAboutNothing(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	pid := e.startStubbornJob("escalated", 1402)

	r := e.run("kill", "escalated", "--signal", "kill")
	if r.code != 0 {
		t.Fatalf("kill --signal kill exited %d, want 0: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, "warning") {
		t.Errorf("stderr = %q, want no warning when the signal worked", r.stderr)
	}
	if !waitForProcessToGo(pid, 15*time.Second) {
		t.Error("the job survived kill --signal kill")
	}
}

// The JSON output carries the surviving pids, so a script can act on a leak.
//
// In its own field rather than in errors: a caller that treats any survivor as a failure can check it,
// and one that does not is unaffected, which is why it does not change the exit status.
func TestKillJSONReportsSurvivingProcesses(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	pid := e.startStubbornJob("jleaky", 1403)

	out := e.mustRun("kill", "jleaky", "--json")

	var got struct {
		Killed    []string           `json:"killed"`
		Errors    map[string]string  `json:"errors"`
		Surviving map[string][]int32 `json:"surviving"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parsing kill output %q: %v", out, err)
	}

	if len(got.Killed) != 1 || got.Killed[0] != "jleaky" {
		t.Errorf("killed = %v, want [jleaky]", got.Killed)
	}
	// Not an error, since the request succeeded.
	if len(got.Errors) != 0 {
		t.Errorf("errors = %v, want none: a leak is not a failed kill", got.Errors)
	}
	pids, ok := got.Surviving["jleaky"]
	if !ok || len(pids) == 0 {
		t.Fatalf("surviving = %v, want the leaked pids", got.Surviving)
	}
	// The pid the test started must be among them, so the field describes this job rather than anything
	// that happened to be running.
	var found bool
	for _, p := range pids {
		if int(p) == pid {
			found = true
		}
	}
	if !found {
		t.Errorf("surviving = %v, want it to include the job's pid %d", pids, pid)
	}
}

// The leak reaches `cm doctor`, through the shim's own log.
//
// Nothing new was added to doctor for this. The shim logs the survivors, doctor already scans shim logs,
// and a warning there is exactly what that check exists to surface: something cm could not do and did not
// interrupt anyone about. Recorded as a test because the path is indirect enough to be broken by accident.
func TestDoctorReportsASurvivingProcess(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.startStubbornJob("dleaky", 1404)

	e.mustRun("kill", "dleaky")

	// Doctor exits non-zero on a finding, which is a report rather than a malfunction.
	r := e.run("doctor")
	if !strings.Contains(r.stdout, "survived") {
		t.Errorf("doctor output does not mention the surviving process:\n%s", r.stdout)
	}
	// The pids are in the finding, so someone reading it afterwards can check whether the process is
	// still there.
	if !strings.Contains(r.stdout, "surviving") {
		t.Errorf("doctor output does not name the surviving pids:\n%s", r.stdout)
	}
}

// A clean kill leaves doctor with nothing to report.
//
// This is a regression test for a pre-existing bug rather than a control. Kill deletes the session record
// and the pump then writes the session's outcome, so the two race by design and the delete usually wins.
// That was logged as an error, which made `cm doctor` report an error after every healthy `cm kill` --
// verified against a binary built before this change. A diagnostic that fires on correct behavior teaches
// people to ignore it, which costs more than the missing line.
func TestDoctorIsQuietAfterACleanKill(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("run", "--session", "tidy", "-d", "--", "/bin/sh", "-c", "sleep 300")

	e.mustRun("kill", "tidy")

	// Waited for rather than checked immediately. The shim exits a moment after replying, so doctor run
	// straight afterwards legitimately sees it as an orphan -- a real finding about a transient state,
	// and not the one under test here.
	var out string
	e.waitFor("doctor to report nothing", 15*time.Second, func() bool {
		r := e.run("doctor")
		out = r.stdout
		return r.code == 0
	})

	// The assertion is the absence of this specific error, which fired on every healthy kill.
	if strings.Contains(out, "recording session outcome failed") {
		t.Errorf("doctor reports an error after a clean kill:\n%s", out)
	}
}
