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
// A missing file is not an error, since config is optional. A malformed one is: silently falling
// back to defaults would leave a user wondering why their settings do nothing.
func Load(path string) (*Config, error) {
	cfg := &Config{}

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
	// Report unknown keys rather than ignoring them. A typo in a config file is otherwise
	// indistinguishable from the setting having no effect.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unknown settings: %v", path, keys)
	}

	return cfg, nil
}

// DefaultPath returns where cm looks for its configuration.
func DefaultPath() (string, error) {
	if p := os.Getenv(paths.Env("CONFIG")); p != "" {
		return p, nil
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
