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
