package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/paths"
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

// Zero is accepted here where expire_after refuses it, and that difference is the point of the test.
//
// What zero would destroy is what separates them: for expire_after it deletes a session's record the moment
// it ends, which is never what someone means, while here it keeps a file that survives forever today. So
// "keep every shim log" has to be sayable, and only a negative duration is an error.
func TestKeepShimLogsForValidation(t *testing.T) {
	// Unset means the default rather than never.
	cfg := &Config{}
	got, err := cfg.KeepShimLogsFor()
	if err != nil || got != DefaultShimLogRetention {
		t.Errorf("KeepShimLogsFor() = (%v, %v), want (%v, nil)", got, err, DefaultShimLogRetention)
	}

	// Zero disables pruning rather than pruning everything, which is the opposite of what an inverted
	// reading would do: it must not delete every shim log the moment its shell exits.
	cfg = &Config{ShimLogRetention: "0"}
	got, err = cfg.KeepShimLogsFor()
	if err != nil || got != 0 {
		t.Errorf("KeepShimLogsFor(\"0\") = (%v, %v), want (0, nil)", got, err)
	}

	cfg = &Config{ShimLogRetention: "48h"}
	got, err = cfg.KeepShimLogsFor()
	if err != nil || got != 48*time.Hour {
		t.Errorf("KeepShimLogsFor(\"48h\") = (%v, %v), want (48h, nil)", got, err)
	}

	for _, spec := range []string{"-1h", "nonsense", "7"} {
		cfg = &Config{ShimLogRetention: spec}
		if _, err := cfg.KeepShimLogsFor(); err == nil {
			t.Errorf("KeepShimLogsFor(%q) = nil error, want a rejection", spec)
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

func TestResizePolicy(t *testing.T) {
	tests := []struct {
		spec    string
		want    string
		wantErr bool
	}{
		// Unset means leader, which is identical to the old behavior for a single client and only
		// differs once two are attached.
		{spec: "", want: ResizeLeader},
		{spec: "leader", want: ResizeLeader},
		{spec: "last-attach", want: ResizeLastAttach},
		{spec: "first-attach", want: ResizeFirstAttach},
		{spec: "smallest", want: ResizeSmallest},
		// A misspelling is reported rather than silently defaulting, since the difference is visible
		// as windows reflowing unexpectedly.
		{spec: "last", wantErr: true},
		{spec: "biggest", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			cfg := &Config{ResizePolicy: tt.spec}
			got, err := cfg.Resize()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Resize() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("Resize() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResizePolicyFromFile(t *testing.T) {
	cfg, err := Load(writeConfig(t, `resize_policy = "smallest"`+"\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got, err := cfg.Resize()
	if err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if got != ResizeSmallest {
		t.Errorf("Resize() = %q, want %q", got, ResizeSmallest)
	}
}

// DefaultPath honours XDG_CONFIG_HOME before falling back to os.UserConfigDir.
//
// This was a real bug. On macOS os.UserConfigDir returns ~/Library/Application Support and ignores
// XDG_CONFIG_HOME entirely, so a user who keeps dotfiles in ~/.config had their file silently not read: a
// missing config is not an error, so nothing reported it, and a detach_key set there simply did nothing.
//
// It was also inconsistent within cm, since paths.Default already honours XDG_RUNTIME_DIR and
// XDG_STATE_HOME. Found by `cm config` printing the path it looks at, which is most of why that command
// exists.
func TestDefaultPathHonoursXDGConfigHome(t *testing.T) {
	t.Setenv(paths.Env("CONFIG"), "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}
	want := filepath.Join("/xdg/config", paths.Name, paths.Name+".toml")
	if got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

// CM_CONFIG wins over XDG_CONFIG_HOME.
//
// The explicit override has to beat the convention, or a test harness and a one-off invocation cannot
// redirect cm on a machine that sets XDG_CONFIG_HOME. Every e2e test depends on this.
func TestDefaultPathPrefersTheExplicitOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	t.Setenv(paths.Env("CONFIG"), "/explicit/cm.toml")

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}
	if got != "/explicit/cm.toml" {
		t.Errorf("DefaultPath() = %q, want the explicit override", got)
	}
}

// With neither set, the platform's config directory is used.
//
// The fallback, so the XDG support above does not become a requirement: a machine with no XDG variables
// still finds a config in the conventional place for its platform.
func TestDefaultPathFallsBackToTheUserConfigDir(t *testing.T) {
	t.Setenv(paths.Env("CONFIG"), "")
	t.Setenv("XDG_CONFIG_HOME", "")

	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath() error = %v", err)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir on this machine: %v", err)
	}
	want := filepath.Join(dir, paths.Name, paths.Name+".toml")
	if got != want {
		t.Errorf("DefaultPath() = %q, want %q", got, want)
	}
}

// Zero keeps every snapshot, the same asymmetry shim_log_retention has and for the same reason.
//
// What zero would destroy separates it from expire_after: here it holds a file that a rollback needs, so
// "keep them all" has to be sayable, and only a negative duration is an error.
func TestKeepDatabaseBackupsForValidation(t *testing.T) {
	cfg := &Config{}
	got, err := cfg.KeepDatabaseBackupsFor()
	if err != nil || got != DefaultDatabaseBackupRetention {
		t.Errorf("KeepDatabaseBackupsFor() = (%v, %v), want (%v, nil)",
			got, err, DefaultDatabaseBackupRetention)
	}

	cfg = &Config{DatabaseBackupRetention: "0"}
	got, err = cfg.KeepDatabaseBackupsFor()
	if err != nil || got != 0 {
		t.Errorf("KeepDatabaseBackupsFor(\"0\") = (%v, %v), want (0, nil)", got, err)
	}

	cfg = &Config{DatabaseBackupRetention: "720h"}
	got, err = cfg.KeepDatabaseBackupsFor()
	if err != nil || got != 720*time.Hour {
		t.Errorf("KeepDatabaseBackupsFor(\"720h\") = (%v, %v), want (720h, nil)", got, err)
	}

	for _, spec := range []string{"-1h", "nonsense", "7"} {
		cfg = &Config{DatabaseBackupRetention: spec}
		if _, err := cfg.KeepDatabaseBackupsFor(); err == nil {
			t.Errorf("KeepDatabaseBackupsFor(%q) = nil error, want a rejection", spec)
		}
	}
}
