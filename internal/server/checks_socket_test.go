package server

import (
	"bytes"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
)

// A server whose socket path stops referring to it reports itself unreachable.
//
// This encodes an incident that `cm doctor` was blind to. A runtime directory was deleted while a server was
// running, which unlinks its socket without stopping it: the process went on listening on an inode nothing
// could name, every later command started another server, and the first one kept holding its sessions and
// their ptys. Running the diagnosis reported no problems, because it reached the new server and asked about a
// directory it had just recreated.
//
// Only the affected server can see this. A client that finds no socket cannot distinguish "no server" from "a
// server whose name is gone", so the check has to live on the server and compare the inode it bound against
// what the path resolves to now.
func TestCheckServerSocket(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)

	socket := dirs.ServerSocket()
	l, ino, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer l.Close()
	if ino == 0 {
		t.Fatal("Listen() returned inode 0, so the check would be skipped and this test would assert nothing")
	}
	mgr.SetServerSocketInode(ino)

	// A healthy server: the path still names the socket it bound.
	if got := mgr.checkServerSocket(); len(got) != 0 {
		t.Fatalf("checkServerSocket() = %+v, want nothing while the path still names this socket", got)
	}

	// The path is deleted out from under the listener, which is what removing the runtime directory does. The
	// listener keeps accepting; nothing can reach it by name.
	if err := os.Remove(socket); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	got := mgr.checkServerSocket()
	if len(got) != 1 {
		t.Fatalf("checkServerSocket() = %+v, want one finding for a deleted socket path", got)
	}
	if got[0].Kind != FindingUnreachableServer {
		t.Errorf("kind = %q, want %q", got[0].Kind, FindingUnreachableServer)
	}
	// The consequence is spelled out, because the state is confusing enough that naming the symptom is the
	// useful part: commands appear to work, against a different server.
	if !strings.Contains(got[0].Detail, "starts another server") {
		t.Errorf("detail does not explain the consequence: %q", got[0].Detail)
	}
	// Not fixable: the repair is to stop this server, which would destroy shells that cannot be adopted, and
	// a diagnostic reporting its own shutdown is not a diagnostic.
	if got[0].Fixable {
		t.Error("Fixable = true, want false: stopping the server loses shells and is the user's call")
	}

	// The other shape: a second server binds the same path after this one's was unlinked. Clients reach that
	// one while this one's sessions are stranded, so dialing the path would look healthy and only the inode
	// tells the truth.
	l2, ino2, err := Listen(socket)
	if err != nil {
		t.Fatalf("second Listen() error = %v", err)
	}
	defer l2.Close()
	if ino2 == ino {
		t.Fatal("the replacement socket has the same inode, so this case cannot be distinguished")
	}

	got = mgr.checkServerSocket()
	if len(got) != 1 {
		t.Fatalf("checkServerSocket() = %+v, want one finding for a replaced socket", got)
	}
	if !strings.Contains(got[0].Detail, "different socket") {
		t.Errorf("detail does not say the path was replaced: %q", got[0].Detail)
	}
}

// A Manager with no recorded inode does not report a problem.
//
// The check is skipped rather than firing, because every unit test and any caller that serves on a socket it
// opened itself leaves this unset. Reporting on a missing value would make the check useless noise.
func TestCheckServerSocketIsSkippedWithoutAnInode(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	// Deliberately not calling SetServerSocketInode, and with no socket on disk either, which is the
	// strongest form of "nothing to compare against".
	if got := mgr.checkServerSocket(); len(got) != 0 {
		t.Errorf("checkServerSocket() = %+v, want nothing when no inode was recorded", got)
	}
}

// Listen reports the inode of the socket it bound.
//
// Returned from Listen rather than stat'd by the caller so it is read while this process holds the only claim
// on the path. A caller that stat'd it later could race a replacement and record the wrong inode, which would
// make the check report a problem that does not exist.
func TestListenReportsItsInode(t *testing.T) {
	_, _, dirs := newTestManager(t, nil)
	socket := dirs.ServerSocket()

	l, ino, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer l.Close()

	onDisk, err := socketInode(socket)
	if err != nil {
		t.Fatalf("socketInode() error = %v", err)
	}
	if ino != onDisk {
		t.Errorf("Listen() inode = %d, want %d as found on disk", ino, onDisk)
	}
	// And it really is a socket that was bound, not just a path that exists.
	if _, ok := l.(*net.UnixListener); !ok {
		t.Errorf("Listen() returned %T, want a *net.UnixListener", l)
	}
}

// Becoming unreachable is logged once per transition, not on every check.
//
// The bound matters more than it looks. A stranded server cannot be asked anything, so the log is the only
// channel by which the finding reaches anyone: it lives in the state directory, which survives the deletion
// that caused the problem, and the reachable server's server-errors check picks it up from there. A line
// appended on every tick would put roughly 1400 identical entries a day into that file and bury every other
// error, which defeats the check it depends on.
func TestLogIfUnreachableReportsEachTransitionOnce(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)

	var buf bytes.Buffer
	mgr.SetLogger(slog.New(slog.NewTextHandler(&buf, nil)))

	socket := dirs.ServerSocket()
	l, ino, err := Listen(socket)
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer l.Close()
	mgr.SetServerSocketInode(ino)

	// Healthy: nothing logged, however many times it is checked.
	for range 3 {
		mgr.LogIfUnreachable()
	}
	if n := strings.Count(buf.String(), "this server is unreachable"); n != 0 {
		t.Fatalf("logged %d times while healthy, want 0", n)
	}

	// Unreachable, checked repeatedly: logged once.
	if err := os.Remove(socket); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	for range 5 {
		mgr.LogIfUnreachable()
	}
	if n := strings.Count(buf.String(), "this server is unreachable"); n != 1 {
		t.Fatalf("logged %d times for one transition, want 1", n)
	}
	// At ERROR, since that is the level the server-errors check matches. A WARN here would be recorded and
	// never surfaced, which is the same as not reporting it.
	if !strings.Contains(buf.String(), "level=ERROR") {
		t.Errorf("not logged at ERROR, so the log check will not surface it: %q", buf.String())
	}

	// Restored and broken again: reported again, since it is a new occurrence rather than the same one.
	l2, ino2, err := Listen(socket)
	if err != nil {
		t.Fatalf("second Listen() error = %v", err)
	}
	defer l2.Close()
	mgr.SetServerSocketInode(ino2)
	mgr.LogIfUnreachable()
	if n := strings.Count(buf.String(), "this server is unreachable"); n != 1 {
		t.Fatalf("logged %d times after the condition cleared, want the count to stay at 1", n)
	}

	if err := os.Remove(socket); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	mgr.LogIfUnreachable()
	if n := strings.Count(buf.String(), "this server is unreachable"); n != 2 {
		t.Errorf("logged %d times after a second transition, want 2", n)
	}
}
