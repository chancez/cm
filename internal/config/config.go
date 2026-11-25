// Package config loads cm's configuration file.
//
// Config is optional: cm works with none, and every field has a default that suits the common
// case. The file exists for the things that genuinely vary between setups, starting with which
// environment variables follow a client into a session.
package config

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/keymap"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/remote"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/sessionenv"
)

// Config is cm's configuration.
type Config struct {
	// path is the main file, read or looked for, files every file actually read, and unknown the settings
	// they name that this build does not know. Read through accessors, so none can be mistaken for a setting.
	path    string
	files   []string
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

	// ScrollbackLines is the older spelling of Scrollback.Lines, which wins when both are set, as
	// [keys.session] wins over detach_key. Still read, since files written before the table have it.
	ScrollbackLines *int `toml:"scrollback_lines"`

	// Scrollback is the [scrollback] table. See ScrollbackConfig.
	Scrollback ScrollbackConfig `toml:"scrollback"`

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

	// Overlay styles the overlay's three regions. Empty values mean the defaults.
	Overlay OverlayConfig `toml:"overlay"`

	// PrefixKey names the key that opens the overlay inside an attached session, as a control character
	// like "ctrl-]". Empty means the default; "none" disables the overlay.
	//
	// A second setting rather than a mode on DetachKey, because both keys are live at once and they do
	// different things: detaching stays one press, and the prefix is what makes cm reachable from under a
	// full-screen program.
	PrefixKey string `toml:"prefix_key"`

	// Keys binds the actions in cm's own interfaces. See KeysConfig.
	Keys KeysConfig `toml:"keys"`

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

	// Debug holds settings for diagnosing cm itself. See DebugConfig.
	Debug DebugConfig `toml:"debug"`

	// Grep is the [grep] table. See GrepConfig.
	Grep GrepConfig `toml:"grep"`

	Env     EnvConfig     `toml:"env"`
	Persist PersistConfig `toml:"persist"`
	Remote  RemoteConfig  `toml:"remote"`
	Close   CloseConfig   `toml:"close"`
	Server  ServerConfig  `toml:"server"`
}

// ServerConfig is how a client starts a server. Read by the client that starts one, so it applies from the
// next start.
type ServerConfig struct {
	// Launchd starts the server through a one-shot launchd job on macOS rather than as a detached child of the
	// client, and is ignored elsewhere.
	//
	// Off by default, because each choice breaks something and this one breaks less of what is visible. A
	// launchd job gets a coalition of its own, so force-quitting the app that started the first client cannot
	// end every session, which is how 27 died at once. It also makes cm the privacy (TCC) responsible process
	// for every program in a session, so access to Downloads or another app's group container is asked of cm
	// rather than of the terminal the user already approved: `op` could not read 1Password's settings and
	// reported not being signed in. An ad-hoc signed cm loses any grant at the next install, so on is only
	// livable with a signed build. Started directly, the server and every session take the coalition and the
	// privacy grants of whichever app started that first client, as tmux's do.
	Launchd bool `toml:"launchd"`
}

// CloseConfig is what `cm close`, a terminal's window-close hook, does to the session a closed window showed
// when no other window shows it. Read by the server that owns the session, so each host keeps its own rule:
// a server can keep a long job running when a laptop's window on it closes, while the laptop ends its own.
type CloseConfig struct {
	// Idle is what happens to a shell idle at a prompt: "shelve", the default, keeps it to restore, and
	// "kill" ends it. A session that saves no output is killed either way, having nothing to shelve to.
	Idle string `toml:"idle"`
	// Busy is what happens to a session running something, or one whose shell never reported a prompt:
	// "detach", the default, leaves it running, and "kill" ends it. For someone who closes a window only to
	// be done with it, and detaches with the detach key to keep one.
	Busy string `toml:"busy"`
}

