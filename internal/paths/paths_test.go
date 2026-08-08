package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultUsesEnvOverrides(t *testing.T) {
	t.Setenv(Env("RUNTIME_DIR"), "/custom/run")
	t.Setenv(Env("STATE_DIR"), "/custom/state")

	got, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	want := Dirs{Runtime: "/custom/run", State: "/custom/state"}
	if got != want {
		t.Errorf("Default() = %+v, want %+v", got, want)
	}
}

func TestDefaultPrefersXDG(t *testing.T) {
	t.Setenv(Env("RUNTIME_DIR"), "")
	t.Setenv(Env("STATE_DIR"), "")
	t.Setenv("XDG_RUNTIME_DIR", "/xdg/run")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")

	got, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	want := Dirs{
		Runtime: filepath.Join("/xdg/run", Name),
		State:   filepath.Join("/xdg/state", Name),
	}
	if got != want {
		t.Errorf("Default() = %+v, want %+v", got, want)
	}
}

// Without XDG_RUNTIME_DIR the runtime directory lands under TMPDIR, and must be
// qualified by uid. A shared path would collide between users, letting one user's
// socket shadow another's.
func TestDefaultRuntimeIsUIDQualified(t *testing.T) {
	t.Setenv(Env("RUNTIME_DIR"), "")
	t.Setenv("XDG_RUNTIME_DIR", "")

	got, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	if base := filepath.Base(got.Runtime); base == Name {
		t.Errorf("Default().Runtime = %q, want a uid suffix so users cannot collide", got.Runtime)
	}
}

func TestEnsureCreatesOwnerOnlyDirs(t *testing.T) {
	root := t.TempDir()
	d := Dirs{
		Runtime: filepath.Join(root, "run"),
		State:   filepath.Join(root, "state"),
	}
	if err := d.Ensure(); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	// Sockets in these directories grant control of a shell, so the mode is the
	// access control and worth asserting rather than assuming.
	for _, dir := range []string{d.Runtime, d.State, d.logDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %o, want 700", dir, perm)
		}
	}
}
