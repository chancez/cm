package shquote

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// awkward holds the arguments a quoting bug loses: a space, both quotes, a backslash pair, a dollar sign,
// a glob, a leading dash, and an empty string.
var awkward = []string{"printf", "%s\\n", "a b", "it's", `say "hi"`, `a\\b`, "$HOME", "*", "-n", ""}

// Each dialect is checked by the shell it is for, rather than against an expected string: what matters is
// that the shell parses the line back to the same argv, and a hand-written expectation would encode the
// same misunderstanding as the code.
func TestJoinRoundTripsThroughTheShell(t *testing.T) {
	for _, tt := range []struct {
		shell string
		fish  bool
	}{
		{"sh", false},
		{"bash", false},
		{"zsh", false},
		{"fish", true},
	} {
		t.Run(tt.shell, func(t *testing.T) {
			path, err := exec.LookPath(tt.shell)
			if err != nil {
				t.Skipf("%s not installed", tt.shell)
			}
			line := Join(awkward, tt.fish)
			out, err := exec.Command(path, "-c", line).Output()
			if err != nil {
				t.Fatalf("%s -c %q: %v", tt.shell, line, err)
			}
			got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
			if want := awkward[2:]; !reflect.DeepEqual(got, want) {
				t.Errorf("%s parsed %q as %q, want %q", tt.shell, line, got, want)
			}
		})
	}
}

// A word no shell treats specially is left alone, so a declared command reads as it was typed.
func TestJoinLeavesPlainWordsUnquoted(t *testing.T) {
	if got, want := Join([]string{"claude", "--resume", "1f2e-3d"}, false), "claude --resume 1f2e-3d"; got != want {
		t.Errorf("Join() = %q, want %q", got, want)
	}
}
