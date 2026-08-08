package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
