// Package config loads cm's configuration file.
//
// Config is optional: cm works with none, and every field has a default that suits the common
// case. The file exists for the things that genuinely vary between setups, starting with which
// environment variables follow a client into a session.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/sessionenv"
)

// Config is cm's configuration.
type Config struct {
	// path is where this was read from, and unknown holds the settings the file names that this build
	// does not know. Both are read through accessors, so neither can be mistaken for a setting.
	path    string
	unknown []string

	// RuntimeDir overrides where sockets live, and StateDir where the database and logs live.
	//
	// Non-circular, since the config file itself is found under the user's config directory rather than
	// under either of these: reading it does not require knowing where they point.
	//
	// Lowest precedence of the three ways to set them. A flag beats the environment, which beats this, so a
	// one-off invocation or a test harness can redirect cm without editing a file. The config file is for a
	// standing preference, such as putting sockets somewhere shorter than the default to stay under the
	// 104-byte limit on a unix socket path.
	RuntimeDir string `toml:"runtime_dir"`
	StateDir   string `toml:"state_dir"`

	// ScrollbackLines bounds retained scrollback per session. Zero means unlimited.
	//
	// libghostty prunes at page granularity, so the effective limit is somewhat higher.
	ScrollbackLines *int `toml:"scrollback_lines"`

	// ResizePolicy decides which of several attached clients sets the session's size:
	// "leader", "last-attach", "first-attach", or "smallest".
	//
	// Only matters with more than one client on a session, which for per-window sessions happens
	// deliberately rather than often. Defaults to "leader", where the client that last typed owns
	// sizing, so the window being worked in stays correct.
	ResizePolicy string `toml:"resize_policy"`

	// DetachKey names the key that detaches a client, as a control character like "ctrl-\".
	// Empty means the default; "none" disables detaching by key.
	DetachKey string `toml:"detach_key"`

	// LogLevel is the minimum severity recorded: debug, info, warn, error, or off.
	//
	// On by default at info. The server and shim run detached with their stdio discarded, so
	// without a log there is no record of what they did, and several errors are deliberately
	// swallowed to keep a session alive. Logging is what keeps that from being silent.
	LogLevel string `toml:"log_level"`

	// ShimLogRetention is how long an exited shim's diagnostic log is kept, as a Go duration. Empty
	// means DefaultShimLogRetention, and "0" disables pruning.
	//
	// A top-level setting rather than one under [persist], because a shim log is written for every
	// session whether or not its output persists.
	ShimLogRetention string `toml:"shim_log_retention"`

	// DatabaseBackupRetention is how long a snapshot taken before a schema migration is kept, as a Go
	// duration. Empty means DefaultDatabaseBackupRetention, and "0" keeps them forever.
	DatabaseBackupRetention string `toml:"database_backup_retention"`

	// RebindReplaces makes `cm rebind` end the session it moves a name off, as though --replace had been
	// passed. --replace=false overrides it for one call.
	//
	// Off by default: the session left behind is a live shell, and cm asks before ending one rather than
	// implying it. On is the right setting for someone whose windows are per-window sessions, where the
	// vacated one is the shell the emulator made and nothing else.
	RebindReplaces bool `toml:"rebind_replaces"`

	Env     EnvConfig     `toml:"env"`
	Persist PersistConfig `toml:"persist"`
}

// PersistConfig controls whether a session's content survives a reboot.
//
// Only content can survive: a pty is a kernel object and a shell is a process, so both are gone
// unconditionally. Restoring means replaying what was on screen and optionally starting something
// fresh, never resuming a process.
type PersistConfig struct {
	// Enabled turns persistence on for sessions matching Sessions, or for any session started with
	// an explicit request.
	Enabled bool `toml:"enabled"`

	// Sessions are name patterns that persist automatically, with a trailing "*" matching by
	// prefix. Per-window sessions are the ones worth persisting; a throwaway usually is not.
	Sessions []string `toml:"sessions"`

	// OnRestore is what happens when a dead session is attached to: "shell", "none", or "command".
	// Empty means "shell".
	OnRestore string `toml:"on_restore"`

	// SafeCommands are program names that may be re-run on restore without a per-session request.
	//
	// A convenience, not a safety boundary. It matches the program name only, so listing "nvim"
	// also matches an nvim invocation that writes files. The per-session setting is the real
	// control, and the default remains a fresh shell.
	SafeCommands []string `toml:"safe_commands"`

	// MaxLines bounds retained output per session. Zero means the default.
	MaxLines int `toml:"max_lines"`
	// MaxBytes is a ceiling that applies regardless of MaxLines, so one very long line cannot fill
	// the disk. Zero means the default.
	MaxBytes int64 `toml:"max_bytes"`

	// ExpireAfter removes a dead persisted session after this long, as a Go duration. Empty means
	// the default. Without expiry both the session list and the disk grow forever across reboots.
	ExpireAfter string `toml:"expire_after"`
	// ForgetUnpersistedAfter removes an ended session that saved no output after this long, as a Go
	// duration. Empty means DefaultForgetUnpersistedAfter.
	ForgetUnpersistedAfter string `toml:"forget_unpersisted_after"`
}

