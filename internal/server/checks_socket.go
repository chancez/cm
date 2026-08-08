package server

import (
	"fmt"
	"os"
	"syscall"
)

// FindingUnreachableServer is a server listening on a socket path that no longer refers to it.
const FindingUnreachableServer = "unreachable-server"

// LogIfUnreachable records to the log when this server's socket path no longer refers to it.
//
// This exists because the check below, on its own, is unreachable. Both failure shapes leave this server's
// socket unlinked, so no client can name it, so no client can ever call Doctor on it: the diagnosis would be
// correct and nobody could ask for it. Verified by reproducing the incident, where `cm doctor` reached the
// replacement server and reported no problems while the stranded one held a live shell.
//
// The log is the way out. It lives in the state directory, not the runtime directory, so it survives the
// deletion that caused the problem and both servers append to the same file. A stranded server writing an
// ERROR there is picked up by the reachable server's server-errors check, which is how the finding gets in
// front of someone.
//
// Called periodically rather than once, since the directory can be removed at any point in a server's life,
// and logged at ERROR because that is the level the log check matches.
//
// Logged once per transition rather than on every tick. A minute's interval would otherwise append some 1400
// identical lines a day, which would bury every other error in the file and make the server-errors check
// useless for anything else -- the opposite of the point. Reset when the condition clears, so a path that is
// restored and broken again is reported again.
func (m *Manager) LogIfUnreachable() {
	findings := m.checkServerSocket()

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(findings) == 0 {
		m.reportedUnreachable = false
		return
	}
	if m.reportedUnreachable {
		return
	}
	m.reportedUnreachable = true
	for _, f := range findings {
		m.log.Error("this server is unreachable", "kind", f.Kind, "socket", f.Socket, "detail", f.Detail)
	}
}

// checkServerSocket reports a server whose own socket path no longer leads to it.
//
// Found the hard way. A runtime directory was deleted while a server was running, which unlinks its socket
// without stopping it: the process keeps listening on an inode nothing can name. Every subsequent command
// finds no socket, so it starts another server, and the first one goes on holding its sessions and their ptys
// with no way to reach them. `cm doctor` against that installation reported no problems, because it was
// talking to the new server about a runtime directory it had just recreated, while the old server and its
// shims were invisible to it.
//
// Two shapes, both detected by comparing the inode bound at startup against what the path resolves to now:
//
//   - The path is gone. The runtime directory was deleted; nothing can reach this server.
//   - The path resolves to a different inode. Another server bound it after this one's was unlinked, so
//     clients reach that one and this one's sessions are stranded.
//
// Verified rather than assumed: a listener whose directory is removed keeps accepting on the old inode, stat
// of the path fails, and a second Listen on the recreated path succeeds and produces a different inode.
//
// Only the affected server can report this, which is why it is a check on the server rather than something a
// client could work out. A client that cannot find a socket cannot tell "no server" from "a server it cannot
// name".
func (m *Manager) checkServerSocket() []Finding {
	if m.socketInode == 0 {
		// Not recorded, which happens for a Manager that was never given a listener: every unit test, and
		// any future caller that serves on a socket it opened itself. Nothing to compare against.
		return nil
	}

	path := m.dirs.ServerSocket()
	now, err := socketInode(path)
	switch {
	case err != nil:
		return []Finding{{
			Kind:   FindingUnreachableServer,
			Socket: path,
			Detail: fmt.Sprintf(
				"this server is still listening but %s is gone, so no client can reach it and every "+
					"command starts another server; its sessions and their ptys are stranded. The runtime "+
					"directory was most likely deleted while it was running. Stop this server to release "+
					"them: the sessions it holds cannot be adopted, so their shells will be lost",
				path),
			// Not fixable from here. The repair is to stop this server, and a diagnostic that stopped the
			// server answering it would be reporting its own suicide. It is also destructive: these sessions
			// cannot be adopted, so their shells go with it, and that is the user's call.
			Fixable: false,
		}}

	case now != m.socketInode:
		return []Finding{{
			Kind:   FindingUnreachableServer,
			Socket: path,
			Detail: fmt.Sprintf(
				"%s now refers to a different socket than the one this server bound, so clients reach "+
					"another server while this one's sessions are stranded. Two servers are running against "+
					"the same directories; stop this one to release its sessions, whose shells will be lost "+
					"since they cannot be adopted",
				path),
			Fixable: false,
		}}
	}
	return nil
}

// socketInode returns the inode a path currently resolves to.
//
// Compared by inode rather than by dialing, because dialing the path would reach whichever server holds it
// now: a second server answers happily, which makes the broken case look healthy. The inode says whether the
// name still refers to this process's socket.
func socketInode(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		// Unreachable on any platform cm builds for. Reported as an error rather than as inode 0, which the
		// caller reads as "not recorded" and would silently disable the check.
		return 0, fmt.Errorf("inode of %s is unavailable on this platform", path)
	}
	return uint64(st.Ino), nil
}
