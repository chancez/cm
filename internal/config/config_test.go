package config

import (
	"github.com/chancez/cm/internal/remote"
	"os"
	"path/filepath"
	"reflect"
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
	if got := cfg.ScrollbackLimit(); got != DefaultScrollbackLines {
		t.Errorf("ScrollbackLimit() = %d, want %d", got, DefaultScrollbackLines)
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
	if got := cfg.ScrollbackLimit(); got != 50000 {
		t.Errorf("ScrollbackLimit() = %d, want 50000", got)
	}
}

// [scrollback] lines is the current spelling, and wins over scrollback_lines when a file has both.
func TestLoadScrollbackTable(t *testing.T) {
	tests := []struct {
		name string
		file string
		want int
	}{
		{name: "the table", file: "[scrollback]\nlines = 300\n", want: 300},
		{name: "the table wins", file: "scrollback_lines = 100\n[scrollback]\nlines = 300\n", want: 300},
		{name: "the table without lines leaves the old spelling", file: "scrollback_lines = 100\n[scrollback]\npager = \"less\"\n", want: 100},
		{name: "zero in the table is unlimited", file: "scrollback_lines = 100\n[scrollback]\nlines = 0\n", want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tc.file))
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if got := cfg.ScrollbackLimit(); got != tc.want {
				t.Errorf("ScrollbackLimit() = %d, want %d", got, tc.want)
			}
			if got := cfg.UnknownSettings(); len(got) != 0 {
				t.Errorf("UnknownSettings() = %q, want none", got)
			}
		})
	}
}