// RestoreMode is what happens when a dead session is attached to.
type RestoreMode string

const (
	// RestoreShell starts a fresh shell in the recorded directory. The default, because it is safe
	// and right for per-window sessions.
	RestoreShell RestoreMode = "shell"
	// RestoreNone leaves the restored content as history and starts nothing.
	RestoreNone RestoreMode = "none"
	// RestoreCommand re-runs the recorded command verbatim.
	RestoreCommand RestoreMode = "command"
)

// DefaultExpireAfter is how long a dead persisted session is kept.
const DefaultExpireAfter = 7 * 24 * time.Hour

// DefaultShimLogRetention is how long an exited shim's diagnostic log is kept.
//
// A week, matching DefaultExpireAfter rather than the 24 hours `cm doctor` scans back over. The two answer
// different questions: the scan window is how old an error can be and still describe what is happening
// now, while this is how long the record of a finished session stays worth having. A shim log outliving
// the session record is the point, since a session that vanished without a trace leaves nothing else.
//
// Sized against a measurement: a real install had accumulated 202 shim logs, one per terminal window
// opened, with the oldest thirteen days out. A week keeps roughly the last hundred on that machine and
// bounds the directory at a few days' worth of windows instead of every window ever opened.
const DefaultShimLogRetention = 7 * 24 * time.Hour

// DefaultDatabaseBackupRetention is how long a pre-migration snapshot of the database is kept.
//
// A week, matching DefaultShimLogRetention and DefaultExpireAfter, and for a reason of its own rather than
// for symmetry. A snapshot is the only way back to a build that predates a schema change, so it cannot be
// deleted when the migration succeeds: that is when it starts being useful. What bounds it is that its
// usefulness decays into a hazard. Every session created after it was taken is missing from it, and a
// session missing from the database is one whose shim nothing can find again, so restoring an old snapshot
// strands however many shells accumulated since. After a week of running the newer build, reinstalling it is
// the only sane recovery and keeping the file only invites the other one.
//
// "0" keeps them forever, for someone who would rather hold the window open.
const DefaultDatabaseBackupRetention = 7 * 24 * time.Hour

// DefaultForgetUnpersistedAfter is how long an ended session that saved no output is kept.
//
// Minutes, not days. The record holds nothing recoverable once the session ends, so its only purpose
// is letting `cm run` read back an exit status, which takes seconds. Long enough to be generous
// about that, short enough that `cm list` still shows the sessions a user cares about rather than
// every command they have run this week.
const DefaultForgetUnpersistedAfter = 5 * time.Minute

// EnvConfig controls which variables follow a client into a session.
type EnvConfig struct {
	// Capture adds to the built-in list. A trailing "*" matches by prefix.
	//
	// Additive rather than replacing, because the built-in list is what makes the feature work
	// out of the box, and a user adding one variable of their own should not have to restate it.
	Capture []string `toml:"capture"`

	// Exclude removes patterns from the effective list, including built-ins.
	//
	// Exists so a user can drop something the default captures, which matters for
	// SSH_AUTH_SOCK: it is included by default because a stale agent socket breaks git in a
	// long-lived session, but someone with a different opinion about forwarding it needs a way
	// out.
	Exclude []string `toml:"exclude"`

	// CaptureOnly replaces the built-in list entirely, ignoring Capture.
	//
	// For a user who wants exact control rather than a curated default.
	CaptureOnly []string `toml:"capture_only"`
}

// DefaultScrollbackLines matches tmux's and zmx's default.
const DefaultScrollbackLines = 2000

// Load reads the configuration file, returning defaults when there is none.
//
// A missing file is not an error, since config is optional. A malformed one is: a file toml cannot
// parse says nothing about what its settings were meant to be, so applying defaults over it would
// ignore a file that plainly asks for something else.
//
// An unrecognized setting is neither, and is recorded for the caller to act on. See UnknownSettings.
func Load(path string) (*Config, error) {
	cfg := &Config{path: path}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	md, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for _, k := range md.Undecoded() {
		cfg.unknown = append(cfg.unknown, k.String())
	}

	return cfg, nil
}

