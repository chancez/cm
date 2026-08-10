package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
)

// bindEnv is the replacement for pulling in viper, so its precedence rules are worth
// asserting directly.
func TestBindEnvPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		args    []string
		want    string
		wantErr bool
	}{
		{
			name: "default when nothing set",
			want: "default",
		},
		{
			name: "env fills an unset flag",
			env:  map[string]string{"CM_RUNTIME_DIR": "/from/env"},
			want: "/from/env",
		},
		{
			name: "flag beats env",
			env:  map[string]string{"CM_RUNTIME_DIR": "/from/env"},
			args: []string{"--runtime-dir=/from/flag"},
			want: "/from/flag",
		},
		{
			// An empty variable is treated as unset. Otherwise exporting an empty value,
			// which shell scripts do routinely, would override a default with "".
			name: "empty env is ignored",
			env:  map[string]string{"CM_RUNTIME_DIR": ""},
			want: "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear the variable first. Without this the test inherits whatever the developer's
			// shell has exported, so the default case passes or fails depending on the
			// environment it ran in rather than on the code.
			t.Setenv("CM_RUNTIME_DIR", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			var got string
			cmd := &cobra.Command{
				Use:               "test",
				PersistentPreRunE: func(c *cobra.Command, _ []string) error { return bindEnv(c) },
				RunE:              func(c *cobra.Command, _ []string) error { return nil },
			}
			cmd.Flags().StringVar(&got, "runtime-dir", "default", "")
			cmd.SetArgs(tt.args)
			cmd.SetOut(&strings.Builder{})
			cmd.SetErr(&strings.Builder{})

			err := cmd.Execute()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Execute() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("runtime-dir = %q, want %q", got, tt.want)
			}
		})
	}
}

// A malformed environment value must be reported rather than silently ignored, and it
// should read the same as a bad flag value since it goes through the same parsing.
func TestBindEnvRejectsInvalidValue(t *testing.T) {
	t.Setenv("CM_COUNT", "notanumber")

	var count int
	cmd := &cobra.Command{
		Use:               "test",
		PersistentPreRunE: func(c *cobra.Command, _ []string) error { return bindEnv(c) },
		RunE:              func(c *cobra.Command, _ []string) error { return nil },
	}
	cmd.Flags().IntVar(&count, "count", 1, "")
	cmd.SetArgs(nil)
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil, want a parse failure")
	}
	if !strings.Contains(err.Error(), "CM_COUNT") {
		t.Errorf("error = %q, want it to name the variable", err)
	}
}

// The shim is an internal re-exec target. Leaking it into help or completions would
// invite a user to run it directly, and it does nothing useful on its own.
func TestShimIsHiddenButRunnable(t *testing.T) {
	root := newRootCommand()

	shim, _, err := root.Find([]string{"shim"})
	if err != nil {
		t.Fatalf("Find(shim) error = %v: the command must still be reachable", err)
	}
	if !shim.Hidden {
		t.Error("shim.Hidden = false, want true")
	}

	var help strings.Builder
	root.SetOut(&help)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--help error = %v", err)
	}
	if strings.Contains(help.String(), "shim") {
		t.Errorf("help output mentions shim:\n%s", help.String())
	}
}

func TestAttachRejectsBadSessionName(t *testing.T) {
	for _, name := range []string{"../evil", "", "a/b"} {
		root := newRootCommand()
		root.SetOut(&strings.Builder{})
		root.SetErr(&strings.Builder{})
		root.SetArgs([]string{"attach", name})
		if err := root.Execute(); err == nil {
			t.Errorf("attach %q = nil error, want rejection", name)
		}
	}
}

