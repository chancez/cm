package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// statusResult is the JSON shape `cm status` prints.
type statusResult struct {
	Running         bool   `json:"running"`
	PID             int32  `json:"pid"`
	Version         string `json:"version"`
	UptimeSec       int64  `json:"uptime_seconds"`
	RuntimeDir      string `json:"runtime_dir"`
	StateDir        string `json:"state_dir"`
	Terminal        bool   `json:"terminal"`
	SessionsRunning int32  `json:"sessions_running"`
	SessionsExited  int32  `json:"sessions_exited"`
	SessionsDead    int32  `json:"sessions_dead"`
	Clients         int32  `json:"clients"`
	ClientVersion   string `json:"client_version"`
}

func (e *env) cmStatus() statusResult {
	e.t.Helper()

	out := e.mustRun("status", "--json")
	var s statusResult
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		e.t.Fatalf("parsing status output %q: %v", out, err)
	}
	return s
}

// `cm status` reports the running server's pid, directories, and session counts.
//
// The directories matter most in practice: a client and a server started with different ones is a confusing
// state where commands appear to work while showing no sessions, and this is where the pair becomes visible.
func TestStatusReportsTheRunningServer(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	got := e.cmStatus()
	if !got.Running {
		t.Fatal("running = false, want true: newEnv started a server")
	}
	if got.PID <= 0 {
		t.Errorf("pid = %d, want the server's process id", got.PID)
	}
	// The pid must name a live process, or the report is worse than nothing.
	if err := processAlive(int(got.PID)); err != nil {
		t.Errorf("pid %d is not alive: %v", got.PID, err)
	}
	// And it must be the server, not merely some live process. Asserted because "is it alive" passes for
	// any pid the test can signal: an earlier mutation returning the parent pid was caught only because pid
	// 1 happened to be unsignalable, which is luck rather than a test.
	if !processIsServer(t, int(got.PID), e) {
		t.Errorf("pid %d is not this environment's server", got.PID)
	}
	if got.RuntimeDir != e.runtime {
		t.Errorf("runtime_dir = %q, want %q", got.RuntimeDir, e.runtime)
	}
	if got.StateDir != e.state {
		t.Errorf("state_dir = %q, want %q", got.StateDir, e.state)
	}
	// Same binary on both sides here, so the versions agree.
	if got.Version != got.ClientVersion {
		t.Errorf("server %q and client %q differ for one binary", got.Version, got.ClientVersion)
	}
}

// Session counts reflect what cm list shows, including finished sessions.
//
// Counted from the store rather than the live registry, because the registry holds only what this server is
// proxying: a record left by an earlier server still appears in `cm list` and has to be counted here too, or
// the two commands disagree.
func TestStatusCountsSessions(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "alive", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.mustRun("run", "--session", "done", "-d", "--", trueBinary())
	e.waitFor("both sessions to settle", 15*time.Second, func() bool {
		s, ok := e.session("alive")
		if !ok || s.State != "running" {
			return false
		}
		d, ok := e.session("done")
		return ok && d.State != "running"
	})

	got := e.cmStatus()
	if got.SessionsRunning != 1 {
		t.Errorf("sessions_running = %d, want 1", got.SessionsRunning)
	}
	// The finished one is counted somewhere rather than dropped, so a total matches `cm list`.
	if got.SessionsExited+got.SessionsDead != 1 {
		t.Errorf("exited=%d dead=%d, want one finished session counted",
			got.SessionsExited, got.SessionsDead)
	}
}

// With no server running, status says so rather than starting one.
//
// Same reasoning as `cm version`: starting a server so a report can describe it would change what is being
// reported, and "not running" is the answer right after `cm server stop`.
func TestStatusDoesNotStartAServer(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("server", "stop")
	e.waitServerGone()

	got := e.cmStatus()
	if got.Running {
		t.Errorf("running = true after stopping the server; status = %+v", got)
	}
	// The client's own version is still reported, since that needs no server.
	if got.ClientVersion == "" {
		t.Error("client_version is empty")
	}
	if e.serverIsRunning() {
		t.Error("`cm status` started a server")
	}
}

// Attached clients are counted.
//
// From the live registry, since only the running server knows who is attached: the store records sessions, not
// connections. This is the field that answers "is anything watching this".
func TestStatusCountsAttachedClients(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	if got := e.cmStatus(); got.Clients != 0 {
		t.Fatalf("clients = %d before attaching, want 0", got.Clients)
	}

	c := attachOnPty(t, e, "watched", "--", "/bin/sh")
	c.waitReady()

	e.waitFor("the client to be counted", 15*time.Second, func() bool {
		return e.cmStatus().Clients == 1
	})
}

