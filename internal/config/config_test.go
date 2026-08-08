package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

func TestPersistConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[persist]
enabled = true
sessions = ["kitty.*", "work"]
on_restore = "command"
safe_commands = ["nvim", "less"]
max_lines = 5000
max_bytes = 1048576
expire_after = "24h"
`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if !cfg.Persist.Enabled {
		t.Error("Enabled = false, want true")
	}
	if !cfg.PersistsSession("kitty.55") {
		t.Error("kitty.55 does not persist, want the pattern to match")
	}
	if !cfg.PersistsSession("work") {
		t.Error("work does not persist, want the exact name to match")
	}
	if cfg.PersistsSession("scratch") {
		t.Error("scratch persists, want only configured names")
	}

	mode, err := cfg.RestoreMode()
	if err != nil {
		t.Fatalf("RestoreMode() error = %v", err)
	}
	if mode != RestoreCommand {
		t.Errorf("RestoreMode() = %q, want %q", mode, RestoreCommand)
	}

	limits := cfg.PersistLimits()
	if limits.MaxLines != 5000 || limits.MaxBytes != 1048576 {
		t.Errorf("PersistLimits() = %+v, want 5000 lines and 1048576 bytes", limits)
	}

	expire, err := cfg.ExpireAfter()
	if err != nil {
		t.Fatalf("ExpireAfter() error = %v", err)
	}
	if expire != 24*time.Hour {
		t.Errorf("ExpireAfter() = %v, want 24h", expire)
	}
}

// Nothing persists unless enabled, so a user who has not opted in never has output written to disk.
func TestPersistDisabledByDefault(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Persist.Enabled {
		t.Error("Enabled = true with no config, want false")
	}
	// Even a matching-looking name must not persist while disabled.
	if cfg.PersistsSession("kitty.1") {
		t.Error("a session persists with persistence disabled")
	}
}

// Patterns are only consulted when enabled, so leaving them configured but turning persistence off
// really does turn it off.
func TestPersistPatternsIgnoredWhenDisabled(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[persist]
enabled = false
sessions = ["kitty.*"]
`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.PersistsSession("kitty.55") {
		t.Error("kitty.55 persists although persistence is disabled")
	}
}

func TestRestoreModeDefaultAndValidation(t *testing.T) {
	// Unset means a fresh shell, the safe option.
	cfg := &Config{}
	mode, err := cfg.RestoreMode()
	if err != nil || mode != RestoreShell {
		t.Errorf("RestoreMode() = (%q, %v), want (shell, nil)", mode, err)
	}

	// A misspelling is reported rather than silently defaulting, since the difference between
	// "shell" and "command" is whether cm executes something.
	cfg = &Config{Persist: PersistConfig{OnRestore: "comand"}}
	if _, err := cfg.RestoreMode(); err == nil {
		t.Error("RestoreMode() = nil error for a misspelling, want a rejection")
	}
}

func TestExpireAfterValidation(t *testing.T) {
	// Unset means the default rather than never.
	cfg := &Config{}
	got, err := cfg.ExpireAfter()
	if err != nil || got != DefaultExpireAfter {
		t.Errorf("ExpireAfter() = (%v, %v), want (%v, nil)", got, err, DefaultExpireAfter)
	}

	// Zero would mean "expire immediately", which nobody means by writing it, so it is refused
	// rather than deleting sessions the moment they die.
	for _, spec := range []string{"0s", "-1h", "nonsense"} {
		cfg = &Config{Persist: PersistConfig{ExpireAfter: spec}}
		if _, err := cfg.ExpireAfter(); err == nil {
			t.Errorf("ExpireAfter(%q) = nil error, want a rejection", spec)
		}
	}
}

// The allowlist matches the program name only, which is why it is documented as a convenience. The
// test pins that behavior so the limitation is not mistaken for a bug later.
func TestCommandIsSafeToRerun(t *testing.T) {
	cfg := &Config{Persist: PersistConfig{SafeCommands: []string{"nvim", "less"}}}

	tests := []struct {
		argv []string
		want bool
	}{
		{[]string{"nvim", "notes.md"}, true},
		{[]string{"/usr/bin/nvim", "notes.md"}, true},
		{[]string{"less", "file"}, true},
		{[]string{"make", "install"}, false},
		{[]string{}, false},
		{nil, false},
		// Documented limitation: an allowlisted program with hostile arguments still matches, since
		// only the name is compared.
		{[]string{"nvim", "-c", ":!rm -rf /tmp/x"}, true},
	}
	for _, tt := range tests {
		if got := cfg.CommandIsSafeToRerun(tt.argv); got != tt.want {
			t.Errorf("CommandIsSafeToRerun(%v) = %v, want %v", tt.argv, got, tt.want)
		}
	}
}

func TestPersistLimitsDefaults(t *testing.T) {
	cfg := &Config{}
	limits := cfg.PersistLimits()
	if limits.MaxLines <= 0 || limits.MaxBytes <= 0 {
		t.Errorf("PersistLimits() = %+v, want both bounds defaulted so neither is unlimited", limits)
	}
}
