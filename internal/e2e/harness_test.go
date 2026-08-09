// Package e2e drives real cm processes: a real client talking to a real server that spawns real
// shims holding real ptys.
//
// The unit tests cover seams by construction, calling into a Manager or Service directly. That is the
// right level for most things and it is deliberately not this. Several bugs cm has shipped were
// invisible at that level because they lived *between* processes:
//
//   - A session adopted after a server restart came back with a blank screen. Nothing in the unit
//     tests starts a second server process against the same store, so nothing could see it.
//   - `cm list` filled with every command ever run, because expiry was gated on a config flag. Every
//     unit test sets the policy explicitly, so the default path was never exercised.
//   - A build without cgo could not create a session at all, since the failure was in wiring that
//     only exists in main.
//
// Each of those is a wiring or lifecycle fault rather than a logic fault, which is what this package
// is for. The cost is real: these tests spawn processes and wait on ptys, so they are slower and
// noisier than the rest of the suite. They are guarded by -short accordingly.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
)

// buildResult caches one build of cm for the whole package rather than per test.
type buildResult struct {
	sync.Once
	path string
	err  error
}

// buildOnce is the ordinary binary, which is what almost every test uses.
var buildOnce buildResult

// versionBuildOnce is the binary built with the version override enabled, used only by the tests that need
// two processes to disagree about their versions.
var versionBuildOnce buildResult

// cmBinary returns a path to a freshly built cm.
//
// Built rather than assumed present: a stale binary would silently test the previous commit, which is
// exactly the failure mode these tests exist to avoid.
func cmBinary(t *testing.T) string {
	t.Helper()
	return buildCM(t, &buildOnce, nil)
}

// cmVersionBinary returns a cm built with the test-only overrides enabled.
//
// A separate binary because the override is behind a build tag on purpose: a released cm ignores CM_VERSION
// entirely, so there is no way to fake a version in one. That is the right default and it means a test that
// needs two disagreeing versions has to ask for an instrumented build explicitly.
//
// The alternative was building from two git tags, which is not a test: it cannot run in CI without the tags
// present, it needs git operations to set up, and if the tags are missing it silently compares a version
// against itself and passes.
func cmVersionBinary(t *testing.T) string {
	t.Helper()
	return buildCM(t, &versionBuildOnce, []string{"-tags", paths.TestHooksBuildTag})
}

// buildCM builds cm once per variant and caches the path.
func buildCM(t *testing.T, once *buildResult, extraArgs []string) string {
	t.Helper()

	once.Do(func() {
		dir, err := os.MkdirTemp("", "cmbin")
		if err != nil {
			once.err = err
			return
		}
		path := filepath.Join(dir, "cm")
		args := append([]string{"build"}, extraArgs...)
		args = append(args, "-o", path, "./cmd/cm")
		cmd := exec.Command("go", args...)
		cmd.Dir = repoRoot()
		if out, cerr := cmd.CombinedOutput(); cerr != nil {
			once.err = &buildError{output: string(out), err: cerr}
			return
		}
		once.path = path
	})
	if once.err != nil {
		t.Fatalf("building cm: %v", once.err)
	}
	return once.path
}

type buildError struct {
	output string
	err    error
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + e.output }

// repoRoot returns the module root, since tests run in their own package directory.
func repoRoot() string {
	// internal/e2e -> internal -> root
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return filepath.Join(wd, "..", "..")
}

// env is an isolated cm installation: its own runtime dir, state dir, and config.
//
// Isolation is not optional here. These tests start and stop servers and kill sessions, and sharing a
// runtime directory with the developer's own cm would let a test destroy real work.
type env struct {
	t       *testing.T
	bin     string
	runtime string
	state   string
	config  string
	// session_ is exported as CM_SESSION when set, standing in for a command run from inside a session.
	session_ string
	// version is exported as CM_VERSION when set, which only an instrumented binary honors. Used by the
	// version-skew tests to make two processes disagree without building from two git tags.
	version string
	// extraEnv is appended to every invocation, for the other test-only overrides an instrumented binary
	// reads. Kept as raw KEY=VALUE strings rather than a map, since that is what exec wants and the order is
	// never interesting.
	extraEnv []string
}

// newEnv returns an isolated cm installation with no config file.
//
// No config on purpose: it is the default path, which is what a new user gets and what the expiry bug
// hid in. Tests that need settings call writeConfig.
func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, cmBinary(t), "")
}

