package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSessionName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"simple", "work", false},
		{"kitty style", "kitty.55", false},
		{"dashes and underscores", "my-session_2", false},
		{"digits only", "12345", false},
		{"max length", strings.Repeat("a", MaxSessionNameLen), false},

		{"empty", "", true},
		{"too long", strings.Repeat("a", MaxSessionNameLen+1), true},
		{"dot", ".", true},
		{"dotdot", "..", true},
		{"leading dot", ".hidden", true},
		{"slash", "a/b", true},
		{"traversal", "../../etc/passwd", true},
		{"nul byte", "a\x00b", true},
		{"newline", "a\nb", true},
		{"space", "a b", true},
		{"shell metachar", "a;b", true},
		{"non-ascii", "sesión", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSessionName(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSessionName(%q) error = %v, wantErr %v",
					tt.input, err, tt.wantErr)
			}
		})
	}
}

// Any name that survives validation must produce paths inside the intended directories.
// This states the property the validator exists to guarantee: rejection is the only
// thing standing between a hostile name and an arbitrary socket or unlink target, since
// the path helpers themselves do no sanitizing.
func TestValidNamesProduceContainedPaths(t *testing.T) {
	d := Dirs{Runtime: "/run/cm", State: "/state/cm"}

	hostile := []string{"../evil", "a/../../evil", "/etc/passwd", "..", ".", "a\x00b"}
	for _, name := range hostile {
		if err := ValidateSessionName(name); err == nil {
			t.Errorf("ValidateSessionName(%q) = nil, want rejection: it would yield socket %q",
				name, d.ShimSocket(name))
		}
	}

	for _, name := range []string{"work", "kitty.55", "my-session_2"} {
		if err := ValidateSessionName(name); err != nil {
			t.Fatalf("ValidateSessionName(%q) = %v, want nil", name, err)
		}
		for _, got := range []string{d.ShimSocket(name), d.SessionLog(name)} {
			if filepath.Clean(got) != got {
				t.Errorf("path %q for name %q is not already clean", got, name)
			}
			if !strings.HasPrefix(got, "/run/cm/") && !strings.HasPrefix(got, "/state/cm/") {
				t.Errorf("path %q for name %q escapes both dirs", got, name)
			}
		}
	}
}

// An over-long socket path fails at bind with a bare EINVAL, so it is checked up front to
// produce an error that names the limit and suggests a fix.
func TestCheckSocketPath(t *testing.T) {
	if err := CheckSocketPath("/tmp/cm-501/shim-work.sock"); err != nil {
		t.Errorf("CheckSocketPath(short) = %v, want nil", err)
	}

	long := "/" + strings.Repeat("a", MaxSocketPathLen)
	err := CheckSocketPath(long)
	if err == nil {
		t.Fatalf("CheckSocketPath(%d bytes) = nil, want an error", len(long))
	}
	// The message has to be actionable: the failure mode it replaces was an opaque
	// "invalid argument" with no mention of length.
	for _, want := range []string{"limit", Env("RUNTIME_DIR")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A reference is a name unless it carries the sigil, and the sigil cannot appear in a name, so the two
// namespaces cannot overlap however a session is named.
func TestSessionRefSplitsIDsFromNames(t *testing.T) {
	tests := []struct {
		ref      string
		want     string
		wantIsID bool
	}{
		{"work", "work", false},
		{"@a7k2m9x4", "a7k2m9x4", true},
		// A digits-only name stays a name, which is the case the sigil exists for.
		{"7", "7", false},
		{"@7", "7", true},
		// Not special-cased: an empty reference stays empty and its own validator rejects it.
		{"", "", false},
		{"@", "", true},
		// Only the first sigil is the marker, so an ID is never silently rewritten.
		{"@@x", "@x", true},
	}
	for _, tt := range tests {
		got, gotIsID := SessionRef(tt.ref)
		if got != tt.want || gotIsID != tt.wantIsID {
			t.Errorf("SessionRef(%q) = %q, %v, want %q, %v",
				tt.ref, got, gotIsID, tt.want, tt.wantIsID)
		}
	}
}

// The sigil must be unusable in a name, or a name could be mistaken for an ID reference.
func TestValidateSessionNameRejectsTheIDSigil(t *testing.T) {
	if err := ValidateSessionName("work" + IDSigil); err == nil {
		t.Error("ValidateSessionName() accepted a name containing the ID sigil, want rejection")
	}
	if err := ValidateSessionName(IDSigil + "work"); err == nil {
		t.Error("ValidateSessionName() accepted a name starting with the ID sigil, want rejection")
	}
}

// Every name is shown, since showing only the first is what made an alias resolve while staying
// invisible in the listing. What overflow does matters as much: a cell must never contain half a name,
// because a reader is expected to type what they see straight back into another command.
func TestFormatNames(t *testing.T) {
	tests := []struct {
		desc   string
		names  []string
		budget int
		want   string
	}{
		{"nothing names it", nil, 32, ""},
		{"one name", []string{"work"}, 32, "work"},
		{"an alias beside the name a terminal invented", []string{"kitty.325", "refactor"}, 32,
			"kitty.325,refactor"},
		{"oldest first, whatever order they fit in", []string{"a", "b", "c"}, 32, "a,b,c"},
		{"a budget of zero means no bound", []string{"kitty.325", "refactor"}, 0, "kitty.325,refactor"},
		{"exactly the budget is not overflow", []string{"abcd", "efgh"}, 9, "abcd,efgh"},
		// One over, so the second name goes and the count says so rather than "abcd,efg".
		{"one over drops a whole name", []string{"abcd", "efgh"}, 8, "abcd +1"},
		{"several dropped are counted together", []string{"abcd", "efgh", "ijkl", "mnop"}, 12,
			"abcd,efgh +2"},
		// Nothing to drop, so no suffix: "+0" would say something is hidden when nothing is, and the
		// caller's own truncation is what bounds the cell.
		{"a single name over the budget is returned whole", []string{"averylongsessionname"}, 8,
			"averylongsessionname"},
		{"the first name is kept even when it does not fit", []string{"averylongsessionname", "x"}, 8,
			"averylongsessionname +1"},
	}
	for _, tt := range tests {
		if got := FormatNames(tt.names, tt.budget); got != tt.want {
			t.Errorf("FormatNames(%q, %d) = %q, want %q (%s)",
				tt.names, tt.budget, got, tt.want, tt.desc)
		}
	}
}

func TestValidateSessionID(t *testing.T) {
	valid := []string{"a7k2m9x4", "mig00001", "x", "0"}
	for _, id := range valid {
		if err := ValidateSessionID(id); err != nil {
			t.Errorf("ValidateSessionID(%q) error = %v, want nil", id, err)
		}
	}
	// Anything that could leave the runtime directory or was never generated by cm.
	invalid := []string{"", "../etc/passwd", "a/b", "Work", "a-b", "a.b", "@a7k2m9x4",
		strings.Repeat("a", MaxSessionIDLen+1)}
	for _, id := range invalid {
		if err := ValidateSessionID(id); err == nil {
			t.Errorf("ValidateSessionID(%q) = nil error, want rejection", id)
		}
	}
}
