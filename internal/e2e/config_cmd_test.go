package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// configResult is the JSON shape `cm config` prints.
type configResult struct {
	File            string            `json:"file"`
	FileExists      bool              `json:"file_exists"`
	RuntimeDir      string            `json:"runtime_dir"`
	StateDir        string            `json:"state_dir"`
	Sources         map[string]string `json:"sources"`
	ScrollbackLines int               `json:"scrollback_lines"`
	ResizePolicy    string            `json:"resize_policy"`
	DetachKey       string            `json:"detach_key"`
	LogLevel        string            `json:"log_level"`
	RestoreMode     string            `json:"restore_mode"`
	UnknownSettings []string          `json:"unknown_settings"`
}

func (e *env) cmConfig() configResult {
	e.t.Helper()

	out := e.mustRun("config", "--json")
	var c configResult
	if err := json.Unmarshal([]byte(out), &c); err != nil {
		e.t.Fatalf("parsing config output %q: %v", out, err)
	}
	return c
}

// `cm config` reports resolved values, with defaults applied, not what the file happens to contain.
//
// That is the question worth answering: a mistyped setting and an absent one look identical in a file, so
// echoing the file back would confirm nothing. Every value here comes through the same accessors the running
// code uses.
func TestConfigReportsResolvedDefaults(t *testing.T) {
	skipIfShort(t)
	// newEnv writes no config file, which is the default path a new user is on.
	e := newEnv(t)

	got := e.cmConfig()
	if got.FileExists {
		t.Errorf("file_exists = true with no config written; file = %q", got.File)
	}
	// Defaults are reported rather than blanks, which is the whole point.
	if got.ScrollbackLines == 0 {
		t.Error("scrollback_lines = 0, want the default applied")
	}
	if got.ResizePolicy == "" {
		t.Error("resize_policy is empty, want the default applied")
	}
	if got.DetachKey == "" {
		t.Error("detach_key is empty, want the default applied")
	}
	if got.RestoreMode == "" {
		t.Error("restore_mode is empty, want the default applied")
	}
}

// Settings from the file are reflected, and the file is reported as present.
//
// The other half: with a file, its values must win over the defaults, or the command would be reassuring and
// wrong.
func TestConfigReflectsTheFile(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.writeConfig("scrollback_lines = 12345\ndetach_key = \"ctrl-o\"\nresize_policy = \"smallest\"\n")

	got := e.cmConfig()
	if !got.FileExists {
		t.Errorf("file_exists = false after writing one to %q", got.File)
	}
	if got.ScrollbackLines != 12345 {
		t.Errorf("scrollback_lines = %d, want 12345 from the file", got.ScrollbackLines)
	}
	if got.DetachKey != "ctrl-o" {
		t.Errorf("detach_key = %q, want ctrl-o from the file", got.DetachKey)
	}
	if got.ResizePolicy != "smallest" {
		t.Errorf("resize_policy = %q, want smallest from the file", got.ResizePolicy)
	}
}

// The report names where the directories came from.
//
// The precedence is the interesting part when cm is looking somewhere unexpected. The harness sets both
// directories through the environment, so that is what must be reported: a defaulted value and one the user
// set are otherwise indistinguishable, which is the bug that made this worth printing.
func TestConfigReportsWhereDirectoriesCameFrom(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	got := e.cmConfig()
	if got.RuntimeDir != e.runtime {
		t.Errorf("runtime_dir = %q, want %q", got.RuntimeDir, e.runtime)
	}
	if got.StateDir != e.state {
		t.Errorf("state_dir = %q, want %q", got.StateDir, e.state)
	}
	// The harness exports CM_RUNTIME_DIR and CM_STATE_DIR, so the source is the environment.
	for _, key := range []string{"runtime_dir", "state_dir"} {
		if src := got.Sources[key]; !strings.HasPrefix(src, "$CM_") {
			t.Errorf("sources[%s] = %q, want the environment variable that set it", key, src)
		}
	}
}

// A setting this build does not know must not stop a server from starting.
//
// The outage it caused: `cm upgrade` stops the running server before starting the replacement, so a
// config file naming a setting the new build lacks left no server at all, with 36 sessions holding live
// shells and every attached client waiting on a server that could never come up. One file serves every
// build on a machine, so a setting one branch knows and another does not is ordinary rather than exotic.
//
// A restart is what reads the file, since the server the harness started predates it, and it is the same
// call `cm upgrade` makes.
func TestServerStartsDespiteAnUnknownSetting(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// A known setting beside the unknown one, because the rest of the file still has to apply.
	e.writeConfig("detach_key = \"ctrl-o\"\nsetting_from_another_build = \"ctrl-]\"\n")

	if r := e.run("server", "restart"); r.code != 0 {
		t.Fatalf("`cm server restart` exit code = %d with an unknown setting, want 0\nstderr: %s",
			r.code, r.stderr)
	}
	// Serving, rather than merely having started: the failure this guards against was a client hanging.
	e.mustRun("list")

	// And the setting next to it took effect, so tolerating one is not ignoring the file.
	r := e.run("config", "--json")
	var got configResult
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("parsing config output %q: %v", r.stdout, err)
	}
	if got.DetachKey != "ctrl-o" {
		t.Errorf("detach_key = %q, want ctrl-o: the known settings in the file still apply", got.DetachKey)
	}
	if !slices.Equal(got.UnknownSettings, []string{"setting_from_another_build"}) {
		t.Errorf("unknown_settings = %v, want the one setting this build does not know",
			got.UnknownSettings)
	}
	// `cm config` is the one command that still fails on it, since a person reading this report is asking
	// why a setting does nothing and every other command has only warned.
	if r.code == 0 {
		t.Errorf("`cm config` exit code = 0 with an unknown setting, want non-zero\nstdout: %s", r.stdout)
	}
}

