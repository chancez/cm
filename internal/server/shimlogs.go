package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// shimLogSuffix is the extension every shim log carries. See paths.ShimLog.
const shimLogSuffix = ".log"

// rotatedSuffix is what cmlog appends to the previous generation when a log rotates.
//
// Named here rather than reached for from cmlog, which keeps the rotation private. A shim log holds two
// lines per session lifetime against a 4 MiB rotation threshold, so a rotated generation is close to
// impossible in practice; it is handled anyway because leaving it behind would make pruning look like it
// had run while a file remained.
const rotatedSuffix = ".1"

// PruneShimLogs removes the diagnostic logs of sessions that are over, once the retention period passes.
//
// Necessary rather than tidy, and for the same reason as ExpireDeadSessions: a machine that opens a session
// per terminal window accumulates one file per window, forever. Measured before this existed: 210 files in
// logs/shim on the machine cm is built for, with nothing able to reap any of them.
//
// Whether a session is over is decided by the store and the shim, never by the log. That is the ordering the
// rest of cm follows -- a record can lag, so liveness is "does a shim answer" -- and getting it backwards
// produced a bug in the first version of this. It required the log to end in "shim exiting" before pruning,
// which a shim writes only when it returns cleanly. A shim killed with SIGKILL, OOM-killed, or lost to pty
// exhaustion never writes that line, so its log was kept forever. Measured on the same install: 30 of 210
// logs contained no exit line at all, and those are the sessions that ended badly, which is to say the ones
// most worth bounding.
//
// Three things must all hold, and each protects a log that is still the only account of something:
//
//   - No session record. logFiles reads every shim log present precisely because a shim whose record is gone
//     is the interesting case, so a log is never removed while `cm list` can still name its session.
//   - Nothing live in the registry, and no shim socket in the runtime directory. The socket is a stat rather
//     than a dial, deliberately: a dial cannot separate a busy shim from a dead one, since ECONNREFUSED
//     means a full backlog as often as an absent listener. A socket that exists keeps the log, which errs
//     toward keeping a file a shim might still be writing to.
//   - The log's own last activity is older than the retention period. See shimLogLastActivity.
//
// Kept separate from ExpireDeadSessions rather than folded into it, because the two walk different things
// and would fight: expiry walks records and deletes rows, while this walks files whose records are already
// gone. A session expired an hour ago keeps its shim log for the rest of the retention period, and that is
// the point of having a second period at all.
func (m *Manager) PruneShimLogs(ctx context.Context, now time.Time) (int, error) {
	if m.shimLogRetention <= 0 {
		return 0, nil
	}

	records, err := m.store.List(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("listing sessions to prune shim logs: %w", err)
	}
	recorded := make(map[string]struct{}, len(records))
	for _, rec := range records {
		recorded[rec.Name] = struct{}{}
	}

	dir := m.dirs.ShimLogDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// No directory yet, which is a fresh installation rather than a problem.
			return 0, nil
		}
		return 0, fmt.Errorf("reading %s: %w", dir, err)
	}

	cutoff := now.Add(-m.shimLogRetention)
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		// Only the current generation is a candidate. A rotated file goes with the log it belongs to, below,
		// rather than being judged on its own.
		name := strings.TrimSuffix(e.Name(), shimLogSuffix)
		if name == e.Name() || name == "" {
			continue
		}

		if m.sessionMayStillExist(name, recorded) {
			continue
		}

		path := filepath.Join(dir, e.Name())
		last, ok := shimLogLastActivity(path)
		if !ok || last.After(cutoff) {
			continue
		}

		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			// Warned about rather than returned, so one unreadable file does not stop the sweep. The next
			// tick tries again, and a log that cannot be removed is a smaller problem than a directory that
			// stops being pruned.
			m.log.Warn("removing pruned shim log failed", "session", name, "path", path, "error", err)
			continue
		}
		removed++
		m.log.Info("pruned shim log", "session", name, "last_activity", last, "age", now.Sub(last))

		if err := os.Remove(path + rotatedSuffix); err != nil && !os.IsNotExist(err) {
			m.log.Warn("removing rotated shim log failed",
				"session", name, "path", path+rotatedSuffix, "error", err)
		}
	}
	return removed, nil
}

// sessionMayStillExist reports whether anything suggests the named session is not over.
//
// Three signals, cheapest first, and any one of them keeps the log. Deliberately biased toward keeping: a
// log kept too long costs a few kilobytes, while one removed early destroys the only account of a session
// that vanished.
func (m *Manager) sessionMayStillExist(name string, recorded map[string]struct{}) bool {
	// A record in any state, since the record is what makes the session nameable: `cm logs shim NAME` has to
	// keep working for as long as `cm list` shows it.
	if _, ok := recorded[name]; ok {
		return true
	}
	// Live in the registry even with no record, since the record can lag behind the registry.
	if _, live := m.Get(name); live {
		return true
	}
	// A shim socket on disk, which is how a shim this server knows nothing about still announces itself: an
	// orphan whose record was deleted is reachable through exactly this path, and doctor finds orphans the
	// same way.
	//
	// Stat rather than dial. Only ENOENT is conclusive about a socket, and absence is precisely what is being
	// asked here, so a socket that exists for any reason -- including a stale one -- keeps the log.
	if _, err := os.Stat(m.dirs.ShimSocket(name)); err == nil {
		return true
	}
	return false
}

// shimLogLastActivity reports when the shim last wrote anything to its log.
//
// The newest timestamp in the file rather than the exit line, so a shim that died without logging an exit is
// still datable. That is the reason this is not shimLogExitedAt: the exit line is missing for every session
// killed rather than ended, and keying on it kept those logs forever.
//
// Modification time is a fallback rather than the primary source, since it is the less trustworthy of the
// two: copying, restoring from a backup, or a filesystem move rewrites it, while a shim log's own timestamps
// were written by the process whose life it describes. It is still better than nothing for a log holding no
// parseable line, which would otherwise never be prunable.
//
// False means the file could not be read or dated at all, and such a file is kept. It cannot be judged, and
// deleting what cannot be judged is how the evidence for a failure gets destroyed.
func shimLogLastActivity(path string) (time.Time, bool) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()

	var newest time.Time
	sc := bufio.NewScanner(f)
	// A long line is possible, since a logged error can embed a command line or a path.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// Every line's timestamp is considered rather than the last line's alone. slog writes one per line
		// and a shim log is appended in order, but a truncated final write can leave a partial line, and
		// taking the maximum means an unparseable tail cannot date the file earlier than what is already
		// recorded in it.
		if when, ok := logStamp(strings.TrimSpace(sc.Text())); ok && when.After(newest) {
			newest = when
		}
	}
	// A read error still lets what was parsed stand, since the unread part can only be newer and treating a
	// partly-read file as undatable would keep it forever.
	if !newest.IsZero() {
		return newest, true
	}

	info, err := f.Stat()
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}