// Zero must mean unlimited rather than falling back to the default, so it has to be
// distinguishable from unset. That is why the field is a pointer.
func TestLoadScrollbackZeroMeansUnlimited(t *testing.T) {
	cfg, err := Load(writeConfig(t, "scrollback_lines = 0\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.ScrollbackLimit(); got != 0 {
		t.Errorf("ScrollbackLimit() = %d, want 0 to mean unlimited", got)
	}
}

// An unknown setting is recorded and the rest of the file still applies.
//
// Refusing is what it used to do, and `cm upgrade` made that an outage: the old server is stopped first,
// so one setting the new build did not know left no server at all and every attached client hanging. The
// caller decides now, and only `cm config` still fails. See UnknownSettings.
//
// The whole Config is asserted, because the load has to be otherwise indistinguishable from a clean one:
// a version that dropped the known settings along with the unknown one would pass any check that only
// looked at the report.
func TestLoadRecordsUnknownSettings(t *testing.T) {
	path := writeConfig(t, "detach_key = \"ctrl-o\"\nscrolback_lines = 100\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want an unknown setting to be recorded rather than refused", err)
	}
	want := &Config{path: path, files: []string{path}, unknown: []string{"scrolback_lines"}, DetachKey: "ctrl-o"}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
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

// With neither set, the platform's config directory is used, unless only ~/.config holds a file.
//
// The fallback, so the XDG support above does not become a requirement: a machine with no XDG variables
// still finds a config in the conventional place for its platform. ~/.config is the second place, for a
// process started without the shell's environment: kitty launched from the Dock runs `cm attach` with no
// XDG_CONFIG_HOME, and those clients read no file while `cm config` in a shell reported the right one.
// HOME is a temporary directory, so the developer's own config cannot decide the result.
func TestDefaultPathFallsBackToTheUserConfigDir(t *testing.T) {
	tests := []struct {
		name string
		// present are the candidate files that exist: "platform" and "dotconfig".
		present []string
		want    string
	}{
		{name: "neither exists", want: "platform"},
		{name: "only the platform directory", present: []string{"platform"}, want: "platform"},
		{name: "only ~/.config", present: []string{"dotconfig"}, want: "dotconfig"},
		{name: "both, the platform directory wins", present: []string{"platform", "dotconfig"}, want: "platform"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(paths.Env("CONFIG"), "")
			t.Setenv("XDG_CONFIG_HOME", "")
			dir, err := os.UserConfigDir()
			if err != nil {
				t.Skipf("no user config dir on this machine: %v", err)
			}
			candidates := map[string]string{
				"platform":  filepath.Join(dir, paths.Name, paths.Name+".toml"),
				"dotconfig": filepath.Join(home, ".config", paths.Name, paths.Name+".toml"),
			}
			for _, name := range tc.present {
				path := candidates[name]
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			got, err := DefaultPath()
			if err != nil {
				t.Fatalf("DefaultPath() error = %v", err)
			}
			if got != candidates[tc.want] {
				t.Errorf("DefaultPath() = %q, want %q", got, candidates[tc.want])
			}
		})
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

// The remote settings come out of the file, and connection_persist's zero is a real answer rather than an
// absent one: it means do not share a connection, which is what someone on a host where ControlMaster is
// unwelcome needs to be able to say.
func TestRemoteSettings(t *testing.T) {
	tests := []struct {
		name        string
		toml        string
		wantCommand string
		wantPersist time.Duration
		wantErr     bool
	}{
		{
			name:        "unset",
			toml:        "",
			wantCommand: "",
			wantPersist: remote.DefaultControlPersist,
		},
		{
			name:        "both",
			toml:        "[remote]\nssh_command = \"kitten ssh\"\nconnection_persist = \"5m\"\n",
			wantCommand: "kitten ssh",
			wantPersist: 5 * time.Minute,
		},
		{
			name:        "sharing off",
			toml:        "[remote]\nconnection_persist = \"0\"\n",
			wantPersist: 0,
		},
		{
			name:    "not a duration",
			toml:    "[remote]\nconnection_persist = \"soon\"\n",
			wantErr: true,
		},
		{
			name:    "negative",
			toml:    "[remote]\nconnection_persist = \"-1m\"\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cm.toml")
			if err := os.WriteFile(path, []byte(tt.toml), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v, want nil", err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			// A setting this build does not know is a warning rather than an error, so an unknown key would
			// pass every assertion below. Checked so a renamed field is caught here.
			if unknown := cfg.UnknownSettings(); len(unknown) != 0 {
				t.Errorf("UnknownSettings() = %q, want none", unknown)
			}

			persist, err := cfg.RemoteConnectionPersist()
			if (err != nil) != tt.wantErr {
				t.Fatalf("RemoteConnectionPersist() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if persist != tt.wantPersist {
				t.Errorf("RemoteConnectionPersist() = %v, want %v", persist, tt.wantPersist)
			}
			if cfg.Remote.SSHCommand != tt.wantCommand {
				t.Errorf("Remote.SSHCommand = %q, want %q", cfg.Remote.SSHCommand, tt.wantCommand)
			}
		})
	}
}

// The window only means something with the recorder on, so it is zero otherwise, whatever it says. That is
// what keeps a window left in the file after turning the recorder off from starting one.
func TestFlightWindow(t *testing.T) {
	for _, tc := range []struct {
		debug DebugConfig
		want  time.Duration
	}{
		{DebugConfig{}, 0},
		{DebugConfig{FlightRecorderWindow: "30s"}, 0},
		{DebugConfig{FlightRecorder: true}, DefaultFlightRecorderWindow},
		{DebugConfig{FlightRecorder: true, FlightRecorderWindow: "30s"}, 30 * time.Second},
	} {
		cfg := &Config{Debug: tc.debug}
		if got, err := cfg.FlightWindow(); err != nil || got != tc.want {
			t.Errorf("FlightWindow(%+v) = (%v, %v), want (%v, nil)", tc.debug, got, err, tc.want)
		}
	}
	for _, spec := range []string{"0", "-1s", "nonsense"} {
		cfg := &Config{Debug: DebugConfig{FlightRecorder: true, FlightRecorderWindow: spec}}
		if _, err := cfg.FlightWindow(); err == nil {
			t.Errorf("FlightWindow(%q) = nil error, want a rejection", spec)
		}
	}
}

func TestClosePolicyDefaultAndValidation(t *testing.T) {
	type policy struct {
		idle, busy string
	}
	for _, tc := range []struct {
		name string
		cfg  CloseConfig
		want policy
		err  bool
	}{
		// Unset keeps what runs and shelves what is idle, the choice that loses nothing.
		{name: "unset", cfg: CloseConfig{}, want: policy{"shelve", "detach"}},
		{name: "kill both", cfg: CloseConfig{Idle: "kill", Busy: "kill"}, want: policy{"kill", "kill"}},
		// A misspelling is refused rather than defaulted, since one side of it ends sessions.
		{name: "bad idle", cfg: CloseConfig{Idle: "kil"}, err: true},
		{name: "bad busy", cfg: CloseConfig{Busy: "shelve"}, err: true},
	} {
		cfg := &Config{Close: tc.cfg}
		idle, busy, err := cfg.ClosePolicy()
		if (err != nil) != tc.err {
			t.Errorf("%s: ClosePolicy() error = %v, want error %v", tc.name, err, tc.err)
			continue
		}
		if got := (policy{idle, busy}); !tc.err && got != tc.want {
			t.Errorf("%s: ClosePolicy() = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestRemotes(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[remote]
local_name = "laptop"
ssh_command = "kitten ssh"
connection_persist = "30s"

[remote.hosts.white]

[remote.hosts.server]
address = "ssh://me@server.lan:2222/home/me/.local/bin/cm"
ssh_command = "ssh -F /etc/other"
connection_persist = "0s"
`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	got, err := cfg.Remotes()
	if err != nil {
		t.Fatalf("Remotes() error = %v", err)
	}
	want := []NamedRemote{
		{
			Name:              "server",
			Target:            remote.Target{User: "me", Host: "server.lan", Port: 2222, Command: "/home/me/.local/bin/cm"},
			SSHCommand:        "ssh -F /etc/other",
			ConnectionPersist: 0,
		},
		// An empty table is the name as an ssh alias, with [remote]'s settings.
		{
			Name:              "white",
			Target:            remote.Target{Host: "white"},
			SSHCommand:        "kitten ssh",
			ConnectionPersist: 30 * time.Second,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Remotes() = %+v\nwant %+v", got, want)
	}

	r, ok, err := cfg.LookupRemote("white")
	if err != nil || !ok || !reflect.DeepEqual(r, want[1]) {
		t.Errorf("LookupRemote(white) = %+v, %v, %v; want %+v", r, ok, err, want[1])
	}
	if _, ok, err := cfg.LookupRemote("elsewhere"); err != nil || ok {
		t.Errorf("LookupRemote(elsewhere) = %v, %v; want not found", ok, err)
	}
	if name, err := cfg.LocalName(); err != nil || name != "laptop" {
		t.Errorf("LocalName() = %q, %v; want laptop", name, err)
	}
}

func TestRemotesRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "a name with a colon", body: "[remote.hosts.\"a:b\"]\n"},
		{name: "a bad address", body: "[remote.hosts.x]\naddress = \"http://x\"\n"},
		{name: "a bad persist", body: "[remote.hosts.x]\nconnection_persist = \"soon\"\n"},
		{name: "a bad local name", body: "[remote]\nlocal_name = \"a:b\"\n"},
	} {
		cfg, err := Load(writeConfig(t, tc.body))
		if err != nil {
			t.Fatalf("%s: Load() error = %v", tc.name, err)
		}
		if got, err := cfg.Remotes(); err == nil {
			t.Errorf("%s: Remotes() = %+v, want an error", tc.name, got)
		}
	}
}

// With nothing configured, a machine answers to its short hostname, lowercased.
func TestLocalNameDefaultsToTheHostname(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no hostname")
	}
	short, _, _ := strings.Cut(host, ".")
	if got, err := cfg.LocalName(); err != nil || got != strings.ToLower(short) {
		t.Errorf("LocalName() = %q, %v; want %q", got, err, strings.ToLower(short))
	}
}

// configHome makes a default config location under a temp XDG_CONFIG_HOME with the given files, cm.toml and
// conf.d/NAME.toml keyed by their path under cm's directory.
func configHome(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv(paths.Env("CONFIG"), "")
	dir := filepath.Join(home, paths.Name)
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// One cm.toml shared between machines and a conf.d file of this machine's own: tables merge key by key, so a
// drop-in that sets one key of a host keeps the address the shared file gave it, and later files win.
func TestLoadDefaultMergesConfD(t *testing.T) {
	dir := configHome(t, map[string]string{
		"cm.toml": `
[close]
idle = "kill"
busy = "kill"

[remote.hosts.laptop]
[remote.hosts.server]
address = "ssh://me@server.lan"
`,
		"conf.d/local.toml": `
[remote]
local_name = "laptop"

[remote.hosts.server]
ssh_command = "kitten ssh"

[close]
busy = "detach"
`,
		// Name order, so this one applies last.
		"conf.d/zz.toml": "[close]\nbusy = \"kill\"\nidle = \"shelve\"\n",
		// Not a .toml, so not read.
		"conf.d/notes.txt": "[close]\nidle = \"nonsense\"\n",
	})
	cfg, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault() error = %v", err)
	}

	wantFiles := []string{
		filepath.Join(dir, "cm.toml"),
		filepath.Join(dir, "conf.d", "local.toml"),
		filepath.Join(dir, "conf.d", "zz.toml"),
	}
	if got := cfg.Files(); !reflect.DeepEqual(got, wantFiles) {
		t.Errorf("Files() = %q, want %q", got, wantFiles)
	}
	if idle, busy, err := cfg.ClosePolicy(); err != nil || idle != "shelve" || busy != "kill" {
		t.Errorf("ClosePolicy() = %q, %q, %v; want shelve, kill", idle, busy, err)
	}
	// laptop is this machine, so the shared list naming it leaves one remote.
	remotes, err := cfg.Remotes()
	if err != nil {
		t.Fatalf("Remotes() error = %v", err)
	}
	want := []NamedRemote{{
		Name:              "server",
		Target:            remote.Target{User: "me", Host: "server.lan"},
		SSHCommand:        "kitten ssh",
		ConnectionPersist: remote.DefaultControlPersist,
	}}
	if !reflect.DeepEqual(remotes, want) {
		t.Errorf("Remotes() = %+v\nwant %+v", remotes, want)
	}
	if len(cfg.UnknownSettings()) != 0 {
		t.Errorf("UnknownSettings() = %q, want none", cfg.UnknownSettings())
	}
}

// A list in a drop-in replaces the shared one rather than appending, or nothing could take an entry out.
func TestConfDReplacesAList(t *testing.T) {
	configHome(t, map[string]string{
		"cm.toml":           "[env]\ncapture = [\"A\", \"B\"]\n",
		"conf.d/local.toml": "[env]\ncapture = [\"C\"]\n",
	})
	cfg, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault() error = %v", err)
	}
	if got := cfg.Env.Capture; !reflect.DeepEqual(got, []string{"C"}) {
		t.Errorf("env.capture = %q, want [C]", got)
	}
}

// A machine with only its own file, and no shared one, still reads it.
func TestConfDWithoutCMToml(t *testing.T) {
	dir := configHome(t, map[string]string{"conf.d/local.toml": "[remote]\nlocal_name = \"desk\"\n"})
	cfg, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault() error = %v", err)
	}
	if name, err := cfg.LocalName(); err != nil || name != "desk" {
		t.Errorf("LocalName() = %q, %v; want desk", name, err)
	}
	if got, want := cfg.Files(), []string{filepath.Join(dir, "conf.d", "local.toml")}; !reflect.DeepEqual(got, want) {
		t.Errorf("Files() = %q, want %q", got, want)
	}
}

// An unknown setting in a drop-in is reported with the file it is in.
func TestConfDUnknownSettingNamesItsFile(t *testing.T) {
	configHome(t, map[string]string{
		"cm.toml":           "typo_main = 1\n",
		"conf.d/local.toml": "typo_local = 1\n",
	})
	cfg, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault() error = %v", err)
	}
	want := []string{"typo_main", "conf.d/local.toml:typo_local"}
	if got := cfg.UnknownSettings(); !reflect.DeepEqual(got, want) {
		t.Errorf("UnknownSettings() = %q, want %q", got, want)
	}
}

// CM_CONFIG names one file and reads nothing beside it. A sandbox's CM_CONFIG=/nonexistent.toml would
// otherwise read /conf.d.
func TestConfigEnvReadsNoConfD(t *testing.T) {
	dir := configHome(t, map[string]string{
		"cm.toml":           "[close]\nidle = \"kill\"\n",
		"conf.d/local.toml": "[close]\nidle = \"shelve\"\n",
	})
	t.Setenv(paths.Env("CONFIG"), filepath.Join(dir, "cm.toml"))
	cfg, err := LoadDefault()
	if err != nil {
		t.Fatalf("LoadDefault() error = %v", err)
	}
	if idle, _, err := cfg.ClosePolicy(); err != nil || idle != "kill" {
		t.Errorf("ClosePolicy() idle = %q, %v; want kill, from cm.toml alone", idle, err)
	}
	if got, want := cfg.Files(), []string{filepath.Join(dir, "cm.toml")}; !reflect.DeepEqual(got, want) {
		t.Errorf("Files() = %q, want %q", got, want)
	}
}