// RemoteConfig is how cm reaches a server on another machine.
//
// Deliberately no default remote. A setting naming one would point *every* cm command in every shell at
// another host, including the ones that answer about sessions, and the local behavior of a command is not
// something a config file should be able to change invisibly. CM_REMOTE already points one shell or one
// window at a host, which is the scope that makes sense for it.
//
// Per-host settings are under a sub-table rather than turning this one into a map: `[remote.hosts.work]`
// beside these keys stays readable and leaves these as the defaults. Naming a host there is not a default
// remote either: it gives `--remote work` and `work:@id` a meaning, and nothing goes there unasked.
type RemoteConfig struct {
	// SSHCommand is the command line that reaches a remote, empty for plain ssh.
	//
	// Overridden by --ssh-command and CM_SSH_COMMAND, as flags and the environment override every other
	// setting here. Several words are expected: `kitten ssh`, or an `ssh -F` naming another config. See
	// remote.Dialing.Command for what a replacement has to do, which is accept ssh's options and pass bytes
	// through unaltered.
	SSHCommand string `toml:"ssh_command"`

	// ConnectionPersist is how long one shared ssh connection outlives the command that opened it.
	//
	// Zero turns sharing off, which is the reason this is a duration rather than a bool plus a duration: a
	// host where ControlMaster is unwelcome and a host where a minute is too short are the same setting at
	// different values.
	//
	// The cost of each choice is measured rather than guessed. Sharing takes a `cm ls --remote` from 170ms to
	// 60-70ms, and what it leaves behind is one ssh process per host holding a connection for this long after
	// the last command. See docs/rpc.md.
	ConnectionPersist string `toml:"connection_persist"`

	// LocalName is this machine's name in a qualified reference, so `laptop:@a7k2m9x4` typed here means a
	// session here rather than an error. Empty means the short hostname. Set it to what the other machines
	// call this one in their own [remote.hosts], so one reference means one session everywhere.
	LocalName string `toml:"local_name"`

	// Hosts are the remotes this machine knows by name, keyed by that name.
	Hosts map[string]RemoteHostConfig `toml:"hosts"`
}

// RemoteHostConfig is one named remote. Every field is optional: an empty table makes the name an ssh alias.
type RemoteHostConfig struct {
	// Address is how to reach it, in --remote's syntax: ssh://[user@]host[:port][/path/to/cm] or [user@]host.
	// Empty means the name itself, which is the ssh alias most hosts already have.
	Address string `toml:"address"`
	// SSHCommand and ConnectionPersist override [remote]'s for this host.
	SSHCommand        string `toml:"ssh_command"`
	ConnectionPersist string `toml:"connection_persist"`
}

// OverlayConfig is how the overlay is drawn.
//
// Configurable because no default is legible in every theme, and that was learned the hard way: the first
// two-shade default came out grey on grey in a terminal other than the one it was written in. Each value is
// a style in the small grammar internal/client.ParseStyle describes, such as "white on bright-black".
// KeysConfig binds the actions in cm's own interfaces.
//
// Per interface rather than one shared table, because the same verb has different keys in each: the
// overlay kills with k under a program that is still drawing, the picker with x in a list where x is
// free. A shared table would have had to pick a winner and silently change one of them.
//
// A list replaces that action's defaults rather than adding to them, which is the only rule that can
// take a default away, and an empty list unbinds. `cm keys` prints what is in effect.
type KeysConfig struct {
	// Session, Overlay and TUI are each keyed by action name: "next", "kill", "attach".
	//
	// One table per place a key is live, which is the only thing that differs between them. Session keys
	// are intercepted before the program sees them, so each one is taken from every program in every
	// session; the other two cost nothing, because they are matched while cm is on screen. detach and
	// prefix are session actions like any other and are the two with defaults.
	//
	// Maps rather than a struct per table, so an action name this build does not know reaches the code that
	// can say so by name instead of toml recording it as an undecoded key.
	Session map[string]KeyList `toml:"session"`
	Overlay map[string]KeyList `toml:"overlay"`
	TUI     map[string]KeyList `toml:"tui"`
}

// KeyList is one key or several, written either way.
//
// `detach_key = "ctrl-\\"` has always been a bare string, so a list-only setting would make the new
// spelling of an old setting look different for no reason. Both forms decode, and a wrong *type* here
// would otherwise be a toml parse error, which fails Load and takes the whole file with it -- every
// other mistake in a binding is survivable, and this one has no reason not to be.
type KeyList []string

// UnmarshalTOML accepts a string, a list of strings, or an empty list.
func (k *KeyList) UnmarshalTOML(v any) error {
	switch val := v.(type) {
	case string:
		*k = KeyList{val}
		return nil
	case []any:
		out := make(KeyList, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				return fmt.Errorf("a key must be a string, got %T", item)
			}
			out = append(out, s)
		}
		// Kept non-nil when the list was written and empty, since that is what unbinds an action: a nil
		// list would be indistinguishable from the setting being absent, which means "use the default".
		*k = out
		return nil
	default:
		return fmt.Errorf("a key setting must be a string or a list of strings, got %T", v)
	}
}

