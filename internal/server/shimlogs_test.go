package server

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/chancez/cm/internal/store"
)

// The lines a shim writes around a session's life.
//
// Built from times rather than hardcoded for the same reason logAt is: pruning filters on age, so a literal
// timestamp would pass today and start failing a week later.
func shimStarted(when time.Time) string { return logAt(when, "INFO", "shim started") }
func shimExited(when time.Time) string  { return logAt(when, "INFO", "shim exiting") }

// shimLogNames lists the files left in the shim log directory.
func shimLogNames(t *testing.T, mgr *Manager) []string {
	t.Helper()
	entries, err := os.ReadDir(mgr.dirs.ShimLogDir())
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// The whole point: a log with no live session behind it goes once retention passes, and everything with a
// reason to be kept stays.
//
// Asserted as the entire directory listing rather than file by file, because the failure that matters is
// deleting a log that should have been kept, and a check on the one expected deletion would pass while the
// directory was emptied.
func TestPruneShimLogsRemovesOnlyOldFinishedSessions(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetShimLogRetention(7 * 24 * time.Hour)
	now := time.Now()

	// Over and eight days idle, so past a seven-day retention. The one file that should go.
	writeLog(t, dirs.ShimLog("old"),
		shimStarted(now.Add(-9*24*time.Hour)), shimExited(now.Add(-8*24*time.Hour)))
	// Over an hour ago, well inside retention.
	writeLog(t, dirs.ShimLog("recent"),
		shimStarted(now.Add(-2*time.Hour)), shimExited(now.Add(-time.Hour)))

	n, err := mgr.PruneShimLogs(context.Background(), now)
	if err != nil {
		t.Fatalf("PruneShimLogs() error = %v", err)
	}
	if n != 1 {
		t.Errorf("PruneShimLogs() = %d, want 1", n)
	}
	if got, want := shimLogNames(t, mgr), []string{"recent.log"}; !equalStrings(got, want) {
		t.Errorf("shim log directory = %v, want %v", got, want)
	}
}

// A shim that died without logging an exit is still pruned once its session is gone and retention passes.
//
// This is the bug the first version of this code had, and it is not hypothetical. A shim writes "shim
// exiting" only when it returns cleanly, so SIGKILL, an OOM kill, and pty exhaustion all leave a log that
// ends on "shim started". Requiring an exit line kept every one of those forever, which is the leak this was
// written to fix: measured on a real install, 30 of 210 shim logs contained no exit line at all.
func TestPruneShimLogsPrunesShimKilledWithoutLoggingExit(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetShimLogRetention(7 * 24 * time.Hour)
	now := time.Now()

	// Started nine days ago and never logged anything else: killed rather than ended. No record, no
	// registry entry, and no socket, so nothing suggests the session still exists.
	writeLog(t, dirs.ShimLog("killed"), shimStarted(now.Add(-9*24*time.Hour)))

	n, err := mgr.PruneShimLogs(context.Background(), now)
	if err != nil {
		t.Fatalf("PruneShimLogs() error = %v", err)
	}
	if n != 1 {
		t.Errorf("PruneShimLogs() = %d, want 1: a shim can die without logging an exit", n)
	}
	if got := shimLogNames(t, mgr); len(got) != 0 {
		t.Errorf("shim log directory = %v, want empty", got)
	}
}

// A session still in the store keeps its log however idle it is, so `cm logs shim NAME` works for as long as
// `cm list` shows the session.
func TestPruneShimLogsKeepsRecordedSession(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	mgr.SetShimLogRetention(7 * 24 * time.Hour)
	now := time.Now()

	writeLog(t, dirs.ShimLog("recorded"),
		shimStarted(now.Add(-30*24*time.Hour)), shimExited(now.Add(-29*24*time.Hour)))
	if err := st.Create(context.Background(), store.Session{
		ID:        "recorded",
		State:     store.StateExited,
		CreatedAt: now.Add(-30 * 24 * time.Hour),
		UpdatedAt: now.Add(-29 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	n, err := mgr.PruneShimLogs(context.Background(), now)
	if err != nil {
		t.Fatalf("PruneShimLogs() error = %v", err)
	}
	if n != 0 {
		t.Errorf("PruneShimLogs() = %d, want 0: the session record still exists", n)
	}
	if got, want := shimLogNames(t, mgr), []string{"recorded.log"}; !equalStrings(got, want) {
		t.Errorf("shim log directory = %v, want %v", got, want)
	}
}

// A shim socket on disk keeps the log even with no record and nothing in the registry.
//
// That combination is an orphan: a shim this server knows nothing about, which is reachable only through its
// socket and is what `cm doctor` reports. Its log is the one account of what it is doing, and a socket cannot
// be dialled to settle the question, since ECONNREFUSED means a full backlog as often as an absent listener.
func TestPruneShimLogsKeepsOrphanWithSocket(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetShimLogRetention(7 * 24 * time.Hour)
	now := time.Now()

	writeLog(t, dirs.ShimLog("orphan"), shimStarted(now.Add(-30*24*time.Hour)))
	// The file only has to exist: presence is the signal, deliberately, rather than anything answering.
	if err := os.WriteFile(dirs.ShimSocket("orphan"), nil, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	n, err := mgr.PruneShimLogs(context.Background(), now)
	if err != nil {
		t.Fatalf("PruneShimLogs() error = %v", err)
	}
	if n != 0 {
		t.Errorf("PruneShimLogs() = %d, want 0: a shim socket exists for this session", n)
	}
	if got, want := shimLogNames(t, mgr), []string{"orphan.log"}; !equalStrings(got, want) {
		t.Errorf("shim log directory = %v, want %v", got, want)
	}
}

// Zero retention disables pruning, which is how a user says "keep every shim log".
func TestPruneShimLogsDisabledByZeroRetention(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	now := time.Now()

	writeLog(t, dirs.ShimLog("ancient"),
		shimStarted(now.Add(-365*24*time.Hour)), shimExited(now.Add(-364*24*time.Hour)))

	n, err := mgr.PruneShimLogs(context.Background(), now)
	if err != nil {
		t.Fatalf("PruneShimLogs() error = %v", err)
	}
	if n != 0 {
		t.Errorf("PruneShimLogs() = %d, want 0 with retention unset", n)
	}
	if got, want := shimLogNames(t, mgr), []string{"ancient.log"}; !equalStrings(got, want) {
		t.Errorf("shim log directory = %v, want %v", got, want)
	}
}

// A rotated generation goes with the log it belongs to. Leaving it would make pruning look like it had run
// while a file stayed behind.
func TestPruneShimLogsRemovesRotatedGeneration(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetShimLogRetention(7 * 24 * time.Hour)
	now := time.Now()

	writeLog(t, dirs.ShimLog("old"),
		shimStarted(now.Add(-9*24*time.Hour)), shimExited(now.Add(-8*24*time.Hour)))
	writeLog(t, dirs.ShimLog("old")+rotatedSuffix,
		shimStarted(now.Add(-11*24*time.Hour)), shimExited(now.Add(-10*24*time.Hour)))

	n, err := mgr.PruneShimLogs(context.Background(), now)
	if err != nil {
		t.Fatalf("PruneShimLogs() error = %v", err)
	}
	// One log pruned, not two: the rotated file is part of the same log rather than a candidate of its own.
	if n != 1 {
		t.Errorf("PruneShimLogs() = %d, want 1", n)
	}
	if got := shimLogNames(t, mgr); len(got) != 0 {
		t.Errorf("shim log directory = %v, want empty", got)
	}
}

// A session's output log lives directly in logs/ while its shim log lives in logs/shim/, and both are
// NAME.log. Pruning must not touch the output log, which is what the shell printed and the thing a user
// would actually miss.
//
// Worth a test rather than obvious from the code: the shared base name is exactly the collision that made
// `cm doctor` label log findings by directory, and six such pairs existed on a real install.
func TestPruneShimLogsLeavesSessionOutputAlone(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetShimLogRetention(7 * 24 * time.Hour)
	now := time.Now()

	writeLog(t, dirs.ShimLog("old"),
		shimStarted(now.Add(-9*24*time.Hour)), shimExited(now.Add(-8*24*time.Hour)))
	// Not a real seqlog, which does not matter: what is asserted is that the file survives.
	output := dirs.SessionLog("old")
	if err := os.WriteFile(output, []byte("cmlog1\nsession output"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := mgr.PruneShimLogs(context.Background(), now); err != nil {
		t.Fatalf("PruneShimLogs() error = %v", err)
	}

	if _, err := os.Stat(output); err != nil {
		t.Errorf("Stat(%s) error = %v, want the session output log untouched", output, err)
	}
	if got := shimLogNames(t, mgr); len(got) != 0 {
		t.Errorf("shim log directory = %v, want empty", got)
	}
}

// A file that is not a log, and a subdirectory, are both left alone rather than tripping the sweep.
func TestPruneShimLogsIgnoresNonLogs(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetShimLogRetention(7 * 24 * time.Hour)

	dir := dirs.ShimLogDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hand-written"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	n, err := mgr.PruneShimLogs(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("PruneShimLogs() error = %v", err)
	}
	if n != 0 {
		t.Errorf("PruneShimLogs() = %d, want 0", n)
	}
	if got, want := shimLogNames(t, mgr), []string{"notes.txt", "subdir"}; !equalStrings(got, want) {
		t.Errorf("shim log directory = %v, want %v", got, want)
	}
}

// A missing directory is a fresh installation, not a problem.
func TestPruneShimLogsToleratesMissingDirectory(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.SetShimLogRetention(7 * 24 * time.Hour)

	if err := os.RemoveAll(dirs.ShimLogDir()); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}

	n, err := mgr.PruneShimLogs(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("PruneShimLogs() error = %v", err)
	}
	if n != 0 {
		t.Errorf("PruneShimLogs() = %d, want 0", n)
	}
}

// shimLogLastActivity dates a log by its newest entry, falling back to mtime.
//
// Table-driven because the interesting input is the shape of the file rather than the manager around it.
func TestShimLogLastActivity(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	newest := now.Add(-8 * 24 * time.Hour)

	for _, tc := range []struct {
		name  string
		lines []string
		want  time.Time
		ok    bool
	}{
		{
			name:  "started then exited takes the exit",
			lines: []string{shimStarted(now.Add(-9 * 24 * time.Hour)), shimExited(newest)},
			want:  newest,
			ok:    true,
		},
		{
			// The case the old implementation could not date at all, which is why it never pruned these.
			name:  "no exit line takes the start",
			lines: []string{shimStarted(newest)},
			want:  newest,
			ok:    true,
		},
		{
			// A warning is activity too. Dating by lifecycle lines alone would ignore the newest thing in
			// the file, and "persisting output failed" is exactly what a shim log is read for.
			name: "newest line is a warning",
			lines: []string{
				shimStarted(now.Add(-9 * 24 * time.Hour)),
				logAt(newest, "WARN", "persisting output failed"),
			},
			want: newest,
			ok:   true,
		},
		{
			// Out-of-order lines cannot date the file earlier than what it already contains, so a truncated
			// or interleaved tail cannot bring a log forward for deletion.
			name: "unparseable tail does not win",
			lines: []string{
				shimStarted(now.Add(-9 * 24 * time.Hour)),
				shimExited(newest),
				`time=not-a-time level=INFO msg="partial write`,
			},
			want: newest,
			ok:   true,
		},
		{
			name: "several restart generations take the newest",
			lines: []string{
				shimStarted(now.Add(-30 * 24 * time.Hour)),
				shimExited(now.Add(-29 * 24 * time.Hour)),
				shimStarted(now.Add(-20 * 24 * time.Hour)),
				shimExited(newest),
			},
			want: newest,
			ok:   true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shim.log")
			writeLog(t, path, tc.lines...)

			got, ok := shimLogLastActivity(path)
			if ok != tc.ok {
				t.Fatalf("shimLogLastActivity() ok = %v, want %v", ok, tc.ok)
			}
			if !got.Equal(tc.want) {
				t.Errorf("shimLogLastActivity() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A log with no parseable timestamp falls back to its modification time, so a file that would otherwise be
// undatable is still prunable rather than kept forever.
func TestShimLogLastActivityFallsBackToModTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shim.log")
	writeLog(t, path, "this is not slog output at all")

	want := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, want, want); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	got, ok := shimLogLastActivity(path)
	if !ok {
		t.Fatalf("shimLogLastActivity() ok = false, want true via the mtime fallback")
	}
	if !got.Equal(want) {
		t.Errorf("shimLogLastActivity() = %v, want %v", got, want)
	}
}

// A file that cannot be opened is kept rather than pruned, since a file that cannot be judged is one whose
// deletion would destroy the evidence for whatever made it unreadable.
func TestShimLogLastActivityUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.log")

	got, ok := shimLogLastActivity(path)
	if ok {
		t.Errorf("shimLogLastActivity() = (%v, true), want (zero, false) for a missing file", got)
	}
}

// equalStrings compares two string slices.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
