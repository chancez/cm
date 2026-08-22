package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
)

// Finding kinds for resource and permission problems.
//
// These are the checks whose symptom appears somewhere other than cm. A pty leak breaks an unrelated
// program's terminal allocation; a loose directory mode exposes what was typed into a session. Neither shows
// up in cm's own output at all, which is what makes them worth a command.
const (
	// FindingPtyPressure is a system close to running out of ptys.
	FindingPtyPressure = "pty-pressure"
	// FindingLooseDirPerms is a runtime or state directory readable by other users.
	FindingLooseDirPerms = "loose-dir-perms"
	// FindingMissingLog is a session record naming an output log that is not there.
	FindingMissingLog = "missing-log"
	// FindingUnreachableShim is a session the server tracks whose shim does not answer.
	FindingUnreachableShim = "unreachable-shim"
)

// ptyPressureFraction is the share of the system pty limit that counts as pressure.
//
// A fraction rather than a count, since the limit differs by platform and by container: 511 on this darwin
// host, often far lower inside a container with its own devpts instance. 80% leaves room to act while still
// being unusual -- a laptop with a browser and an editor open sits near 13%.
const ptyPressureFraction = 0.8

// checkPtyPressure reports a system running low on ptys.
//
// This encodes the worst incident here so far. A test harness invoked a command that did not exist, ignored
// the error, and left every shim it started running; 426 accumulated against a limit of 511. Each held a pty
// and a shell. The symptom was not anything in cm: it was other programs failing to open a terminal, with an
// error that says nothing about why.
//
// Reported with cm's own share alongside the total, because the number that decides what to do is not the
// total but how much of it is cm's. 400 ptys with 2 sessions is someone else's problem; 400 with 380 sessions
// is `cm doctor --clean`.
func (m *Manager) checkPtyPressure() []Finding {
	used, limit, ok := ptyUsage()
	if !ok || limit <= 0 {
		// Pty accounting is unavailable on this platform or restricted in this container. Not knowing is not
		// a fault to report.
		return nil
	}
	if float64(used) < float64(limit)*ptyPressureFraction {
		return nil
	}

	m.mu.Lock()
	mine := len(m.sessions)
	m.mu.Unlock()

	detail := fmt.Sprintf(
		"%d of %d available ptys are in use, and this server accounts for %d of them; a program that "+
			"cannot allocate a terminal will fail with an error that does not mention this limit",
		used, limit, mine)
	if mine > 0 {
		detail += ", so check `cm list` for sessions nothing is attached to and `cm doctor --clean` for orphans"
	}
	return []Finding{{
		Kind:   FindingPtyPressure,
		Detail: detail,
		// Not fixable from here. Killing sessions to free ptys would destroy work that a user may be about
		// to reattach to, and the orphans that can safely be cleaned up are already reported separately.
		Fixable: false,
	}}
}

// checkDirPerms reports cm's directories being readable by anyone but their owner.
//
// A session's output log holds everything the shell printed and, for anything typed at a prompt with echo on,
// everything typed. The socket next to it grants control of a live shell. Both are created 0600, and the
// directories 0700, so a default installation is fine.
//
// The check exists because that is not enough. os.MkdirAll applies its mode only when it creates a directory:
// pointed at one that already exists it succeeds and leaves the mode alone. Verified rather than assumed --
// MkdirAll(dir, 0700) over an existing 0777 directory leaves it 0777. So anyone who sets CM_RUNTIME_DIR to a
// shared or pre-existing path gets a working installation with its contents exposed, and nothing says so.
func (m *Manager) checkDirPerms() []Finding {
	var findings []Finding
	for _, d := range []struct {
		what string
		path string
	}{
		{what: "runtime", path: m.dirs.Runtime},
		{what: "state", path: m.dirs.State},
	} {
		fi, err := os.Stat(d.path)
		if err != nil || !fi.IsDir() {
			// A missing directory is not a permission problem, and a state directory that does not exist yet
			// means nothing has been written to it.
			continue
		}
		// Group and other bits only. The owner's own access is not the question.
		if extra := fi.Mode().Perm() & 0o077; extra != 0 {
			findings = append(findings, Finding{
				Kind: FindingLooseDirPerms,
				Detail: fmt.Sprintf(
					"%s directory %s is mode %04o, so other users can reach session output logs and the "+
						"sockets that control live shells; `chmod 700 %s` (cm creates its own directories "+
						"correctly, but leaves the mode alone on one that already existed)",
					d.what, d.path, fi.Mode().Perm(), d.path),
				Socket:  d.path,
				Fixable: true,
			})
		}
	}
	return findings
}

// checkMissingLogs reports session records whose output log has gone.
//
// The record and the log are separate: sqlite holds the metadata and a file next to it holds the bytes. When
// they disagree the failure is late and confusing. `cm history` on a live session returns nothing, which
// looks like the emulator failing; reattaching restores a blank screen; a revived session comes back with no
// scrollback. All three read as a bug in restore rather than a missing file.
//
// Reported for finished sessions too, not just live ones, since reading a finished command's output is the
// whole point of keeping its record.
func (m *Manager) checkMissingLogs(ctx context.Context) []Finding {
	records, err := m.store.List(ctx)
	if err != nil {
		return nil
	}

	var missing []string
	for _, rec := range records {
		if rec.LogPath == "" {
			continue
		}
		// Absent is the finding. Any other error means the file is there and could not be read, which is a
		// permission problem that checkDirPerms covers rather than a missing log.
		if _, err := os.Stat(rec.LogPath); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		missing = append(missing, fmt.Sprintf("%s (%s)", rec.ID, rec.LogPath))
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return []Finding{{
		Kind: FindingMissingLog,
		Detail: fmt.Sprintf(
			"%d session record(s) name an output log that is not there, so `cm history` returns nothing and "+
				"reattaching restores a blank screen, both of which look like a bug in screen restore: %s",
			len(missing), strings.Join(missing, ", ")),
		// Not fixable: the bytes are gone. Deleting the record to make the inconsistency go away would also
		// discard the exit status and timings, which are the parts still intact.
		Fixable: false,
	}}
}

// checkTrackedShims reports sessions the server has in memory whose shims do not answer.
//
// Distinct from missing-shim, which compares the store against the sockets on disk. This compares the
// server's own live registry against reality, and catches the case where the server believes it is proxying a
// session that is not there: every command against it fails, while `cm list` keeps showing it as running.
//
// It is the shape of an adoption bug rather than a leak. A server that adopted a session and then lost the
// shim, or adopted one whose socket was replaced, would look exactly like this.
func (m *Manager) checkTrackedShims(ctx context.Context) []Finding {
	m.mu.Lock()
	type tracked struct {
		name   string
		socket string
	}
	live := make([]tracked, 0, len(m.sessions))
	for name, sess := range m.sessions {
		if ended, _ := sess.Ended(); ended {
			// A finished session's shim is meant to be gone, so silence is correct.
			continue
		}
		live = append(live, tracked{name: name, socket: m.dirs.ShimSocket(name)})
	}
	m.mu.Unlock()

	var findings []Finding
	for _, t := range live {
		if _, alive := probeShimState(ctx, t.socket); alive {
			continue
		}
		findings = append(findings, Finding{
			Kind:    FindingUnreachableShim,
			Session: t.name,
			Socket:  t.socket,
			Detail: "the server is tracking this session but its shim does not answer, so `cm list` shows " +
				"it as running while every command against it fails",
			// Not fixable here: dropping it from the registry would hide the inconsistency without
			// establishing what happened, and a shim that is merely slow to answer would be discarded.
			Fixable: false,
		})
	}
	return findings
}