// A malformed config is an error rather than a silent fallback.
//
// Same reasoning as everywhere else in cm: falling back to defaults would leave a user wondering why their
// settings do nothing, which is precisely the confusion this command exists to end.
func TestConfigRejectsAMalformedFile(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.writeConfig("this is not = valid toml [[[\n")

	r := e.run("config")
	if r.code == 0 {
		t.Errorf("exit code = 0 for a malformed config, want non-zero\nstdout: %s", r.stdout)
	}
}

// The text output is readable and names the file.
//
// The JSON is for scripts; a person running this wants to see the path first, since a file in the wrong place
// is the most common reason a setting appears to do nothing.
func TestConfigTextNamesTheFile(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	out := e.mustRun("config")
	if !strings.Contains(out, "file") {
		t.Errorf("output does not name the config file:\n%s", out)
	}
	// With no file, it says so rather than printing a path that reads as confirmation it was read.
	if !strings.Contains(out, "does not exist") {
		t.Errorf("output does not say the file is absent:\n%s", out)
	}
}

// `cm a` is `cm attach`.
//
// The command typed most often, so it gets the shortest unambiguous name. Asserted because an alias that
// silently stops resolving is the kind of thing nobody notices until muscle memory fails.
func TestAttachAliasExists(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// --help rather than a real attach, since this is about the alias resolving, not about attaching.
	out := e.mustRun("a", "--help")
	if !strings.Contains(out, "Attach to a session") {
		t.Errorf("`cm a --help` did not print attach's help:\n%s", out)
	}
}

// The reported source distinguishes a flag from an environment variable, including when both are set.
//
// Worth its own test because it cannot be worked out after the fact and I got it wrong twice. bindEnv fills
// unset flags by calling Flags().Set, which marks them Changed, so a flag passed on the command line and one
// taken from the environment are indistinguishable afterwards. The first version inferred it from whether the
// variable was set, which reported the environment as the source even when a flag had overridden it: the value
// was right and the explanation was wrong, which for a command whose whole job is explaining where values come
// from is the worst kind of bug. bindEnv now records what it filled.
func TestConfigDistinguishesFlagFromEnvironment(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// The harness sets both directories through the environment, so a flag passed here overrides one.
	other := filepath.Join(e.state, "other-runtime")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	out := e.mustRun("--runtime-dir", other, "config", "--json")
	var got configResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("parsing config output %q: %v", out, err)
	}

	// The flag wins, which it already did.
	if got.RuntimeDir != other {
		t.Errorf("runtime_dir = %q, want the flag's value %q", got.RuntimeDir, other)
	}
	// And is reported as winning, which is the part that was wrong.
	if got.Sources["runtime_dir"] != "flag" {
		t.Errorf("sources[runtime_dir] = %q, want \"flag\": the flag overrode the environment",
			got.Sources["runtime_dir"])
	}
	// The directory with no flag still reports the environment, so the fix did not simply report "flag"
	// for everything.
	if src := got.Sources["state_dir"]; !strings.HasPrefix(src, "$CM_") {
		t.Errorf("sources[state_dir] = %q, want the environment variable that set it", src)
	}
}

// A test environment does not read the developer's real config file.
//
// This was broken and silently so. The harness set CM_CONFIG to an empty string meaning "no config", but empty
// means unset, so cm fell through to XDG_CONFIG_HOME and read ~/.config/cm/cm.toml: a run that had written no
// config saw detach_key = ctrl-o from the developer's machine. It looked isolated only because the fallback path
// did not exist until XDG support was added, which is what surfaced it.
//
// Asserted here rather than trusted, since the failure mode is every e2e test quietly depending on whoever ran
// it.
func TestHarnessDoesNotReadTheDevelopersConfig(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	got := e.cmConfig()
	if got.FileExists {
		t.Errorf("a test read an existing config file at %q, want an isolated absent one", got.File)
	}
	// The path has to be inside this test's own directory, or isolation depends on the machine not having a
	// config rather than on the harness.
	if !strings.HasPrefix(got.File, e.state) {
		t.Errorf("config path %q is outside the test's state directory %q", got.File, e.state)
	}
}

// A config the server cannot read must stop an upgrade before it stops the running server.
//
// The order is what makes this worth a check. `cm upgrade` shuts the old server down first, and its binary
// has already been replaced in place, so a replacement that cannot start leaves nothing to go back to: on
// darwin the bytes the old process is still executing cannot be named again, measured against a running
// server. Failing first costs nothing, since the file is read either way.
//
// Malformed TOML rather than an unknown setting, because an unknown setting is only a warning now.
func TestUpgradeRefusesRatherThanStrandingTheServer(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	// A session, so the thing at risk is present: its shim holds a pty and a shell.
	e.mustRun("run", "--session", "held", "-d", "--", "/bin/sh", "-c", "sleep 30")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("held")
		return ok && s.State == "running"
	})

	e.writeConfig("this is not = valid toml [[[\n")

	r := e.run("upgrade")
	if r.code == 0 {
		t.Errorf("`cm upgrade` exit code = 0 with a config the server cannot read, want non-zero\nstdout: %s",
			r.stdout)
	}
	// The point of refusing: the server that was running is still running and still serving.
	if s, ok := e.session("held"); !ok || s.State != "running" {
		t.Errorf("session held = %+v after a refused upgrade, want it still running and listed", s)
	}
}