// Path is where the configuration was read from, or looked for when there is no file.
//
// Kept so a warning can name the file. The server loads its own config and has no other way to say
// where the setting it is complaining about lives.
func (c *Config) Path() string { return c.path }

// UnknownSettings returns the settings the file names that this build does not know.
//
// Recorded rather than refused, which reverses what a typo argues for: a misspelled setting is
// otherwise indistinguishable from one that has no effect, and that is still worth reporting.
//
// What decided it is the order of an upgrade. `cm upgrade` stops the running server before starting
// the replacement, so one unknown setting stopped the new server from ever coming up, after the old
// one was already gone. 36 live sessions kept their shells and every attached client hung, and no
// amount of reconnecting could help: the only way out was editing the file. One config file serves
// every build on a machine, so a setting one branch knows and another does not is ordinary.
//
// The split is therefore by who is reading. Anything holding a shell up warns and carries on;
// `cm config` fails, because a person is reading that and a typo is the question it answers.
func (c *Config) UnknownSettings() []string { return c.unknown }

// DefaultPath returns where cm looks for its configuration.
//
// XDG_CONFIG_HOME is honoured before falling back to os.UserConfigDir, which matters on macOS: that
// function returns ~/Library/Application Support there and ignores the variable entirely, so a user who
// keeps their dotfiles in ~/.config had their file silently not read. It is also what the rest of cm
// already does, since paths.Default honours XDG_RUNTIME_DIR and XDG_STATE_HOME.
//
// Found the hard way: `cm config` reported the file as absent while it sat in ~/.config/cm/cm.toml, and a
// detach_key set there had never taken effect. A missing config is not an error, so nothing said so.
func DefaultPath() (string, error) {
	if p := os.Getenv(paths.Env("CONFIG")); p != "" {
		return p, nil
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, paths.Name, paths.Name+".toml"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolving config directory: %w", err)
	}
	return filepath.Join(dir, paths.Name, paths.Name+".toml"), nil
}

// Scrollback returns the configured scrollback limit, or the default.
func (c *Config) Scrollback() int {
	if c.ScrollbackLines == nil {
		return DefaultScrollbackLines
	}
	return *c.ScrollbackLines
}

// EnvPatterns returns the effective list of environment variable patterns.
func (c *Config) EnvPatterns() []string {
	base := c.Env.CaptureOnly
	if len(base) == 0 {
		base = append(base, sessionenv.DefaultCapture...)
		base = append(base, c.Env.Capture...)
	}

	if len(c.Env.Exclude) == 0 {
		return base
	}
	excluded := make(map[string]struct{}, len(c.Env.Exclude))
	for _, p := range c.Env.Exclude {
		excluded[p] = struct{}{}
	}
	out := make([]string, 0, len(base))
	for _, p := range base {
		if _, drop := excluded[p]; !drop {
			out = append(out, p)
		}
	}
	return out
}

// EnvMatcher returns a matcher for the effective environment patterns.
func (c *Config) EnvMatcher() *sessionenv.Matcher {
	return sessionenv.NewMatcher(c.EnvPatterns())
}

// Logging returns the configured log level and whether logging is on.
func (c *Config) Logging() (slog.Level, bool, error) {
	return cmlog.ParseLevel(c.LogLevel)
}

// Resize policy names, kept as strings here rather than importing the server package: config is a
// leaf that the server depends on, and reversing that would make a cycle.
const (
	ResizeLeader      = "leader"
	ResizeLastAttach  = "last-attach"
	ResizeFirstAttach = "first-attach"
	ResizeSmallest    = "smallest"
)

// Resize returns the configured resize policy, validating it.
func (c *Config) Resize() (string, error) {
	switch c.ResizePolicy {
	case "", ResizeLeader:
		return ResizeLeader, nil
	case ResizeLastAttach, ResizeFirstAttach, ResizeSmallest:
		return c.ResizePolicy, nil
	default:
		return ResizeLeader, fmt.Errorf(
			"resize_policy = %q, want %q, %q, %q, or %q",
			c.ResizePolicy, ResizeLeader, ResizeLastAttach, ResizeFirstAttach, ResizeSmallest)
	}
}