// GrepConfig is what `cm grep` searches scrollback with.
type GrepConfig struct {
	// Command is the shell command `cm grep` runs, with its own flags. cm appends `-e PATTERN --` and one file
	// per session, so it must take a pattern after -e and files after --, as rg, grep, and ugrep do. Empty
	// means rg, then grep. --grep-command and CM_GREP_COMMAND override it.
	Command string `toml:"command"`
}

// ScrollbackConfig is how much history a session keeps and what shows it.
type ScrollbackConfig struct {
	// Lines bounds retained scrollback per session. Zero means unlimited, so nil is what means unset.
	//
	// libghostty prunes at page granularity, so the effective limit is somewhat higher.
	Lines *int `toml:"lines"`
	// Pager is the shell command the overlay shows the scrollback in, fed it on stdin, as kitty's
	// scrollback_pager is. Empty means PAGER, then less. CM_PAGER overrides it, as a CM_ variable overrides
	// the file everywhere else.
	Pager string `toml:"pager"`
}

type OverlayConfig struct {
	// Bar is the top row, which names the session and the keys.
	Bar string `toml:"bar"`
	// Body is everything under it: the help, a command's output, and a list's rows.
	Body string `toml:"body"`
	// Selected is a list's selected row, which is otherwise a body row with a marker on it.
	Selected string `toml:"selected"`
}