// newEnvWith is newEnv with the binary and reported version chosen by the caller.
//
// Both have to be set before the first command rather than afterwards, because that command starts the
// server: a version applied later would reach only subsequent clients while the server went on reporting the
// real build, which is a test that passes for the wrong reason.
func newEnvWith(t *testing.T, bin, version string) *env {
	t.Helper()

	// os.MkdirTemp with a short prefix rather than t.TempDir(), which embeds the test name and blows
	// past the 104-byte sockaddr_un limit. That failure surfaces as a bare EINVAL.
	root, err := os.MkdirTemp("", "cme2e")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}

	e := &env{
		t:       t,
		bin:     bin,
		version: version,
		runtime: filepath.Join(root, "r"),
		state:   filepath.Join(root, "s"),
	}
	for _, d := range []string{e.runtime, e.state} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) error = %v", d, err)
		}
	}

	t.Cleanup(func() {
		// Stop every session before the server, or shims outlive the test holding ptys.
		//
		// This is not hypothetical housekeeping. A shim holds a pty for as long as it runs, macOS caps
		// them at 511 system-wide, and a suite that leaks one per session exhausts them: the failure
		// arrives as `pty.Start() error = device not configured` in whichever test happens to run once
		// the limit is reached, which looks like a bug in that test rather than a leak in the harness.
		//
		// mustRun, not run, and checked rather than ignored. This previously called `kill --all` before
		// that flag existed, so it failed silently on every teardown and leaked every session. 437 stray
		// ptys had accumulated by the time the exhaustion surfaced.
		if r := e.run("kill", "--all"); r.code != 0 {
			t.Errorf("cleanup: kill --all failed, which leaks a pty per session: %s", r.stderr)
		}
		e.run("server", "stop")
		e.waitServerGone()
		os.RemoveAll(root)
	})

	// Start the server before returning, rather than letting the first command start it.
	//
	// Every cm command auto-starts a server if none is running, which is a feature and a hazard here:
	// two commands issued close together each start one, and the loser exits after the winner has
	// bound the socket. A test that read `cm list` while a client was still starting saw an empty list
	// from the losing server and looked like a session that had vanished.
	e.mustRun("list")
	return e
}

// writeConfig installs a config file for this environment.
func (e *env) writeConfig(body string) {
	e.t.Helper()
	path := filepath.Join(e.state, "cm.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		e.t.Fatalf("WriteFile() error = %v", err)
	}
	e.config = path
}

// environ returns the process environment for a cm invocation.
func (e *env) environ() []string {
	out := append(os.Environ(),
		"CM_RUNTIME_DIR="+e.runtime,
		"CM_STATE_DIR="+e.state,
		// A predictable shell, so a session's own output does not depend on the developer's prompt.
		"SHELL=/bin/sh",
		// Pointed at this test's own directory rather than left inherited, so a change to config-path
		// precedence cannot quietly reintroduce reading the developer's real file.
		"XDG_CONFIG_HOME="+filepath.Join(e.state, "xdg-config"),
	)
	if e.session_ != "" {
		out = append(out, "CM_SESSION="+e.session_)
	}
	if e.version != "" {
		out = append(out, paths.Env(paths.VersionEnvSuffix)+"="+e.version)
	}
	out = append(out, e.extraEnv...)
	if e.config != "" {
		out = append(out, "CM_CONFIG="+e.config)
	} else {
		// A path inside this test's own directory rather than an empty value.
		//
		// Empty means unset, so cm falls through to XDG_CONFIG_HOME or the platform config directory and
		// reads the developer's real file. That is not hypothetical: with detach_key set in ~/.config/cm,
		// a test that had written no config saw ctrl-o, so every e2e test was running against whatever
		// happened to be on the machine. It looked isolated only because the fallback path did not exist
		// until XDG support was added.
		//
		// Naming a file that does not exist is both isolated and the default path a new user is on, since a
		// missing config is not an error.
		out = append(out, "CM_CONFIG="+filepath.Join(e.state, "absent-cm.toml"))
	}
	return out
}

// result is the outcome of one cm invocation.
type result struct {
	stdout string
	stderr string
	code   int
}

// run invokes cm and returns its output and exit status.
//
// A non-zero status is not a test failure: several tests assert on exit codes, since propagating a
// command's status is a feature.
func (e *env) run(args ...string) result {
	e.t.Helper()
	return e.runWithin(20*time.Second, args...)
}

