package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The round trip a layout takes: saved from a file, read back byte for byte, and saved again only on the
// revision read, with the losing write exiting 2 rather than 1 so a caller can tell it from a failure.
func TestSpaceMetaRoundTrip(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// Bytes that are not text, and a trailing newline, both of which get must print unchanged.
	value := "tab\tnul\x00line\n"
	file := filepath.Join(t.TempDir(), "layout")
	if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	out := e.mustRun("space", "meta", "set", "cm", "kspaces/layout/main", "--file", file, "--if-revision", "0")
	rev, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		t.Fatalf("set printed %q, want a revision", out)
	}

	if got := e.mustRun("space", "meta", "get", "cm", "kspaces/layout/main"); got != value {
		t.Errorf("get = %q, want %q", got, value)
	}

	var entry struct {
		Value    []byte `json:"value"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal([]byte(e.mustRun("space", "meta", "get", "cm", "kspaces/layout/main", "--json")), &entry); err != nil {
		t.Fatalf("parsing get --json: %v", err)
	}
	if string(entry.Value) != value || entry.Revision != rev {
		t.Errorf("get --json = %q at %d, want %q at %d", entry.Value, entry.Revision, value, rev)
	}

	// A second machine that read nothing loses to the first.
	if r := e.run("space", "meta", "set", "cm", "kspaces/layout/main", "other", "--if-revision", "0"); r.code != 2 {
		t.Errorf("conflicting set exited %d, want 2: %s%s", r.code, r.stdout, r.stderr)
	}
	e.mustRun("space", "meta", "set", "cm", "kspaces/layout/main", "newer", "--if-revision", strconv.FormatInt(rev, 10))
	if got := e.mustRun("space", "meta", "get", "cm", "kspaces/layout/main"); got != "newer" {
		t.Errorf("get after a conditional set = %q, want %q", got, "newer")
	}

	if got, want := e.mustRun("space", "meta", "list", "cm"), "kspaces/layout/main"; !strings.Contains(got, want) {
		t.Errorf("list = %q, want it to name %s", got, want)
	}
	e.mustRun("space", "meta", "remove", "cm", "kspaces/layout/main")
	if r := e.run("space", "meta", "get", "cm", "kspaces/layout/main"); r.code != 2 {
		t.Errorf("get after remove exited %d, want 2: %s%s", r.code, r.stdout, r.stderr)
	}
	// A mistyped space is an error, not a space with nothing saved.
	if r := e.run("space", "meta", "get", "nope", "k"); r.code != 1 {
		t.Errorf("get from a missing space exited %d, want 1: %s%s", r.code, r.stdout, r.stderr)
	}
}
