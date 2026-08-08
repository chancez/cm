package paths

import (
	"os"
	"path/filepath"
	"strings"
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

// DefaultWithOrigin reports which rule chose each directory.
//
// Reported because it cannot be recovered afterwards: every branch produces an absolute path, so a directory
// XDG chose and one from the built-in default are indistinguishable once resolved. `cm config` reported
// "default" for a path XDG_STATE_HOME had picked, which is a right value with a wrong explanation -- the same
// class of mistake as conflating a flag with an environment variable.
func TestDefaultWithOriginNamesTheRule(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want DirOrigin
	}{
		{
			name: "built-in defaults",
			env:  map[string]string{},
			want: DirOrigin{Runtime: OriginDefault, State: OriginDefault},
		},
		{
			name: "cm's own variables win",
			env: map[string]string{
				Env("RUNTIME_DIR"): "/explicit/run",
				Env("STATE_DIR"):   "/explicit/state",
				"XDG_RUNTIME_DIR":  "/xdg/run",
				"XDG_STATE_HOME":   "/xdg/state",
			},
			want: DirOrigin{Runtime: "$" + Env("RUNTIME_DIR"), State: "$" + Env("STATE_DIR")},
		},
		{
			name: "XDG variables when cm's are unset",
			env: map[string]string{
				"XDG_RUNTIME_DIR": "/xdg/run",
				"XDG_STATE_HOME":  "/xdg/state",
			},
			want: DirOrigin{Runtime: "$XDG_RUNTIME_DIR", State: "$XDG_STATE_HOME"},
		},
		{
			// Mixed, since the two directories resolve independently and a shared code path would hide it.
			name: "one from XDG, one defaulted",
			env:  map[string]string{"XDG_STATE_HOME": "/xdg/state"},
			want: DirOrigin{Runtime: OriginDefault, State: "$XDG_STATE_HOME"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Cleared first, so the ambient environment cannot decide the result.
			for _, k := range []string{Env("RUNTIME_DIR"), Env("STATE_DIR"), "XDG_RUNTIME_DIR", "XDG_STATE_HOME"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			_, got, err := DefaultWithOrigin()
			if err != nil {
				t.Fatalf("DefaultWithOrigin() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("origin = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Sockets stay under TMPDIR rather than moving to XDG_DATA_HOME.
//
// A deliberate decision, so it is asserted rather than left to drift. The runtime directory holds nothing but
// sockets, and an abandoned socket in a temp directory is swept for free where one in a persistent directory
// accumulates. XDG_DATA_HOME is for persistent application data, which is what the state directory already
// holds, so honouring it here would put sockets in the wrong place by the same spec that names them.
func TestRuntimeDefaultsUnderTempDir(t *testing.T) {
	for _, k := range []string{Env("RUNTIME_DIR"), "XDG_RUNTIME_DIR", "XDG_DATA_HOME"} {
		t.Setenv(k, "")
	}
	// Set, to prove it is ignored rather than merely absent.
	t.Setenv("XDG_DATA_HOME", "/xdg/data")

	d, origin, err := DefaultWithOrigin()
	if err != nil {
		t.Fatalf("DefaultWithOrigin() error = %v", err)
	}
	if strings.HasPrefix(d.Runtime, "/xdg/data") {
		t.Errorf("Runtime = %q, want it under the temp directory rather than XDG_DATA_HOME", d.Runtime)
	}
	if !strings.HasPrefix(d.Runtime, os.TempDir()) {
		t.Errorf("Runtime = %q, want it under %q", d.Runtime, os.TempDir())
	}
	if origin.Runtime != OriginDefault {
		t.Errorf("origin.Runtime = %q, want %q", origin.Runtime, OriginDefault)
	}
}

// Default and DefaultWithOrigin resolve to the same directories.
//
// Default delegates, so this guards against the two drifting if one is ever changed alone: a divergence would
// mean `cm config` reporting paths the rest of the program does not use, which is worse than no report.
func TestDefaultMatchesDefaultWithOrigin(t *testing.T) {
	plain, err := Default()
	if err != nil {
		t.Fatalf("Default() error = %v", err)
	}
	withOrigin, _, err := DefaultWithOrigin()
	if err != nil {
		t.Fatalf("DefaultWithOrigin() error = %v", err)
	}
	if plain != withOrigin {
		t.Errorf("Default() = %+v, DefaultWithOrigin() = %+v, want identical", plain, withOrigin)
	}
}