// The runtime and state directories can be set three ways, and the precedence must hold.
//
// A flag beats the environment, which beats the config file. That ordering is what lets a test harness or a
// one-off invocation redirect cm without editing a file, while a standing preference still lives in
// configuration.
//
// Worth testing because it is silent when wrong: cm would use a different directory than the caller asked
// for, find no sessions there, and report an empty list rather than an error.
func TestDirsPrecedence(t *testing.T) {
	cfgDir := t.TempDir()
	cfgRuntime := filepath.Join(cfgDir, "from-config")
	cfgPath := filepath.Join(cfgDir, "cm.toml")
	if err := os.WriteFile(cfgPath, []byte("runtime_dir = "+strconv.Quote(cfgRuntime)+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	// A state dir is always set, so these never touch the real one.
	stateDir := t.TempDir()
	t.Setenv(paths.Env("STATE_DIR"), stateDir)
	t.Setenv(paths.Env("CONFIG"), cfgPath)

	t.Run("config file when nothing else is set", func(t *testing.T) {
		// Explicitly empty rather than unset, since an inherited value would decide the result and the test
		// would pass based on the environment rather than the code.
		t.Setenv(paths.Env("RUNTIME_DIR"), "")

		g := &globals{}
		got, err := g.dirs()
		if err != nil {
			t.Fatalf("dirs() error = %v", err)
		}
		if got.Runtime != cfgRuntime {
			t.Errorf("Runtime = %q, want the config file's value %q", got.Runtime, cfgRuntime)
		}
	})

	t.Run("environment beats the config file", func(t *testing.T) {
		envRuntime := filepath.Join(t.TempDir(), "from-env")
		t.Setenv(paths.Env("RUNTIME_DIR"), envRuntime)

		g := &globals{}
		got, err := g.dirs()
		if err != nil {
			t.Fatalf("dirs() error = %v", err)
		}
		if got.Runtime != envRuntime {
			t.Errorf("Runtime = %q, want the environment's value %q", got.Runtime, envRuntime)
		}
	})

	t.Run("flag beats the environment", func(t *testing.T) {
		envRuntime := filepath.Join(t.TempDir(), "from-env")
		flagRuntime := filepath.Join(t.TempDir(), "from-flag")
		t.Setenv(paths.Env("RUNTIME_DIR"), envRuntime)

		g := &globals{runtimeDir: flagRuntime}
		got, err := g.dirs()
		if err != nil {
			t.Fatalf("dirs() error = %v", err)
		}
		if got.Runtime != flagRuntime {
			t.Errorf("Runtime = %q, want the flag's value %q", got.Runtime, flagRuntime)
		}
	})
}

// A malformed config must not stop a command from resolving its directories.
//
// Every command calls dirs(), so returning an error here would make even `cm --help` fail on a typo in a
// config file. The commands that actually read configuration report the problem themselves.
func TestDirsToleratesABrokenConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "cm.toml")
	if err := os.WriteFile(cfgPath, []byte("this is not = valid = toml\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	t.Setenv(paths.Env("CONFIG"), cfgPath)
	t.Setenv(paths.Env("RUNTIME_DIR"), "")
	t.Setenv(paths.Env("STATE_DIR"), t.TempDir())

	g := &globals{}
	if _, err := g.dirs(); err != nil {
		t.Errorf("dirs() error = %v, want a broken config to be tolerated here", err)
	}
}

// Every state a wait accepts must appear in the flag's help.
//
// The two drifted: the help said "idle, busy, or exited" while the command also accepted blocked, so the
// one state cm cannot derive for itself was undiscoverable from `cm wait --help`. Both now come from the
// same table, and this keeps a future state from being added to one and not the other.
func TestWaitStatesAreAllDocumented(t *testing.T) {
	root := newRootCommand()

	var wait *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "wait" {
			wait = c
			break
		}
	}
	if wait == nil {
		t.Fatal("no wait command registered")
	}

	help := wait.Flags().Lookup("until").Usage
	for state := range waitStates {
		if !strings.Contains(help, state) {
			t.Errorf("--until help %q does not mention the accepted state %q", help, state)
		}
	}
	// And the long description, since that is where someone reads what the states mean.
	for state := range waitStates {
		if !strings.Contains(wait.Long, state) {
			t.Errorf("wait's help does not explain the accepted state %q", state)
		}
	}
}

// A flag cm exports the value of must not be filled from the environment.
//
// cm exports CM_SESSION into every session's shell, so the convention that gives every flag a
// CM_-prefixed variable turned an inherited value into an argument nobody passed. `cm run -- make` from
// inside a session reused the *calling* session: the second run printed the first command's output and
// returned its exit status, then later runs failed with "ttrpc: closed". Nothing reported an error,
// which is why this needs a test rather than a comment.
func TestBindEnvSkipsSessionFlag(t *testing.T) {
	t.Setenv(paths.SessionEnv(), "the-calling-session")

	var session string
	cmd := &cobra.Command{
		Use:               "test",
		PersistentPreRunE: func(c *cobra.Command, _ []string) error { return bindEnv(c) },
		RunE:              func(c *cobra.Command, _ []string) error { return nil },
	}
	cmd.Flags().StringVar(&session, "session", "", "")
	cmd.SetArgs(nil)
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if session != "" {
		t.Errorf("session = %q, want it empty so the server allocates a fresh name", session)
	}
	// And nothing claims the environment as its source, since `cm config` reports that.
	if key, ok := filledFromEnv["session"]; ok {
		t.Errorf("filledFromEnv[session] = %q, want the flag left unbound", key)
	}
}

// An explicit --session must still work, since excluding it from the environment must not make the
// flag itself unusable.
func TestSessionFlagStillWorksExplicitly(t *testing.T) {
	t.Setenv(paths.SessionEnv(), "the-calling-session")

	var session string
	cmd := &cobra.Command{
		Use:               "test",
		PersistentPreRunE: func(c *cobra.Command, _ []string) error { return bindEnv(c) },
		RunE:              func(c *cobra.Command, _ []string) error { return nil },
	}
	cmd.Flags().StringVar(&session, "session", "", "")
	cmd.SetArgs([]string{"--session=asked-for"})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if session != "asked-for" {
		t.Errorf("session = %q, want the value passed on the command line", session)
	}
}