// RestoreMode returns the configured restore behavior, validating it.
func (c *Config) RestoreMode() (RestoreMode, error) {
	switch RestoreMode(c.Persist.OnRestore) {
	case "", RestoreShell:
		return RestoreShell, nil
	case RestoreNone:
		return RestoreNone, nil
	case RestoreCommand:
		return RestoreCommand, nil
	default:
		return RestoreShell, fmt.Errorf(
			"on_restore = %q, want \"shell\", \"none\", or \"command\"", c.Persist.OnRestore)
	}
}

// PersistsSession reports whether a session name persists by configuration alone.
//
// Patterns use the same trailing-"*" prefix form as the environment list, so one convention covers
// both.
func (c *Config) PersistsSession(name string) bool {
	if !c.Persist.Enabled {
		return false
	}
	return sessionenv.NewMatcher(c.Persist.Sessions).Match(name)
}

// PersistLimits returns the retention bounds for a persisted log.
func (c *Config) PersistLimits() seqlog.FileLimits {
	limits := seqlog.DefaultFileLimits
	if c.Persist.MaxLines > 0 {
		limits.MaxLines = c.Persist.MaxLines
	}
	if c.Persist.MaxBytes > 0 {
		limits.MaxBytes = c.Persist.MaxBytes
	}
	return limits
}

// ExpireAfter returns how long a dead persisted session is kept.
func (c *Config) ExpireAfter() (time.Duration, error) {
	if c.Persist.ExpireAfter == "" {
		return DefaultExpireAfter, nil
	}
	d, err := time.ParseDuration(c.Persist.ExpireAfter)
	if err != nil {
		return 0, fmt.Errorf("expire_after: %w", err)
	}
	if d <= 0 {
		// Zero would mean "expire immediately", which is never what someone means by writing it.
		// Refusing is better than deleting sessions the moment they die.
		return 0, fmt.Errorf("expire_after must be positive, got %q", c.Persist.ExpireAfter)
	}
	return d, nil
}

// ForgetUnpersistedAfter returns how long an ended session that saved no output is kept.
func (c *Config) ForgetUnpersistedAfter() (time.Duration, error) {
	if c.Persist.ForgetUnpersistedAfter == "" {
		return DefaultForgetUnpersistedAfter, nil
	}
	d, err := time.ParseDuration(c.Persist.ForgetUnpersistedAfter)
	if err != nil {
		return 0, fmt.Errorf("forget_unpersisted_after: %w", err)
	}
	if d <= 0 {
		// Zero would delete a session's record the instant it ended, which would break `cm run`:
		// it reads the exit status back from the record after the command finishes.
		return 0, fmt.Errorf("forget_unpersisted_after must be positive, got %q",
			c.Persist.ForgetUnpersistedAfter)
	}
	return d, nil
}

// KeepShimLogsFor returns how long an exited shim's diagnostic log is kept.
//
// Zero is accepted here and means "never prune", unlike expire_after and forget_unpersisted_after, which
// reject it. The difference is what zero would destroy: there it would delete a session's record the
// moment it ended, which is never what someone means, while here it keeps a file that already survives
// forever today. Someone who wants every shim log kept has to be able to say so.
// Named for the question rather than after the field, since a method cannot share a struct field's name.
// Logging() does the same for log_level.
// KeepDatabaseBackupsFor is how long a pre-migration snapshot is kept.
//
// Named for the question rather than the field, as KeepShimLogsFor is.
func (c *Config) KeepDatabaseBackupsFor() (time.Duration, error) {
	if c.DatabaseBackupRetention == "" {
		return DefaultDatabaseBackupRetention, nil
	}
	d, err := time.ParseDuration(c.DatabaseBackupRetention)
	if err != nil {
		return 0, fmt.Errorf("database_backup_retention: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf(
			"database_backup_retention cannot be negative, got %q", c.DatabaseBackupRetention)
	}
	return d, nil
}

func (c *Config) KeepShimLogsFor() (time.Duration, error) {
	if c.ShimLogRetention == "" {
		return DefaultShimLogRetention, nil
	}
	d, err := time.ParseDuration(c.ShimLogRetention)
	if err != nil {
		return 0, fmt.Errorf("shim_log_retention: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("shim_log_retention cannot be negative, got %q", c.ShimLogRetention)
	}
	return d, nil
}

// CommandIsSafeToRerun reports whether a recorded command may be re-run on restore.
//
// Matches the program name only, which is why this is documented as a convenience rather than a
// guarantee: it cannot distinguish an editor opening a file from the same editor running a shell
// command on startup.
func (c *Config) CommandIsSafeToRerun(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	program := filepath.Base(argv[0])
	for _, safe := range c.Persist.SafeCommands {
		if program == safe {
			return true
		}
	}
	return false
}