// PersistConfig controls whether a session's content survives a reboot.
//
// Only content can survive: a pty is a kernel object and a shell is a process, so both are gone
// unconditionally. Restoring means replaying what was on screen and optionally starting something
// fresh, never resuming a process.
type PersistConfig struct {
	// Enabled turns on the name patterns in Sessions, and nothing else. An explicit --persist
	// persists a session whatever this says: the request is per session and needs no config to
	// honor it. Measured with enabled = false: a --persist session wrote its log and restored its
	// scrollback on revive, and the same session without the flag did neither.
	Enabled bool `toml:"enabled"`

	// Sessions are name patterns that persist automatically, with a trailing "*" matching by
	// prefix. Per-window sessions are the ones worth persisting; a throwaway usually is not.
	Sessions []string `toml:"sessions"`

	// OnRestore is what happens when a dead session is attached to: "shell", "none", "prefill", or
	// "command".
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
	// RestorePrefill starts a shell with the declared resume command on its prompt, unrun.
	RestorePrefill RestoreMode = "prefill"
	// RestoreCommand re-runs the recorded command verbatim, or runs the declared resume command.
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
	cfg := &Config{path: path, files: existing(path)}

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

// DropInDir is the directory of drop-in files beside a config file: conf.d, next to cm.toml.
const DropInDir = "conf.d"

// LoadDefault loads the configuration from where cm looks for it by default: DefaultPath, then every
// *.toml in the conf.d directory beside it, in name order, each overriding what came before.
//
// conf.d exists so one cm.toml can be shared between machines, through dotfiles, while each machine keeps
// what is its own in a file the shared copy does not have: remote.local_name, which is necessarily
// different on every machine, or a [close] policy a server wants and a laptop does not. A directory rather
// than an include key, so nothing has to be declared, and a directory rather than every *.toml beside
// cm.toml, so other files can live there.
//
// CM_CONFIG names one file and nothing else is read. A sandbox sets CM_CONFIG=/nonexistent.toml to read no
// configuration at all, and a conf.d resolved beside it would be /conf.d.
func LoadDefault() (*Config, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	if os.Getenv(paths.Env("CONFIG")) != "" {
		return Load(path)
	}
	dropIns, err := filepath.Glob(filepath.Join(filepath.Dir(path), DropInDir, "*.toml"))
	if err != nil {
		return nil, err
	}
	if len(dropIns) == 0 {
		return Load(path)
	}
	slices.Sort(dropIns)
	return loadMerged(path, dropIns)
}

// loadMerged reads main and then each drop-in, merging them before decoding.
//
// Merged as generic tables rather than decoded into the struct one file at a time, because decoding a
// second file replaces a map entry whole: a conf.d file saying only `[remote.hosts.server]
// ssh_command = "x"` would drop the address cm.toml gave server. Tables merge key by key; any other value,
// arrays included, replaces, so a drop-in can take an entry out of a list as well as add one.
func loadMerged(main string, dropIns []string) (*Config, error) {
	cfg := &Config{path: main}
	merged := map[string]any{}
	for _, path := range append([]string{main}, dropIns...) {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) && path == main {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		var doc map[string]any
		if _, err := toml.Decode(string(data), &doc); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		// Each file's own unknown settings, named with the file when it is a drop-in, so the warning points at
		// the file that has the typo rather than at the merge.
		md, err := toml.Decode(string(data), &Config{})
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		for _, k := range md.Undecoded() {
			name := k.String()
			if path != main {
				name = filepath.Join(DropInDir, filepath.Base(path)) + ":" + name
			}
			cfg.unknown = append(cfg.unknown, name)
		}
		mergeTables(merged, doc)
		cfg.files = append(cfg.files, path)
	}

	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(merged); err != nil {
		return nil, fmt.Errorf("merging %s: %w", strings.Join(cfg.files, ", "), err)
	}
	if _, err := toml.Decode(buf.String(), cfg); err != nil {
		return nil, fmt.Errorf("merging %s: %w", strings.Join(cfg.files, ", "), err)
	}
	return cfg, nil
}

// mergeTables merges src into dst: tables recursively, everything else replaced.
func mergeTables(dst, src map[string]any) {
	for k, v := range src {
		if sub, ok := v.(map[string]any); ok {
			if have, ok := dst[k].(map[string]any); ok {
				mergeTables(have, sub)
				continue
			}
		}
		dst[k] = v
	}
}

// existing returns path in a list when the file exists, for Files.
func existing(path string) []string {
	if _, err := os.Stat(path); err == nil {
		return []string{path}
	}
	return nil
}

// Files lists every file this configuration was read from, in the order applied: cm.toml, then conf.d.
// Empty when there were none, which is every default.
func (c *Config) Files() []string { return c.files }

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

// Keymap resolves one interface's bindings: the defaults with this file's changes applied.
//
// Problems are returned beside the map rather than as an error, and everything understandable is still
// bound, for the reason UnknownSettings gives at length: a client reads this file too, and a typo that
// refused to attach would take a terminal away over a key nobody pressed.
func (c *Config) Keymap(ctx keymap.Context) (keymap.Map, []keymap.Problem) {
	var table map[string]KeyList
	switch ctx {
	case keymap.Session:
		table = c.Keys.Session
	case keymap.Overlay:
		table = c.Keys.Overlay
	case keymap.TUI:
		table = c.Keys.TUI
	}

	overrides := make(map[string][]string, len(table))
	for action, keys := range table {
		overrides[action] = keys
	}
	// The older spellings, folded in here so everything downstream sees one table. They predate [keys] and
	// every existing config uses them, so an upgrade that ignored one would take away the only way some
	// people have of leaving a session. The table wins where a file sets both.
	if ctx == keymap.Session {
		if _, ok := overrides[string(keymap.OverlayDetach)]; !ok && strings.TrimSpace(c.DetachKey) != "" {
			overrides[string(keymap.OverlayDetach)] = []string{c.DetachKey}
		}
		if _, ok := overrides[string(keymap.SessionPrefix)]; !ok && strings.TrimSpace(c.PrefixKey) != "" {
			overrides[string(keymap.SessionPrefix)] = []string{c.PrefixKey}
		}
	}
	return keymap.Build(ctx, overrides)
}

// DetachKeys and PrefixKeys are the two session keys a client needs before it has a keymap: they decide
// whether it reads the terminal at all, and one of them is refused against the other.
//
// Read from the session table rather than from settings of their own, since that is where they live now.
// Nil means nothing was configured and the caller's default applies.
func (c *Config) DetachKeys() []string { return c.sessionKeys(keymap.OverlayDetach) }

func (c *Config) PrefixKeys() []string { return c.sessionKeys(keymap.SessionPrefix) }

func (c *Config) sessionKeys(action keymap.Action) []string {
	if list, ok := c.Keys.Session[string(action)]; ok {
		return list
	}
	switch action {
	case keymap.OverlayDetach:
		return keyListOr(nil, c.DetachKey)
	case keymap.SessionPrefix:
		return keyListOr(nil, c.PrefixKey)
	}
	return nil
}

func keyListOr(list KeyList, single string) []string {
	if list != nil {
		return list
	}
	if strings.TrimSpace(single) != "" {
		return []string{single}
	}
	return nil
}

// DefaultPath returns where cm looks for its configuration.
//
// XDG_CONFIG_HOME is honoured before falling back to os.UserConfigDir, which matters on macOS: that
// function returns ~/Library/Application Support there and ignores the variable entirely, so a user who
// keeps their dotfiles in ~/.config had their file silently not read. It is also what the rest of cm
// already does, since paths.Default honours XDG_RUNTIME_DIR and XDG_STATE_HOME.
//
// Found the hard way: `cm config` reported the file as absent while it sat in ~/.config/cm/cm.toml, and a
// detach_key set there had never taken effect. A missing config is not an error, so nothing said so.
//
// Without XDG_CONFIG_HOME, ~/.config/cm/cm.toml is still found when the platform directory holds no file.
// The variable is usually exported by a shell's rc file, so a process a GUI app starts directly has none:
// kitty launching `cm attach` from the Dock is that, and its clients read no file at all while `cm config`
// in a shell reported the right one, so the overlay's pager, the keys and log_level silently took their
// defaults. The platform directory still wins when it has a file, so nobody using it sees a change.
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
	platform := filepath.Join(dir, paths.Name, paths.Name+".toml")
	if _, err := os.Stat(platform); err == nil {
		return platform, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		dotConfig := filepath.Join(home, ".config", paths.Name, paths.Name+".toml")
		if _, err := os.Stat(dotConfig); err == nil {
			return dotConfig, nil
		}
	}
	return platform, nil
}

