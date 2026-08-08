package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cm.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// Configuration is optional, so a missing file must yield working defaults rather than an error.
func TestLoadMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load() error = %v, want nil for a missing file", err)
	}
	if got := cfg.Scrollback(); got != DefaultScrollbackLines {
		t.Errorf("Scrollback() = %d, want %d", got, DefaultScrollbackLines)
	}
	if len(cfg.EnvPatterns()) == 0 {
		t.Error("EnvPatterns() is empty with no config, want the built-in list")
	}
}

func TestLoadScrollback(t *testing.T) {
	cfg, err := Load(writeConfig(t, "scrollback_lines = 50000\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Scrollback(); got != 50000 {
		t.Errorf("Scrollback() = %d, want 50000", got)
	}
}

// Zero must mean unlimited rather than falling back to the default, so it has to be
// distinguishable from unset. That is why the field is a pointer.
func TestLoadScrollbackZeroMeansUnlimited(t *testing.T) {
	cfg, err := Load(writeConfig(t, "scrollback_lines = 0\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Scrollback(); got != 0 {
		t.Errorf("Scrollback() = %d, want 0 to mean unlimited", got)
	}
}

// A typo in a config file must be reported. Ignoring unknown keys makes a misspelled setting
// indistinguishable from one that has no effect.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	_, err := Load(writeConfig(t, "scrolback_lines = 100\n"))
	if err == nil {
		t.Fatal("Load() = nil error for an unknown key, want a report")
	}
	if !strings.Contains(err.Error(), "scrolback_lines") {
		t.Errorf("error = %q, want it to name the offending key", err)
	}
}

func TestLoadRejectsMalformedFile(t *testing.T) {
	if _, err := Load(writeConfig(t, "this is not toml [[[\n")); err == nil {
		t.Error("Load() = nil error for malformed TOML, want a parse failure")
	}
}

// Capture adds to the built-in list rather than replacing it, so adding one variable does not
// mean restating the defaults that make the feature work.
func TestEnvCaptureIsAdditive(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[env]
capture = ["MY_VAR", "CUSTOM_*"]
`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	patterns := cfg.EnvPatterns()
	for _, want := range []string{"MY_VAR", "CUSTOM_*", "TERM", "KITTY_*"} {
		if !slices.Contains(patterns, want) {
			t.Errorf("EnvPatterns() = %v, want it to contain %q", patterns, want)
		}
	}

	m := cfg.EnvMatcher()
	if !m.Match("MY_VAR") || !m.Match("CUSTOM_THING") || !m.Match("KITTY_LISTEN_ON") {
		t.Error("matcher does not cover both configured and built-in patterns")
	}
}

// Exclude has to be able to drop a built-in, which is the only way out for someone who does not
// want SSH_AUTH_SOCK recorded.
func TestEnvExcludeRemovesBuiltins(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[env]
exclude = ["SSH_AUTH_SOCK"]
`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.EnvMatcher().Match("SSH_AUTH_SOCK") {
		t.Error("SSH_AUTH_SOCK is still matched after being excluded")
	}
	// Other built-ins must survive.
	if !cfg.EnvMatcher().Match("KITTY_LISTEN_ON") {
		t.Error("excluding one pattern removed others")
	}
}

func TestEnvCaptureOnlyReplacesBuiltins(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[env]
capture_only = ["ONLY_THIS"]
`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	m := cfg.EnvMatcher()
	if !m.Match("ONLY_THIS") {
		t.Error("configured pattern is not matched")
	}
	if m.Match("KITTY_LISTEN_ON") || m.Match("TERM") {
		t.Error("capture_only did not replace the built-in list")
	}
}

func TestDefaultPathHonorsEnvOverride(t *testing.T) {
	t.Setenv("CM_CONFIG", "/custom/cm.toml")
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}
	if got != "/custom/cm.toml" {
		t.Errorf("DefaultPath() = %q, want %q", got, "/custom/cm.toml")
	}
}
