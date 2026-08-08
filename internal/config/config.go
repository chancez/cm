// Package config loads cm's configuration file.
//
// Config is optional: cm works with none, and every field has a default that suits the common
// case. The file exists for the things that genuinely vary between setups, starting with which
// environment variables follow a client into a session.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/sessionenv"
)

// Config is cm's configuration.
type Config struct {
	// ScrollbackLines bounds retained scrollback per session. Zero means unlimited.
	//
	// libghostty prunes at page granularity, so the effective limit is somewhat higher.
	ScrollbackLines *int `toml:"scrollback_lines"`

	// DetachKey names the key that detaches a client, as a control character like "ctrl-\".
	// Empty means the default; "none" disables detaching by key.
	DetachKey string `toml:"detach_key"`

	Env EnvConfig `toml:"env"`
}

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