// ScrollbackLimit returns the configured scrollback limit, or the default. The table's spelling wins over
// the older top-level one.
func (c *Config) ScrollbackLimit() int {
	switch {
	case c.Scrollback.Lines != nil:
		return *c.Scrollback.Lines
	case c.ScrollbackLines != nil:
		return *c.ScrollbackLines
	}
	return DefaultScrollbackLines
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
	case RestorePrefill:
		return RestorePrefill, nil
	case RestoreCommand:
		return RestoreCommand, nil
	default:
		return RestoreShell, fmt.Errorf(
			"on_restore = %q, want \"shell\", \"none\", \"prefill\", or \"command\"", c.Persist.OnRestore)
	}
}

// ClosePolicy returns what a close does to an idle session and to a busy one, validating both.
func (c *Config) ClosePolicy() (idle, busy string, err error) {
	idle, busy = c.Close.Idle, c.Close.Busy
	switch idle {
	case "":
		idle = "shelve"
	case "shelve", "kill":
	default:
		return "", "", fmt.Errorf("close.idle = %q, want \"shelve\" or \"kill\"", c.Close.Idle)
	}
	switch busy {
	case "":
		busy = "detach"
	case "detach", "kill":
	default:
		return "", "", fmt.Errorf("close.busy = %q, want \"detach\" or \"kill\"", c.Close.Busy)
	}
	return idle, busy, nil
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

// RemoteConnectionPersist returns how long a shared ssh connection to a remote is kept, zero for no sharing.
//
// Named for the question rather than the field, as KeepShimLogsFor is. Zero is a real answer here and means
// "do not share", so an unset value has to be distinguishable from a set one: an empty string is unset.
func (c *Config) RemoteConnectionPersist() (time.Duration, error) {
	if c.Remote.ConnectionPersist == "" {
		return remote.DefaultControlPersist, nil
	}
	d, err := time.ParseDuration(c.Remote.ConnectionPersist)
	if err != nil {
		return 0, fmt.Errorf("remote.connection_persist: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf(
			"remote.connection_persist cannot be negative, got %q", c.Remote.ConnectionPersist)
	}
	return d, nil
}

// NamedRemote is a remote from [remote.hosts], resolved.
type NamedRemote struct {
	Name   string
	Target remote.Target
	// SSHCommand is the host's own, else [remote]'s, empty for plain ssh.
	SSHCommand string
	// ConnectionPersist is the host's own, else [remote]'s.
	ConnectionPersist time.Duration
}

// LocalName is this machine's name in a qualified reference: remote.local_name, else the short hostname,
// lowercased, so a machine with no config still answers to the name it would most likely be given.
func (c *Config) LocalName() (string, error) {
	if c.Remote.LocalName != "" {
		if err := remote.ValidateName(c.Remote.LocalName); err != nil {
			return "", fmt.Errorf("remote.local_name: %w", err)
		}
		return c.Remote.LocalName, nil
	}
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("remote.local_name is unset and the hostname cannot be read: %w", err)
	}
	short, _, _ := strings.Cut(host, ".")
	short = strings.ToLower(short)
	if err := remote.ValidateName(short); err != nil {
		return "", fmt.Errorf("remote.local_name is unset and the hostname is not usable as one: %w", err)
	}
	return short, nil
}

