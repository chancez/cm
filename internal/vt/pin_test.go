package vt

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The libghostty cm builds has to be the one the Go binding was written against.
//
// Two pins name it: GHOSTTY_REF in mise.toml, which every build site reads, and the GIT_TAG in the binding's
// own CMakeLists.txt, which moves when go.mod does. Bumping one and not the other compiles against a C API
// the binding does not expect, and depending on what changed that is a link error, a crash, or a field read
// at the wrong offset.
func TestGhosttyRefMatchesBinding(t *testing.T) {
	toml, err := os.ReadFile(filepath.Join("..", "..", "mise.toml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^GHOSTTY_REF = "([0-9a-f]+)"$`).FindSubmatch(toml)
	if m == nil {
		t.Fatal("no GHOSTTY_REF in mise.toml")
	}
	ours := string(m[1])

	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "go.mitchellh.com/libghostty").Output()
	if err != nil {
		t.Fatalf("locating the binding: %v", err)
	}
	cmake, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)), "CMakeLists.txt"))
	if err != nil {
		t.Fatal(err)
	}
	b := regexp.MustCompile(`GIT_TAG\s+([0-9a-f]+)`).FindSubmatch(cmake)
	if b == nil {
		t.Fatal("no GIT_TAG in the binding's CMakeLists.txt")
	}
	if theirs := string(b[1]); ours != theirs {
		t.Errorf("mise.toml pins libghostty %s, the binding is written against %s; set GHOSTTY_REF to match",
			ours, theirs)
	}
}
