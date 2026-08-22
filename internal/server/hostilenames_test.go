package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// hostileNames are session names no client would send.
//
// The client validates names on every command that takes one, so these can only arrive from something speaking
// the protocol directly. That is worth testing because a client is not the only possible caller and because the
// protections here are structural rather than a check at the door: names become filenames for the shim socket
// and the output log, so a name containing a separator is a path traversal question.
var hostileNames = []string{
	"../escape",
	"../../escape",
	"/tmp/absolute",
	"a/b",
	".",
	"..",
	"tab\tname",
	"new\nline",
	"nul\x00byte",
	strings.Repeat("x", 300),
	"",
}

// No RPC taking a session name creates anything outside the runtime directory.
//
// The invariant is that only Manager.Open builds a path from a name, and it validates first; every other
// handler either looks the name up and fails, or passes it to the store where it is just a string. That holds
// today by construction rather than by a check in each handler, which is exactly the kind of property that
// breaks quietly when someone adds a call site.
func TestHostileSessionNamesCreateNothingOutsideTheRuntimeDir(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()

	// The parent of both directories, so an escape by one or two levels would show up.
	parent := filepath.Dir(dirs.Runtime)
	before := dirEntries(t, parent)

	for _, name := range hostileNames {
		// Report, Kill, and History between them cover the shapes: one that writes state, one that removes a
		// session, and one that reads a log.
		_, _ = svc.Report(ctx, &serverv1.ReportRequest{
			Session: name, State: serverv1.ReportedState_REPORTED_STATE_BUSY,
		})
		_, _ = svc.Kill(ctx, &serverv1.KillRequest{Sessions: []string{name}})
		_, _ = svc.History(ctx, &serverv1.HistoryRequest{Session: name})
	}

	if after := dirEntries(t, parent); after != before {
		t.Errorf("entries in %s went from %q to %q: a hostile name escaped the runtime directory",
			parent, before, after)
	}
	// And nothing landed inside the runtime or state directories under a name that should have been refused.
	for _, dir := range []string{dirs.Runtime, dirs.State} {
		for _, e := range mustReadDir(t, dir) {
			if strings.ContainsAny(e, "\t\n\x00/") || strings.HasPrefix(e, ".") {
				t.Errorf("%s contains %q, which no valid session name could produce", dir, e)
			}
		}
	}
}

// Every RPC rejects a hostile name rather than acting on it.
//
// Distinct from the traversal check above: that one asks whether anything escaped, this one asks whether the
// call was refused. A handler that silently succeeded on a name that cannot exist would make `cm kill` report
// success for a typo, which is worse than an error because a script would not notice.
func TestHostileSessionNamesAreRejected(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	svc := NewService(mgr)
	ctx := context.Background()

	for _, name := range hostileNames {
		t.Run(nameForTest(name), func(t *testing.T) {
			if _, err := svc.Report(ctx, &serverv1.ReportRequest{
				Session: name, State: serverv1.ReportedState_REPORTED_STATE_BUSY,
			}); err == nil {
				t.Errorf("Report(%q) error = nil, want a rejection", name)
			}
			if _, err := svc.History(ctx, &serverv1.HistoryRequest{Session: name}); err == nil {
				t.Errorf("History(%q) error = nil, want a rejection", name)
			}
			if _, err := svc.Read(ctx, &serverv1.ReadRequest{Session: name}); err == nil {
				t.Errorf("Read(%q) error = nil, want a rejection", name)
			}
			if _, err := svc.GetEnv(ctx, &serverv1.GetEnvRequest{Session: name}); err == nil {
				t.Errorf("GetEnv(%q) error = nil, want a rejection", name)
			}

			// Kill reports per-name failures in the response rather than as a call error, so both have to be
			// checked. Reading only the error made an earlier version of this test report "accepted" for names
			// that were in fact refused.
			resp, err := svc.Kill(ctx, &serverv1.KillRequest{Sessions: []string{name}})
			if err != nil {
				return // a call error is a rejection
			}
			if _, rejected := resp.Errors[name]; !rejected {
				t.Errorf("Kill(%q) killed %v, want a rejection", name, resp.Killed)
			}
		})
	}
}

// Every hostile name is refused by Open, which is the one path that turns a name into a filename.
//
// TestOpenRejectsInvalidName already covers "../evil"; this covers the rest of the set, and asserts the
// consequence rather than only the error: that no socket was created for the name. Uses a nonexistent shim
// binary so the spawn cannot succeed, since validation happens before it and is what is being checked.
func TestOpenRejectsEveryHostileName(t *testing.T) {
	mgr, _, dirs := newTestManager(t, nil)
	mgr.selfExe = "/nonexistent/cm"
	ctx := context.Background()

	for _, name := range hostileNames {
		if name == "" {
			// Not hostile: an empty name means "allocate one", which TestOpenAllocatesNameWhenEmpty covers.
			continue
		}
		t.Run(nameForTest(name), func(t *testing.T) {
			_, _, err := mgr.Open(ctx, OpenOptions{Ref: name, Rows: 24, Cols: 80})
			if err == nil {
				t.Fatalf("Open(%q) error = nil, want the name refused before it becomes a filename", name)
			}
			// Refused by validation, not by the missing binary. Without this the test would pass for a name
			// that was accepted and merely failed to spawn.
			if strings.Contains(err.Error(), "/nonexistent/cm") {
				t.Errorf("Open(%q) failed at the spawn rather than at validation: %v", name, err)
			}
			if _, statErr := os.Stat(dirs.ShimSocket(name)); statErr == nil {
				t.Errorf("a socket exists for %q", name)
			}
		})
	}
}

// dirEntries returns a directory's entries as a comparable string.
func dirEntries(t *testing.T, dir string) string {
	t.Helper()
	return strings.Join(mustReadDir(t, dir), ",")
}

func mustReadDir(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// nameForTest makes a hostile name usable as a subtest name.
func nameForTest(name string) string {
	switch name {
	case "":
		return "empty"
	}
	r := strings.NewReplacer("/", "_slash_", "\t", "_tab_", "\n", "_nl_", "\x00", "_nul_", ".", "_dot_")
	s := r.Replace(name)
	if len(s) > 24 {
		return s[:24] + "_truncated"
	}
	return s
}
