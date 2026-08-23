package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/client"
	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
)

func newConfigCommand(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print the effective configuration",
		Long: `Print the configuration ` + paths.Name + ` is actually using.

Resolved rather than echoed: every value is what the running code would read,
with defaults applied, so a setting left out of the file shows the value in
effect rather than a blank. That is the question worth answering, since a
mistyped setting and an absent one look identical in a file.

Where each value came from is shown too. Directories resolve through flag, then
environment, then file, then default, and knowing which one won is usually the
whole reason for asking.

Reports where the file was looked for even when there is none, which is the
first thing to check when a setting appears to do nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfig(cmd, g, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// configJSON is the JSON shape of the effective configuration.
type configJSON struct {
	// File is where the config was read from, or looked for.
	File       string `json:"file"`
	FileExists bool   `json:"file_exists"`

	RuntimeDir string `json:"runtime_dir"`
	StateDir   string `json:"state_dir"`
	// Sources records how the directories were resolved, since that is the part with precedence.
	Sources map[string]string `json:"sources"`

	ScrollbackLines int    `json:"scrollback_lines"`
	ResizePolicy    string `json:"resize_policy"`
	DetachKey       string `json:"detach_key"`
	LogLevel        string `json:"log_level"`
	LogEnabled      bool   `json:"log_enabled"`
	RestoreMode     string `json:"restore_mode"`
	ExpireAfter     string `json:"expire_after"`
	ForgetAfter     string `json:"forget_unpersisted_after"`
	// ShimLogRetention is reported like the other retention settings, since "my setting does nothing" is
	// the question this command exists to answer, and a value that cannot be read back cannot be checked.
	ShimLogRetention string `json:"shim_log_retention"`
	// DatabaseBackupRetention, for the same reason: a snapshot is what a rollback needs, so how long one
	// survives is worth being able to read back before finding out by needing it.
	DatabaseBackupRetention string `json:"database_backup_retention"`
	// RebindReplaces, because "my setting does nothing" is the question this command answers, and a
	// default that ends a session is one worth being able to read back.
	RebindReplaces bool     `json:"rebind_replaces"`
	EnvCapture     []string `json:"env_capture"`
	// UnknownSettings are settings in the file this build does not know, which everything else ignores
	// with a warning. Empty on a healthy install.
	UnknownSettings []string `json:"unknown_settings"`
}

func runConfig(cmd *cobra.Command, g *globals, asJSON bool) error {
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	if g.configPath != "" {
		path = g.configPath
	}

	cfg, err := g.config()
	if err != nil {
		return err
	}
	dirs, err := g.dirs()
	if err != nil {
		return err
	}
	// The origin of the built-in resolution, which only paths knows: XDG_RUNTIME_DIR and XDG_STATE_HOME both
	// produce an absolute path, so a directory they chose is indistinguishable from the default once resolved.
	_, origin, err := paths.DefaultWithOrigin()
	if err != nil {
		return err
	}

	// Read through the accessors rather than the struct fields, so what is printed is what the rest of the
	// program sees: the fields hold what the file said, which for anything unset is the zero value.
	level, logEnabled, err := cfg.Logging()
	if err != nil {
		return err
	}
	resize, err := cfg.Resize()
	if err != nil {
		return err
	}
	restore, err := cfg.RestoreMode()
	if err != nil {
		return err
	}
	expire, err := cfg.ExpireAfter()
	if err != nil {
		return err
	}
	forget, err := cfg.ForgetUnpersistedAfter()
	if err != nil {
		return err
	}
	dbBackupRetention, err := cfg.KeepDatabaseBackupsFor()
	if err != nil {
		return err
	}
	shimLogRetention, err := cfg.KeepShimLogsFor()
	if err != nil {
		return err
	}

	detach := cfg.DetachKey
	if detach == "" {
		detach = client.DefaultDetachKey
	}

	_, fileErr := os.Stat(path)
	out := configJSON{
		File:       path,
		FileExists: fileErr == nil,
		RuntimeDir: dirs.Runtime,
		StateDir:   dirs.State,
		Sources: map[string]string{
			"runtime_dir": dirSource(cmd, "runtime-dir", cfg.RuntimeDir, origin.Runtime),
			"state_dir":   dirSource(cmd, "state-dir", cfg.StateDir, origin.State),
		},
		ScrollbackLines: cfg.Scrollback(),
		ResizePolicy:    resize,
		DetachKey:       detach,
		LogLevel:        strings.ToLower(level.String()),
		LogEnabled:      logEnabled,
		RestoreMode:     string(restore),
		ExpireAfter:     expire.String(),
		ForgetAfter:     forget.String(),
		// Spelled "never" rather than "0s" when pruning is off, since zero here means "keep every shim log"
		// rather than "prune immediately", and "0s" reads as the second.
		ShimLogRetention: durationOrNever(shimLogRetention),
		// Also "never" rather than "0s" when disabled, where zero means "keep every snapshot".
		DatabaseBackupRetention: durationOrNever(dbBackupRetention),
		RebindReplaces:          cfg.RebindReplaces,
		EnvCapture:              cfg.EnvPatterns(),
		UnknownSettings:         cfg.UnknownSettings(),
	}
	if out.UnknownSettings == nil {
		// An empty array rather than null, so a script can iterate unconditionally.
		out.UnknownSettings = []string{}
	}

	if asJSON {
		if err := writeJSON(os.Stdout, out); err != nil {
			return err
		}
		return unknownSettingsError(out.UnknownSettings)
	}

	fmt.Fprintf(os.Stdout, "file                      %s", out.File)
	if !out.FileExists {
		// Said explicitly, because "my setting does nothing" is nearly always a file that is not where cm
		// looks, and a path printed without this reads as confirmation that it was read.
		fmt.Fprint(os.Stdout, " (does not exist; defaults in use)")
	}
	fmt.Fprintln(os.Stdout)

	fmt.Fprintf(os.Stdout, "runtime_dir               %s (%s)\n", out.RuntimeDir, out.Sources["runtime_dir"])
	fmt.Fprintf(os.Stdout, "state_dir                 %s (%s)\n", out.StateDir, out.Sources["state_dir"])
	fmt.Fprintf(os.Stdout, "scrollback_lines          %d\n", out.ScrollbackLines)
	fmt.Fprintf(os.Stdout, "resize_policy             %s\n", out.ResizePolicy)
	fmt.Fprintf(os.Stdout, "detach_key                %s\n", out.DetachKey)
	if out.LogEnabled {
		fmt.Fprintf(os.Stdout, "log_level                 %s\n", out.LogLevel)
	} else {
		fmt.Fprintln(os.Stdout, "log_level                 off")
	}
	fmt.Fprintf(os.Stdout, "restore_mode              %s\n", out.RestoreMode)
	fmt.Fprintf(os.Stdout, "expire_after              %s\n", out.ExpireAfter)
	fmt.Fprintf(os.Stdout, "forget_unpersisted_after  %s\n", out.ForgetAfter)
	fmt.Fprintf(os.Stdout, "shim_log_retention        %s\n", out.ShimLogRetention)
	fmt.Fprintf(os.Stdout, "database_backup_retention %s\n", out.DatabaseBackupRetention)
	fmt.Fprintf(os.Stdout, "rebind_replaces           %t\n", out.RebindReplaces)
	fmt.Fprintf(os.Stdout, "env capture               %s\n", strings.Join(out.EnvCapture, " "))
	if len(out.UnknownSettings) > 0 {
		// In the report rather than on stderr, next to the values that are in effect, since the whole
		// report is what a reader is scanning to find the setting that is not doing anything.
		fmt.Fprintf(os.Stdout, "unknown settings          %s (ignored by this build: a typo, or a setting from another build)\n",
			strings.Join(out.UnknownSettings, " "))
	}
	return unknownSettingsError(out.UnknownSettings)
}

// unknownSettingsError fails when the file names settings this build does not know.
//
// This is the one command that still fails on them. Everything that holds a shell up warns and carries
// on, for the reason config.UnknownSettings gives, which leaves nothing to answer "why does my setting
// do nothing" -- the question this command exists for.
//
// After the report rather than instead of it, since the rest of the output is what says which values are
// actually in effect, and the report already names them. Hence reported: no second message.
func unknownSettingsError(unknown []string) error {
	if len(unknown) == 0 {
		return nil
	}
	return &exitCodeError{code: 1, reported: true}
}

// durationOrNever renders a retention period, naming the disabled case rather than printing zero.
//
// Zero means "keep forever" for shim log pruning, which is the opposite of what "0s" suggests, and a
// diagnostic that reads as the opposite of the behavior is worse than one that says nothing.
func durationOrNever(d time.Duration) string {
	if d <= 0 {
		return "never (pruning disabled)"
	}
	return d.String()
}

// dirSource names where a directory setting came from.
//
// Reported because the precedence is the interesting part: a flag beats the environment, which beats the
// file, which beats the default. When cm is looking in an unexpected place, this is the line that says why.
//
// The distinction between a flag and an environment variable comes from bindEnv, which records what it
// filled. It cannot be recovered afterwards: bindEnv fills unset flags by calling Flags().Set, which marks
// them Changed, so a flag passed on the command line and one taken from the environment look identical.
//
// An earlier version inferred it by checking whether the variable was set, which reported the environment as
// the source even when a flag had overridden it -- the value was right and the explanation was wrong, which
// for a command whose entire job is explaining where values come from is the worst kind of bug.
func dirSource(cmd *cobra.Command, flagName, fileVal, fallback string) string {
	if envName, ok := filledFromEnv[flagName]; ok {
		return "$" + envName
	}
	if f := cmd.Flags().Lookup(flagName); f != nil && f.Changed {
		return "flag"
	}
	if fileVal != "" {
		return "config file"
	}
	// Whatever paths resolved it to: an XDG variable, or the built-in default. Reporting "default"
	// unconditionally here was wrong for anyone with XDG_STATE_HOME set, which is the same class of mistake as
	// conflating a flag with an environment variable -- a right value with a wrong explanation.
	return fallback
}
