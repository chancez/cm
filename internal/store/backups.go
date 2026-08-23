package store

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ExpireBackups removes pre-migration snapshots older than keep, returning what it removed.
//
// Time-based because there is no other moment to pick. A snapshot cannot be deleted when the migration
// succeeds, which is when it becomes useful rather than when it stops being: it exists for a rollback,
// and a rollback happens later or not at all.
//
// What bounds it is that a snapshot's usefulness *decays*, and does so in a way that makes it steadily
// more dangerous to restore rather than merely less helpful. Every session created after it was taken is
// absent from it, and a session absent from the database is one whose shim nothing can find again, so
// restoring a week-old snapshot strands a week of shells. That is the argument for an age limit: past
// some point, reinstalling the newer build is the only sane recovery, and keeping the file only invites
// the other one.
//
// keep of zero disables removal, for someone who would rather hold every snapshot.
//
// The glob is anchored on both ends against the database's own path, so it can match neither the database
// nor its -wal and -shm sidecars. That is worth stating because the failure mode of getting it wrong is
// this function deleting the live database.
func ExpireBackups(dbPath string, keep time.Duration, now time.Time) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}

	matches, err := filepath.Glob(dbPath + ".v*.bak")
	if err != nil {
		return nil, fmt.Errorf("listing database snapshots: %w", err)
	}

	cutoff := now.Add(-keep)
	var removed []string
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			// Gone between the glob and the stat, or unreadable. Either way there is nothing to do and
			// nothing worth failing a sweep over.
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return removed, fmt.Errorf("removing database snapshot %s: %w", path, err)
		}
		removed = append(removed, path)
	}
	sort.Strings(removed)
	return removed, nil
}