// `cm server restart` replaces the server and keeps sessions running.
//
// The upgrade path as one command. No process hand-off is involved and none is needed: the shim owns the pty,
// so the shell is untouched, and a stop-then-start takes a few tens of milliseconds. What the command adds over
// two commands is waiting for the old server to release the socket, since Listen refuses while something still
// answers there.
func TestServerRestartKeepsSessions(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "survivor", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("survivor")
		return ok && s.State == "running"
	})
	before, _ := e.session("survivor")
	beforePID := e.cmStatus().PID

	e.mustRun("server", "restart")

	// A server is running afterwards, and a different one.
	after := e.cmStatus()
	if !after.Running {
		t.Fatal("no server running after a restart")
	}
	if after.PID == beforePID {
		t.Errorf("server pid is still %d, want a new process", beforePID)
	}

	// And the session came through with the same shell, which is what makes this an upgrade rather than a
	// restart that drops everything.
	got, ok := e.session("survivor")
	if !ok {
		t.Fatal("the session was not adopted by the new server")
	}
	if got.ShellPID != before.ShellPID {
		t.Errorf("shell pid changed: %d -> %d, want the same process", before.ShellPID, got.ShellPID)
	}
	if got.State != "running" {
		t.Errorf("state = %q after the restart, want running", got.State)
	}
}

// Restarting with no server running starts one.
//
// The caller wants a server running afterwards either way, so refusing would make this useless as the one
// command to reach for after a stop.
func TestServerRestartStartsWhenNoneRunning(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("server", "stop")
	e.waitServerGone()

	e.mustRun("server", "restart")
	if !e.cmStatus().Running {
		t.Error("no server running after restarting with none up")
	}
}

// `cm logs --clear` empties the log, and the live server keeps logging afterwards.
//
// The second half is the point. The server holds its log open for the life of the process, so unlinking the
// file would leave it writing to a deleted inode: its output would vanish with nothing to report it. Truncating
// keeps the same file, and O_APPEND recomputes the offset so no NUL padding is left behind. Both verified
// rather than assumed.
func TestLogsClearEmptiesTheLogAndLoggingContinues(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// Something in the log to clear.
	//
	// Waiting for the session's *end* rather than for any content at all, because the log is truncated
	// next and the two race otherwise. The command here exits immediately, but the server logs "session
	// ended" from the pump that notices it, so a wait satisfied by the earlier "session started" line
	// leaves that write still in flight and it lands after the clear. Seen on Linux as a log holding one
	// line, `session ended state=exited exit_code=0`, where the test wanted it empty. Nothing else is
	// written about a session after that line, so once it is there the clear has nothing to race.
	e.mustRun("run", "--session", "noisy", "-d", "--", trueBinary())
	e.waitFor("the session to be logged as ended", 15*time.Second, func() bool {
		return strings.Contains(e.readFileOrEmpty(e.serverLogPath()), `msg="session ended"`)
	})

	e.mustRun("logs", "server", "--clear")
	if body := e.readFileOrEmpty(e.serverLogPath()); len(body) != 0 {
		t.Errorf("log is %d bytes after --clear, want empty:\n%s", len(body), body)
	}

	// The server, which still holds the file open, must keep writing into it.
	e.mustRun("run", "--session", "after", "-d", "--", trueBinary())
	e.waitFor("the server to log again after the clear", 15*time.Second, func() bool {
		return len(e.readFileOrEmpty(e.serverLogPath())) > 0
	})

	// And what it wrote is readable rather than padded with NULs from the old offset.
	body := e.readFileOrEmpty(e.serverLogPath())
	if !strings.HasPrefix(body, "time=") {
		t.Errorf("log does not start with a log line after the clear, got %q",
			body[:min(len(body), 40)])
	}
	if strings.ContainsRune(body, 0) {
		t.Error("log contains NUL bytes, so the writer kept its old offset")
	}
}

// Clearing a log that does not exist is not an error.
//
// `cm logs --clear` on a fresh installation, or for a session that never logged, has nothing to do, and asking
// for that is not a mistake.
func TestLogsClearToleratesAMissingLog(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// A session name that never existed, so its shim log was never created. sessionNameArg validates the name
	// but does not require the session to exist, which is what makes clearing a missing log reachable.
	if r := e.run("logs", "shim", "neverexisted", "--clear"); r.code != 0 {
		t.Errorf("exit code = %d clearing a missing log, want 0\nstderr: %s", r.code, r.stderr)
	}
}

// `cm logs --clear --all` removes the rotated generation too.
//
// The rotated file is removed rather than truncated, since nothing holds it open: it exists only as a previous
// generation, and an empty one left behind is noise that `cm logs --all` would then print. Covered because the
// flag combination is a separate branch, and a mutation that skipped the removal passed every other test here.
func TestLogsClearAllRemovesTheRotatedLog(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// A rotated generation, written directly: making the server produce one needs 4 MiB of log, which is a
	// slow way to test a file being removed.
	rotated := e.serverLogPath() + ".1"
	e.appendLog(rotated, "time=2026-01-01T00:00:00Z level=ERROR msg=\"old generation\"")
	if e.readFileOrEmpty(rotated) == "" {
		t.Fatal("the rotated log was not created, so this test would assert nothing")
	}

	// Without --all it is left alone, since the default is to clear the current log only.
	e.mustRun("logs", "server", "--clear")
	if e.readFileOrEmpty(rotated) == "" {
		t.Error("`logs --clear` removed the rotated log, want it left alone without --all")
	}

	e.mustRun("logs", "server", "--clear", "--all")
	if body := e.readFileOrEmpty(rotated); body != "" {
		t.Errorf("the rotated log survived `--clear --all`: %q", body)
	}
}
