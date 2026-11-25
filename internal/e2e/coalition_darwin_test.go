//go:build darwin

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// A session keeps the resource limits of the client that started the server rather than taking launchd's.
//
// A launchd job starts with launchd's limits rather than its submitter's, and every process inherits the limits
// of its parent, so the server and each shell under it took launchd's: `ulimit -n` in a session read 256, the
// soft default launchd reports for maxfiles, against 1048576 when the same client started the server directly.
// A build or a language server at 256 open files fails with "too many open files", with nothing about the cause
// in any log.
//
// Compared against this process's own limits because the client is a Go program started by this one, so it
// sees the same values: Go raises its soft open-files limit at startup the same way in both, and the number of
// processes is inherited unchanged.
func TestSessionsKeepTheLimitsOfTheClientThatStartedTheServer(t *testing.T) {
	skipIfShort(t)
	if !guiDomainExists() {
		t.Skip("no gui launchd domain to start the server in, so cm starts it directly")
	}
	e := newEnv(t)
	useLaunchd(e)

	got := strings.TrimSpace(e.mustRun("run", "--", "/bin/sh", "-c", "echo $(ulimit -n) $(ulimit -u)"))

	var nofile, nproc syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &nofile); err != nil {
		t.Fatalf("Getrlimit(RLIMIT_NOFILE) error = %v", err)
	}
	if err := syscall.Getrlimit(unix.RLIMIT_NPROC, &nproc); err != nil {
		t.Fatalf("Getrlimit(RLIMIT_NPROC) error = %v", err)
	}
	if want := fmt.Sprintf("%d %d", nofile.Cur, nproc.Cur); got != want {
		t.Errorf("a session's `ulimit -n` and `ulimit -u` = %q, want %q", got, want)
	}
}

// The server, and every session under it, must not share a coalition with whatever started the first client.
//
// macOS groups processes into coalitions at spawn, and Setsid does not leave one. LaunchServices treats every
// process in an app's coalition as that app's, so once the app quits with them still running, its Dock icon
// says "Running in Background", and Force Quit or "Stop Running in Background" on it kills them all. That is
// how 27 sessions died at once: a server had been started from Terminal.app, its successors were started by
// commands run inside its sessions, so six hours and two upgrades later everything was still in Terminal's
// coalition, and quitting Terminal's leftover Dock icon took it down. The Dock's own log listed each pid it
// killed, the server's among them.
//
// A coalition is the test's to compare because this test process stands in for the app: it is whatever
// started the client, and the server must end up somewhere else.
func TestServerRunsOutsideTheCoalitionThatStartedIt(t *testing.T) {
	skipIfShort(t)
	if !guiDomainExists() {
		t.Skip("no gui launchd domain to start the server in, so cm starts it directly")
	}
	e := newEnv(t)
	useLaunchd(e)

	got := placementOf(t, e)
	// The shell is checked as well because it is what is actually lost: a server in a coalition of its own
	// whose shims were somewhere else would fix nothing.
	want := placement{
		ServerSharesOurs:   false,
		ShellSharesServers: true,
	}
	if got != want {
		t.Errorf("placement = %+v, want %+v", got, want)
	}
}

// Without [server] launchd, the server is a child of the client and stays in the coalition that started it.
//
// The default, because under launchd cm becomes the privacy (TCC) responsible process for every program in a
// session: `op vaults list` got EPERM reading 1Password's group container and reported not being signed in,
// where the terminal that started the client already had the grant. Checked so that the default cannot
// quietly go back to launchd, which would bring that back with nothing failing.
func TestServerStaysInTheCoalitionThatStartedItByDefault(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	got := placementOf(t, e)
	want := placement{
		ServerSharesOurs:   true,
		ShellSharesServers: true,
	}
	if got != want {
		t.Errorf("placement = %+v, want %+v", got, want)
	}
}

// useLaunchd turns on [server] launchd in e and restarts its server, which newEnv already started without it.
func useLaunchd(e *env) {
	e.writeConfig("[server]\nlaunchd = true\n")
	e.mustRun("server", "restart")
}

// placement is which coalitions a session's shell and its server share.
type placement struct {
	ServerSharesOurs   bool
	ShellSharesServers bool
}

// placementOf starts a session in e and reports where it and its server landed relative to this process.
func placementOf(t *testing.T, e *env) placement {
	t.Helper()
	e.mustRun("run", "--session", "work", "-d", "--", "/bin/sh", "-c", "sleep 120")
	shell := e.sessionDetail(t, "work").ShellPID
	server := int(e.cmStatus().PID)
	return placement{
		ServerSharesOurs:   coalitionOf(t, server) == coalitionOf(t, os.Getpid()),
		ShellSharesServers: coalitionOf(t, shell) == coalitionOf(t, server),
	}
}

// guiDomainExists reports whether this user has a launchd gui domain, which is where cm starts its server.
func guiDomainExists() bool {
	return exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())).Run() == nil
}

// coalitionOf returns the jetsam coalition id of a process, which is the id LaunchServices kills by.
//
// proc_pidinfo's PROC_PIDCOALITIONINFO flavor is not in the public SDK headers, so it is spelled out here
// from xnu's sys/proc_info.h. The jetsam id rather than the resource one: the Dock logged
// `_LSForceQuitApplication ... coalition=14404`, and 14404 was the jetsam id of every process it then killed.
func coalitionOf(t *testing.T, pid int) uint64 {
	t.Helper()

	const (
		procInfoCallPIDInfo   = 2
		procPIDCoalitionInfo  = 20
		coalitionTypeJetsam   = 1
		coalitionTypesTracked = 2
	)
	var info struct {
		ID       [coalitionTypesTracked]uint64
		Reserved [3]uint64
	}
	n, _, errno := unix.Syscall6(unix.SYS_PROC_INFO, procInfoCallPIDInfo, uintptr(pid), procPIDCoalitionInfo, 0,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if errno != 0 || n != unsafe.Sizeof(info) {
		t.Fatalf("proc_pidinfo(%d, PROC_PIDCOALITIONINFO) = %d, %v", pid, n, errno)
	}
	return info.ID[coalitionTypeJetsam]
}