// runInSession invokes cm with CM_SESSION set, as a hook running inside a session would see it.
//
// cm exports that variable into every session's shell, so a hook needs no argument to say which session it
// is in. Reproducing that here means testing the path a hook actually takes rather than one it does not.
func (e *env) runInSession(session string, args ...string) result {
	e.t.Helper()

	prev := e.session_
	e.session_ = session
	defer func() { e.session_ = prev }()
	return e.runWithin(20*time.Second, args...)
}

func (e *env) runWithin(timeout time.Duration, args ...string) result {
	e.t.Helper()

	cmd := exec.Command(e.bin, args...)
	cmd.Env = e.environ()
	cmd.Dir = e.state
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	// /dev/null rather than inherited stdin. A client with a tty behaves differently, and inheriting
	// the test runner's stdin would make behavior depend on how the tests were invoked.
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		e.t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull

	if err := cmd.Start(); err != nil {
		e.t.Fatalf("starting cm %v: %v", args, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		code := 0
		var ee *exec.ExitError
		if err != nil {
			if asExit(err, &ee) {
				code = ee.ExitCode()
			} else {
				e.t.Fatalf("cm %v: %v", args, err)
			}
		}
		return result{stdout: out.String(), stderr: errBuf.String(), code: code}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		e.t.Fatalf("cm %v timed out after %s\nstdout: %s\nstderr: %s",
			args, timeout, out.String(), errBuf.String())
		return result{}
	}
}

// followFor runs a command that streams until interrupted, stopping it after d.
//
// A timeout is fatal in runWithin, which is right for a command expected to finish: a follower that fails to
// stop is a bug, and one such bug ran for 504 seconds before it was noticed. It is wrong for a follower of a
// live session, where the interruption is the point -- that is how a watcher normally stops, and the exit is
// what the test is about.
func (e *env) followFor(d time.Duration, args ...string) result {
	e.t.Helper()

	cmd := exec.Command(e.bin, args...)
	cmd.Env = e.environ()
	cmd.Dir = e.state
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		e.t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	cmd.Stdin = devNull

	if err := cmd.Start(); err != nil {
		e.t.Fatalf("starting cm %v: %v", args, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
		// Ended on its own, which happens when the session finishes first.
	case <-time.After(d):
		// Interrupted, as a user pressing ctrl-c would. SIGINT rather than Kill so the client takes its
		// normal exit path, which is what detaches and is the whole point of following it here.
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			e.t.Fatalf("cm %v did not exit within 5s of SIGINT", args)
		}
	}
	return result{stdout: out.String(), stderr: errBuf.String()}
}

// asExit is errors.As specialized to *exec.ExitError, kept separate to keep run readable.
func asExit(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// mustRunWithin is mustRun with a deadline, for commands that stream until something ends.
//
// A bound rather than the default, because a follower that fails to stop hangs rather than failing: one such
// bug ran for 504 seconds before it was noticed. A timeout turns that into a test failure with output.
func (e *env) mustRunWithin(timeout time.Duration, args ...string) string {
	e.t.Helper()

	r := e.runWithin(timeout, args...)
	if r.code != 0 {
		e.t.Fatalf("cm %v exited %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// waitForOutputInSession blocks until a session's output contains want.
//
// Read through `cm read` rather than by attaching, so this does not add a client that the test would then have
// to account for in a clients count.
func (e *env) waitForOutputInSession(session, want string, timeout time.Duration) {
	e.t.Helper()

	e.waitFor("output "+want+" in "+session, timeout, func() bool {
		return strings.Contains(e.run("read", session).stdout, want)
	})
}

// mustRun invokes cm and fails the test unless it succeeds.
func (e *env) mustRun(args ...string) string {
	e.t.Helper()
	r := e.run(args...)
	if r.code != 0 {
		e.t.Fatalf("cm %v exited %d\nstdout: %s\nstderr: %s", args, r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

// sessionDetail reads one session through `cm info --json`.
//
// Distinct from session(), which reads the list: info is the command a person runs about one session, and the
// fields have to be present on both paths rather than only the one a test happens to use.
func (e *env) sessionDetail(t *testing.T, name string) sessionJSON {
	t.Helper()

	out := e.mustRun("info", name, "--json")
	var s sessionJSON
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("parsing info output %q: %v", out, err)
	}
	return s
}

// sessionJSON is the subset of `cm list --json` these tests read.
type sessionJSON struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	ShellPID int    `json:"shell_pid"`
	Clients  int    `json:"clients"`
	ExitCode int    `json:"exit_code"`
	// Busy and Command are what the shell reported via OSC 133.
	Busy    bool   `json:"busy"`
	Command string `json:"command"`
	// LastCommandExitCode is the status of the last command the shell finished, and CommandFinished whether
	// there was one. Separate from ExitCode, which is the session's own.
	LastCommandExitCode int  `json:"last_command_exit_code"`
	CommandFinished     bool `json:"command_finished"`
	// ReportedState and ReportedDetail are what a program in the session reported about itself.
	ReportedState  string `json:"reported_state"`
	ReportedDetail string `json:"reported_detail"`
}

// list returns the sessions cm reports.
//
// Parsed from JSON rather than scraped from the table, so a formatting change does not look like a
// behavior change.
func (e *env) list() []sessionJSON {
	e.t.Helper()
	out := e.mustRun("list", "--json")
	var sessions []sessionJSON
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		e.t.Fatalf("parsing list output %q: %v", out, err)
	}
	return sessions
}

// session returns one session by name, reporting whether cm knows it.
func (e *env) session(name string) (sessionJSON, bool) {
	e.t.Helper()
	for _, s := range e.list() {
		if s.Name == name {
			return s, true
		}
	}
	return sessionJSON{}, false
}

// waitServerGone blocks until no server is listening.
func (e *env) waitServerGone() {
	sock := filepath.Join(e.runtime, "server.sock")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// serverLogPath is where the server writes its diagnostic log.
//
// In the state directory rather than the runtime one, which is load-bearing for the stranded-server test: the
// log has to survive the deletion of the runtime directory, since it is the only channel a server that can no
// longer be reached has left.
func (e *env) serverLogPath() string {
	return filepath.Join(e.state, "logs", "server", "server.log")
}

// clientLogPath is the shared client diagnostic log.
func (e *env) clientLogPath() string {
	return filepath.Join(e.state, "logs", "client", "client.log")
}

// shimLogPath is where a session's shim writes its diagnostic log.
//
// Separate from the session's output log: this records what the shim did, that records what the shell printed.
func (e *env) shimLogPath(session string) string {
	return filepath.Join(e.state, "logs", "shim", session+".log")
}

// appendLog adds lines to a log file.
//
// Appends rather than writes, because the server is running and writing to the same file: truncating it under a
// live server would test something else, with a worse failure mode than the test is about.
func (e *env) appendLog(path string, lines ...string) {
	e.t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatalf("MkdirAll() error = %v", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		e.t.Fatalf("OpenFile(%s) error = %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		e.t.Fatalf("writing to %s: %v", path, err)
	}
}

// readFileOrEmpty returns a file's contents, or "" if it is not there.
//
// Absence is not an error here: callers poll for a log to appear or to be emptied, and a missing file is a
// legitimate state in both directions.
func (e *env) readFileOrEmpty(path string) string {
	body, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(body)
}

// processIsServer reports whether a pid is this environment's cm server.
//
// Matched on the command line, which has to name both this test's binary and its runtime directory. Needed
// because "the pid is alive" is satisfied by any process the test can signal, so a report of the wrong pid
// would pass: what makes this specific is that no other process carries this temp path.
func processIsServer(t *testing.T, pid int, e *env) bool {
	t.Helper()

	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Logf("inspecting pid %d failed: %v", pid, err)
		return false
	}
	line := string(out)
	return strings.Contains(line, e.bin) && strings.Contains(line, e.runtime) &&
		strings.Contains(line, "server")
}

// processAlive reports whether a pid names a live process.
//
// Signal 0 rather than reading the process table, since it asks the kernel the question directly and does not
// depend on parsing ps output.
func processAlive(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.Signal(0))
}

// killStrandedServers stops any cm process still running against this environment's directories.
//
// Needed because a stranded server cannot be reached by name, so the ordinary teardown, which asks politely
// over the socket, cannot stop it. Signalling is the only channel left, and this is the one place that is the
// right answer rather than a shortcut.
//
// Matched on this environment's own runtime directory, which is a fresh temp path per test, so it cannot touch
// anything but processes this test started.
func killStrandedServers(t *testing.T, e *env) {
	t.Helper()

	out, err := exec.Command("ps", "ax", "-o", "pid=,command=").Output()
	if err != nil {
		t.Logf("listing processes to clean up stranded servers failed: %v", err)
		return
	}
	for line := range strings.Lines(string(out)) {
		// Both the binary and the runtime directory have to match, so a process merely mentioning the path
		// is not a candidate.
		if !strings.Contains(line, e.bin) || !strings.Contains(line, e.runtime) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pid, cerr := strconv.Atoi(fields[0])
		if cerr != nil {
			continue
		}
		// TERM, not KILL, so shims run their shutdown path and close their ptys rather than orphaning shells.
		if p, ferr := os.FindProcess(pid); ferr == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
}

// serverIsRunning reports whether a server is listening.
//
// Checked by the socket rather than by running a command, since every command starts one if none is running,
// which would make the check create what it is looking for.
func (e *env) serverIsRunning() bool {
	sock := filepath.Join(e.runtime, "server.sock")
	if _, err := os.Stat(sock); err != nil {
		return false
	}
	// A socket file can outlive its server, so dial it: the file's existence alone would report a stale socket
	// as a running server.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// restartServer stops the running server and waits for it to go.
//
// The next cm command starts a fresh one, which is how an upgrade works and the scenario that hid the
// blank-screen adoption bug.
func (e *env) restartServer() {
	e.t.Helper()
	e.mustRun("server", "stop")
	e.waitServerGone()
}

// ageAllRecords backdates every session record by d.
//
// Expiry is measured in minutes to days, so a test cannot wait it out. Written through the store's own
// SetUpdatedAt, which exists for exactly this, rather than by reaching into sqlite from here: the
// column name stays in one place and the test cannot drift from the schema.
//
// Requires no server to be running, since sqlite is opened directly.
func (e *env) ageAllRecords(d time.Duration) {
	e.t.Helper()

	e.mustRun("server", "stop")
	e.waitServerGone()

	// Through paths.Dirs rather than a hardcoded filename, so this cannot drift from where the server
	// actually keeps the database.
	dirs := paths.Dirs{Runtime: e.runtime, State: e.state}
	st, err := store.Open(context.Background(), dirs.Database())
	if err != nil {
		e.t.Fatalf("store.Open() error = %v", err)
	}
	defer st.Close()

	sessions, err := st.List(context.Background(), "")
	if err != nil {
		e.t.Fatalf("List() error = %v", err)
	}
	for _, rec := range sessions {
		if err := st.SetUpdatedAt(context.Background(), rec.Name, rec.UpdatedAt.Add(-d)); err != nil {
			e.t.Fatalf("SetUpdatedAt(%s) error = %v", rec.Name, err)
		}
	}
}

// deleteSessionRecord removes a session's row while leaving its shim running.
//
// The only way to produce a real orphan: every command that removes a session also stops its shim, which is
// correct behavior and means an orphan cannot be created through the CLI. This reproduces what a crash between
// spawning a shim and recording it, or a deleted state directory, leaves behind.
//
// Requires no server to be running, since sqlite is opened directly.
func (e *env) deleteSessionRecord(name string) {
	e.t.Helper()

	dirs := paths.Dirs{Runtime: e.runtime, State: e.state}
	st, err := store.Open(context.Background(), dirs.Database())
	if err != nil {
		e.t.Fatalf("store.Open() error = %v", err)
	}
	defer st.Close()

	if err := st.Delete(context.Background(), name); err != nil {
		e.t.Fatalf("Delete(%s) error = %v", name, err)
	}
}

// waitFor polls until cond holds, failing with describe if it never does.
//
// Polling rather than sleeping: how long a shell takes to produce output is not something to hardcode,
// and a fixed sleep is either flaky or slow.
func (e *env) waitFor(describe string, timeout time.Duration, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	e.t.Fatalf("timed out after %s waiting for %s", timeout, describe)
}

// waitForHistory blocks until a session's history contains want, and returns it.
func (e *env) waitForHistory(name, want string) string {
	e.t.Helper()
	var last string
	e.waitFor("session "+name+" history to contain "+want, 15*time.Second, func() bool {
		r := e.run("history", name)
		last = r.stdout
		return r.code == 0 && strings.Contains(r.stdout, want)
	})
	return last
}

// skipIfShort skips these tests under -short.
//
// They spawn processes and wait on real ptys, so they are slower than the rest of the suite and worth
// being able to opt out of.
func skipIfShort(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e tests spawn real processes; skipped under -short")
	}
}

// requireShell skips a test when the shell it needs is not installed.
//
// OSC 133 markers come from a shell's interactive prompt hooks, so a test about them needs a specific
// shell. Skipping beats failing on a machine that does not have it, and beats substituting /bin/sh,
// which emits no markers and would make the test assert nothing.
func requireShell(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s is not installed, and its prompt hooks are what emit OSC 133", path)
	}
}

// osc133rc is a zshrc that emits the OSC 133 markers cm reads.
//
// The tests supply this rather than relying on whatever the machine has configured. On a developer's
// machine kitty's shell integration is usually loaded and everything works; in a container there are no
// dotfiles at all, so the shell reports nothing and every test about busy state fails for a reason that
// has nothing to do with cm. That failure is also misleading: it looks like broken detection rather than
// a shell that was never asked to report.
//
// Deliberately the minimum, not a copy of kitty's integration: prompt start, command start with the
// cmdline extension, and command end with the exit status. Those are the markers the features depend on,
// and writing them out here means the test states its own preconditions.
//
// The D marker carries $? and must be the first thing precmd reads, since anything run before it would
// overwrite the status.
//
// It is sent only when a command actually started, tracked by _cm_ran. zsh runs precmd before the *first*
// prompt as well, so emitting D unconditionally reports a status for a shell that has run nothing -- a fresh
// session then looks like it just succeeded. kitty's integration guards this the same way, with its own
// _ksi_state, which is where the approach comes from rather than being invented here.
const osc133rc = `
autoload -Uz add-zsh-hook
typeset -g _cm_ran=0
_osc133_precmd() {
  local status_=$?
  if (( _cm_ran )); then
    printf '\033]133;D;%d\007' $status_
    _cm_ran=0
  fi
  printf '\033]133;A\007'
}
_osc133_preexec() {
  _cm_ran=1
  printf '\033]133;C;cmdline=%s\007' "${1// /\\ }"
}
add-zsh-hook precmd _osc133_precmd
add-zsh-hook preexec _osc133_preexec
PS1='` + promptMarker + `> '
`

// promptMarker is the prompt the test zshrc sets, and the signal that the shell is ready for input.
//
// A fixed, distinctive string rather than whatever zsh defaults to, because a test needs to *wait* for the
// prompt and so has to recognize it. The default prompt contains the hostname and cwd, which differ per
// machine and per test.
// No % in it: zsh's prompt expansion treats % as an escape introducer, so a marker ending in one arrives
// with it consumed and never matches. Found by reading the bytes cm renders rather than assuming.
const promptMarker = "CM_TEST_READY"

// waitForPrompt blocks until a session's shell is ready to read input.
//
// Waiting for `state == "running"` is not the same thing and was a real source of flakiness. A session is
// running the moment its process exists, while zsh still has to load its rc files and start its line editor;
// input written in between is read by the terminal driver before zle is listening, and the first character
// gets echoed twice.
//
// Readiness is proved with a round-trip rather than by looking for the prompt in the session's output, which
// was the first attempt and turned three passing tests into 30-second timeouts on the then-current no-cgo
// Linux image: `cm read` renders through the terminal emulator, so without one it returned empty successfully
// and the wait could never be satisfied. cgo is required now, but a round-trip is still the better signal --
// it proves the shell read and ran the input rather than that something was painted.
//
// A `send --wait idle` that succeeds proves the whole path: the shell read the input, ran it, and reported
// through OSC 133 that it was done. The server tracks those markers from the raw stream, so this needs no
// emulator. `true` is used because it changes nothing about the session.
//
// Only for sessions created with withOSC133, since without those markers the wait has nothing to resolve on.
func (e *env) waitForPrompt(session string) {
	e.t.Helper()
	e.waitFor(session+" to be ready for input", 30*time.Second, func() bool {
		return e.run("send", session, "true", "--enter", "--wait", "idle", "--timeout", "2s").code == 0
	})
}

// withOSC133 returns the flags that make a session's shell report OSC 133.
//
// Passed to `cm run --env` rather than exported into this process, because the server spawns shims: a
// variable this test exports is not in the server's environment and so never reaches the shell. That is
// exactly the gap `--env` exists to close.
//
// ZDOTDIR is what zsh reads instead of $HOME for its startup files, so the user's own configuration is
// neither needed nor consulted, and the test states its own preconditions.
func (e *env) withOSC133() []string {
	e.t.Helper()

	dir := filepath.Join(e.state, "zdotdir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".zshrc"), []byte(osc133rc), 0o600); err != nil {
		e.t.Fatalf("WriteFile() error = %v", err)
	}
	return []string{"--env", "ZDOTDIR=" + dir}
}
