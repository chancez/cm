package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
)

// globals holds settings shared by every subcommand.
type globals struct {
	runtimeDir string
	stateDir   string
	configPath string
}

// dirs resolves the directories to use, letting flags override the environment.
func (g *globals) dirs() (paths.Dirs, error) {
	d, err := paths.Default()
	if err != nil {
		return paths.Dirs{}, err
	}

	// The config file applies only where neither the environment nor a flag has spoken, so it is the
	// lowest of the three precedences.
	//
	// The environment is read again here rather than inferred from what Default returned. Default bakes it
	// into the result, which makes "the user set CM_RUNTIME_DIR" indistinguishable from "we fell back to a
	// default" -- and overwriting the latter is right while overwriting the former is not.
	//
	// A failure to read the config is deliberately ignored rather than returned. Every command resolves
	// directories, so a malformed config would otherwise make even `cm --help` fail; the commands that
	// actually use configuration report the error themselves.
	if cfg, err := g.config(); err == nil && cfg != nil {
		if cfg.RuntimeDir != "" && os.Getenv(paths.Env("RUNTIME_DIR")) == "" {
			d.Runtime = cfg.RuntimeDir
		}
		if cfg.StateDir != "" && os.Getenv(paths.Env("STATE_DIR")) == "" {
			d.State = cfg.StateDir
		}
	}

	// A flag beats everything, including the environment Default already applied.
	if g.runtimeDir != "" {
		d.Runtime = g.runtimeDir
	}
	if g.stateDir != "" {
		d.State = g.stateDir
	}
	return d, nil
}

func newRootCommand() *cobra.Command {
	g := &globals{}

	root := &cobra.Command{
		Use:   paths.Name,
		Short: "Persistent terminal sessions",
		Long: paths.Name + ` manages terminal sessions that outlive the terminal.

Sessions survive detaching, the client exiting, and the server restarting. It
provides no windows, tabs, or splits: your terminal emulator already does that.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		// A --version flag as well as the subcommand, because both are what people try first and neither
		// being there is its own small friction. Cobra prints this for --version; the subcommand reports more,
		// including the running server's build.
		Version: paths.Version(),
		// Without this, running bare `cm` exits 0 having printed nothing useful.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return bindEnv(cmd)
		},
	}

	// Cobra's default `completion` command nests as `cm completion zsh`. The explicit
	// command below is `cm completions zsh`, matching what the user's shell config
	// already invokes for zmx.
	root.CompletionOptions.DisableDefaultCmd = true

	pf := root.PersistentFlags()
	pf.StringVar(&g.runtimeDir, "runtime-dir", "",
		"directory for sockets ($"+paths.Env("RUNTIME_DIR")+")")
	pf.StringVar(&g.stateDir, "state-dir", "",
		"directory for the database and logs ($"+paths.Env("STATE_DIR")+")")
	pf.StringVar(&g.configPath, "config", "",
		"configuration file ($"+paths.Env("CONFIG")+")")

	root.AddCommand(
		newAttachCommand(g),
		newListCommand(g),
		newWaitCommand(g),
		newReadCommand(g),
		newReportCommand(g),
		newTagCommand(g),
		newSignalCommand(g),
		newDoctorCommand(g),
		newKillCommand(g),
		newServerCommand(g),
		newSendCommand(g),
		newRunCommand(g),
		newHistoryCommand(g),
		newInfoCommand(g),
		newGetEnvCommand(g),
		newLogsCommand(g),
		newShimCommand(g),
		newVersionCommand(g),
		newConfigCommand(g),
		newStatusCommand(g),
		newCompletionsCommand(),
		newShellInitCommand(),
	)

	return root
}

// bindEnv fills flags that were not passed on the command line from the environment, so
// every flag gains a CM_-prefixed variable without declaring it twice.
//
// A flag's variable is its name uppercased with dashes as underscores, so --runtime-dir
// reads CM_RUNTIME_DIR. Values route through Flags().Set to reuse pflag's parsing and
// validation, which means a malformed value is reported the same way whether it came from
// a flag or the environment.
//
// Only unset flags are filled, which gives flag > env > default precedence for free.
//
// Which flags were filled is recorded in filledFromEnv, because Flags().Set marks a flag Changed and thereby
// erases the difference between "passed on the command line" and "taken from the environment". Nothing depends
// on that for behavior, since either way the value is the same, but `cm config` reports where each setting came
// from and would otherwise have to guess. It guessed wrong: checking the variable first reported the
// environment as the source even when a flag had overridden it.
func bindEnv(cmd *cobra.Command) error {
	var err error
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if err != nil || f.Changed {
			return
		}
		key := paths.Env(strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_")))
		val, ok := os.LookupEnv(key)
		if !ok || val == "" {
			return
		}
		if setErr := cmd.Flags().Set(f.Name, val); setErr != nil {
			err = fmt.Errorf("%s: %w", key, setErr)
			return
		}
		filledFromEnv[f.Name] = key
	})
	return err
}

// filledFromEnv records which flags bindEnv set, and from which variable.
//
// Package-level because it is written by PersistentPreRunE and read by a command's RunE, which have no other
// channel between them, and because exactly one command runs per process.
var filledFromEnv = map[string]string{}

// sessionNameArg validates a single session-name argument before RunE, so an invalid name
// fails before any connection is made.
func sessionNameArg(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("expected exactly one session name, got %d", len(args))
	}
	return paths.ValidateSessionName(args[0])
}

// config resolves and loads the configuration file.
//
// A missing file is not an error, since configuration is optional and every setting has a
// default that suits the common case.
func (g *globals) config() (*config.Config, error) {
	path := g.configPath
	if path == "" {
		p, err := config.DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	return config.Load(path)
}
