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
		newKillCommand(g),
		newServerCommand(g),
		newSendCommand(g),
		newRunCommand(g),
		newHistoryCommand(g),
		newInfoCommand(g),
		newGetEnvCommand(g),
		newLogsCommand(g),
		newShimCommand(g),
		newCompletionsCommand(),
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
		}
	})
	return err
}

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