// Remotes returns every named remote, by name, failing on the first one that is not usable.
//
// All of them rather than only the one asked for, so a typo in a host nobody is using right now is reported
// by `cm config` rather than discovered by whichever command first names it.
func (c *Config) Remotes() ([]NamedRemote, error) {
	local, err := c.LocalName()
	if err != nil {
		return nil, err
	}
	persist, err := c.RemoteConnectionPersist()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(c.Remote.Hosts))
	for name := range c.Remote.Hosts {
		names = append(names, name)
	}
	slices.Sort(names)

	out := make([]NamedRemote, 0, len(names))
	for _, name := range names {
		h := c.Remote.Hosts[name]
		if err := remote.ValidateName(name); err != nil {
			return nil, fmt.Errorf("remote.hosts: %w", err)
		}
		// This machine, in a hosts list shared between machines, which lists every one of them. Not a remote:
		// `laptop:@id` on laptop is a session here, and reaching it over ssh would be a second path to the same
		// server.
		if name == local {
			continue
		}
		target, err := remote.Parse(cmp.Or(h.Address, name))
		if err != nil {
			return nil, fmt.Errorf("remote.hosts.%s.address: %w", name, err)
		}
		r := NamedRemote{
			Name:              name,
			Target:            target,
			SSHCommand:        cmp.Or(h.SSHCommand, c.Remote.SSHCommand),
			ConnectionPersist: persist,
		}
		if h.ConnectionPersist != "" {
			d, err := time.ParseDuration(h.ConnectionPersist)
			if err != nil {
				return nil, fmt.Errorf("remote.hosts.%s.connection_persist: %w", name, err)
			}
			if d < 0 {
				return nil, fmt.Errorf(
					"remote.hosts.%s.connection_persist cannot be negative, got %q", name, h.ConnectionPersist)
			}
			r.ConnectionPersist = d
		}
		out = append(out, r)
	}
	return out, nil
}

// LookupRemote returns the named remote called name. ok is false when there is none, which leaves name to be
// read as an address.
func (c *Config) LookupRemote(name string) (NamedRemote, bool, error) {
	all, err := c.Remotes()
	if err != nil {
		return NamedRemote{}, false, err
	}
	for _, r := range all {
		if r.Name == name {
			return r, true, nil
		}
	}
	return NamedRemote{}, false, nil
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

// DebugConfig holds settings for diagnosing cm itself rather than for using it.
type DebugConfig struct {
	// Pprof makes the server, each shim, and each attached client serve runtime profiles on a socket in
	// the runtime directory, read with `cm debug pprof`. Off by default. Read when each process starts, so
	// turning it on reaches a running server after a restart and a running session not at all.
	Pprof bool `toml:"pprof"`

	// FlightRecorder makes the same processes keep the last FlightRecorderWindow of their execution trace
	// in memory, read with `cm debug flight`. Independent of Pprof: either serves the socket, and each
	// refuses requests for the other when it is off. Read when each process starts, as Pprof is.
	FlightRecorder bool `toml:"flight_recorder"`

	// FlightRecorderWindow is how much trace the recorder keeps, as a Go duration. Empty means
	// DefaultFlightRecorderWindow.
	FlightRecorderWindow string `toml:"flight_recorder_window"`
}

// DefaultFlightRecorderWindow is long enough to cover noticing a stall and running a command, and short
// enough to stay small: 10s of trace measured 96 KB for an idle shim and 5.2 MB for a server under 11 MB/s
// of output.
const DefaultFlightRecorderWindow = 10 * time.Second

// FlightWindow returns how much trace each process keeps, or zero when the recorder is off.
func (c *Config) FlightWindow() (time.Duration, error) {
	if !c.Debug.FlightRecorder {
		return 0, nil
	}
	if c.Debug.FlightRecorderWindow == "" {
		return DefaultFlightRecorderWindow, nil
	}
	d, err := time.ParseDuration(c.Debug.FlightRecorderWindow)
	if err != nil {
		return 0, fmt.Errorf("debug.flight_recorder_window: %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("debug.flight_recorder_window must be positive, got %q", c.Debug.FlightRecorderWindow)
	}
	return d, nil
}
