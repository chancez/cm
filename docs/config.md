# Configuration

Configuration is optional. cm works with no file, and every setting has a default that suits the
common case.

The file is TOML, read from `$XDG_CONFIG_HOME/cm/cm.toml` if that is set, otherwise the platform's
config directory (`~/.config/cm/cm.toml` on Linux, `~/Library/Application Support/cm/cm.toml` on
macOS). Override with `--config` or `$CM_CONFIG`. Run `cm config` to see which path is in use.

An unknown key is ignored with a warning, and `cm config` lists it and exits non-zero. The split is
deliberate. Refusing everywhere is what a misspelled setting deserves, but the server reads the same
file, and `cm upgrade` stops the running server before starting the replacement: one key the new build
did not know left no server at all, with every session's shell alive and every attached client waiting
on a server that could never start. One file serves every build on a machine, so a key one build knows
and another does not is ordinary. Anything holding a shell up therefore carries on, and the one command
a person runs to ask why a setting does nothing is the one that fails.

## Example

```toml
# Scrollback retained per session, in lines. 0 means unlimited.
scrollback_lines = 10000

# Which of several attached clients sets the session's size:
# "leader" (default), "last-attach", "first-attach", or "smallest".
#
# Under "leader" the window you are typing in owns the size. That survives a reconnect: a client whose
# stream dropped, which includes a repaint and a server restart, keeps its place rather than handing the
# size to whichever window reconnects first.
resize_policy = "leader"

# The key that detaches a client. "none" disables detaching by key.
detach_key = "ctrl-\\"

# The key that opens cm's overlay inside a session. "none" disables it.
prefix_key = "ctrl-]"

# Diagnostic log level: debug, info, warn, error, or off.
log_level = "info"

# How long an exited shim's diagnostic log is kept. "0" keeps every one.
shim_log_retention = "168h"

# How long a pre-migration snapshot of the database is kept, for rolling back.
database_backup_retention = "168h"

# Whether `cm rebind` ends the session it moves a name off.
rebind_replaces = false

# Where sockets and state live. Absolute paths: "~" is not expanded.
runtime_dir = "/tmp/cm"
state_dir = "/home/user/.local/state/cm"

[env]
# Added to the built-in capture list. A trailing "*" matches by prefix.
capture = ["MY_TERMINAL_VAR", "PROJECT_*"]

# Removed from the effective list, including built-ins.
exclude = ["SSH_AUTH_SOCK"]

# Replaces the built-in list entirely, ignoring `capture`.
# capture_only = ["TERM", "KITTY_*"]

[persist]
# Let session content survive a reboot. Off by default.
enabled = true

# Name patterns that persist automatically. A trailing "*" matches by prefix.
sessions = ["kitty.*", "work"]

# What happens when a dead session is attached to: "shell", "none", or "command".
on_restore = "shell"

# Program names that may be re-run on restore without a per-session request.
safe_commands = ["nvim", "less", "htop"]

# Retained output per session. 0 means the default.
max_lines = 10000
max_bytes = 16777216

# Remove a dead persisted session after this long.
expire_after = "168h"

# Remove an ended session that saved no output after this long.
forget_unpersisted_after = "5m"
```

## Top-level options

| Key | Values | Default |
| --- | --- | --- |
| `scrollback_lines` | integer; `0` is unlimited | `2000` |
| `resize_policy` | `leader`, `last-attach`, `first-attach`, `smallest` | `leader` |
| `detach_key` | `ctrl-<key>`, or `none` to disable | `ctrl-\` |
| `prefix_key` | `ctrl-<key>`, or `none` to disable the overlay | `ctrl-]` |
| `log_level` | `debug`, `info`, `warn`, `error`, `off` | `info` |
| `shim_log_retention` | Go duration; `0` keeps every log | `168h` (a week) |
| `database_backup_retention` | Go duration; `0` keeps every snapshot | `168h` (a week) |
| `rebind_replaces` | bool; `cm rebind` ends the session it moves a name off | `false` |
| `runtime_dir` | path | see [Directories](#directories) |
| `state_dir` | path | see [Directories](#directories) |

### scrollback_lines

How much scrollback a session retains. libghostty prunes at page granularity, so the effective
limit is somewhat higher than the number given.

### resize_policy

A session can have several clients attached, at different sizes, and only one size reaches the pty.
This picks which client's size wins. For a single client every policy behaves identically.

- **`leader`** gives sizing to the client that last typed. Only real typing transfers it: mouse
  motion, focus changes, key releases, and replies to queries never claim sizing.
- **`last-attach`** gives it to the newest client, so opening a second window on a session reflows
  the first.
- **`first-attach`** keeps it with the earliest client until it leaves, then passes it to the next
  earliest.
- **`smallest`** fits every client, minimizing each dimension independently, so nothing is cut off
  for anyone.

A read-only follower never owns sizing under any policy.

A client that upgrades in place, or reconnects after the server went away, is a window returning rather
than arriving, and that distinction is load-bearing in two directions.

Its size is recorded, which it has to be. A resume used to skip sizing entirely, on the reasoning that
the pty already matched the terminal coming back, and that left the client recorded as 0x0. Every policy
reads zero as "has not reported a size", so under `smallest` a window silently dropped out of the
calculation on upgrade and the session grew to another window's size.

But a resume acquires no sizing the window did not already hold. Under `leader` and `last-attach` a
returning window does not become the one that sizes the session, because the session may deliberately be
holding a size its departed leader set: leadership is unclaimed rather than transferred when a leader
detaches, so that a window nobody touched is not reflowed, and an upgrade is not a touch. Typing still
takes leadership as usual. Under `smallest` the size is a function of every attached client, so a
returning window's constraint applies immediately, and under `first-attach` sizing follows attach order,
which returning does not change.

When a resume does resize, it uses a plain resize rather than the forced SIGWINCH a fresh attach sends,
so an upgrade at an unchanged size costs the shell no redraw.

### detach_key

Accepts `ctrl-<key>` for a letter or one of the characters that have a control code (`[`, `\`, `]`,
`^`, `_`, `?`, `@`, and space), and `none` to disable detaching by key. `c-` is accepted as a
synonym for the `ctrl-` prefix, and the key is case-insensitive.

The key is detected in three encodings: the raw control byte, the kitty keyboard protocol form, and
xterm's modifyOtherKeys form, so it still works under a program that enables either protocol.

Nested cm sessions need no special setting: the key leaves the innermost session, and a second press
leaves the outer one. See "Who owns the detach key when sessions are nested" in
[architecture.md](architecture.md).

`cm attach --detach-key` overrides the setting for a single attachment. Use it when something
outside cm already claims the key, such as attaching from inside another multiplexer.
`cm detach [session]` is the same operation without a key, for a script or for detaching a session
other than the one you are in.

### prefix_key

The key that opens the overlay: a few rows at the bottom of the screen from which any cm command can
run without leaving the program in the session. Accepts the same spellings as `detach_key`, including
`ctrl-space`, and `none` to disable the overlay entirely. `cm attach --prefix-key` overrides it for one
attachment.

Detaching is unaffected and still takes one press of `detach_key`. Both keys are live at once, so
`cm attach` refuses a configuration where they are the same key rather than choosing between them.

Inside the overlay: `s` to switch and `k` to kill, both choosing from a filterable list rather than asking
you to type a name (`ctrl-j`/`ctrl-k` or the arrows move, enter chooses); `b` to name this session; `d` to detach; `:` for any cm command; `?` for the rest;
escape to close. Pressing the prefix or the detach key a second time forwards it to the program,
which is the only way to reach a key cm intercepts: `ctrl-\` never reached a pty from a cm client
before this, so SIGQUIT was unreachable inside a session.

`ctrl-]` was chosen for ergonomics and cost. Left ctrl with a right-hand key, unlike screen's `ctrl-a`
and tmux's `ctrl-b`, and the cheapest right-hand control code to take: `ctrl-o` is vim's jumplist-back,
`ctrl-u`, `ctrl-p`, `ctrl-n` and `ctrl-l` are readline's, and `ctrl-]` costs only vim's ctags tag-jump,
which an LSP's `gd` has largely replaced. `ctrl-space` has better ergonomics still and is spellable, but
is not the default because not every terminal sends NUL for it. See [overlay.md](overlay.md).

### log_level

The minimum severity recorded in the diagnostic logs. `off` disables them.

Logs live under the state directory, are owner-only since they record session names, directories,
and command lines, and rotate at 4 MiB keeping one previous generation. Read them with `cm logs`:

```
cm logs server            # the server's log
cm logs client            # every client's, shared
cm logs shim work         # one session's shim log
cm logs server -f         # follow
cm logs server --all      # include the rotated previous file
```

This is the diagnostic log, not session output. `cm history` is what the shell printed.

### shim_log_retention

How long an exited shim's diagnostic log is kept. There is one shim log per session, so without
pruning a machine that opens a session per terminal window accumulates a file per window.

The server prunes at startup and hourly after that. A log is kept regardless of age while its
session has a record, is live in the registry, or has a shim socket on disk, so `cm logs shim NAME`
works for as long as the name is listed. Age comes from the log's newest entry, falling back to its
modification time.

`shim_log_retention = "0"` keeps every shim log.

### database_backup_retention

How long a snapshot of the database taken before a schema migration is kept.

Schema changes are not reversible, so the snapshot is the only way back to a build that predates one. It is
taken whenever the schema moves, which includes the server a client starts automatically, and deliberately
*not* deleted when the migration succeeds: that is when it becomes useful rather than when it stops being.
Nothing needs to guard a migration that failed, because each one runs in a single transaction with its
version bump, so an interrupted upgrade leaves the database untouched.

What bounds it is that a snapshot's usefulness decays into a hazard. Every session created after it was
taken is missing from it, and a session missing from the database is one whose shim nothing can find again,
so restoring an old snapshot strands however many shells accumulated since. A week in, reinstalling the
newer build is the only sane recovery.

The file sits beside the database as `cm.db.v<version>.bak` and costs tens of kilobytes: 28672 bytes for a
snapshot of a database with one session, against a real install's 61440-byte database. Swept at startup and
hourly, on the same pass as the shim logs.

`database_backup_retention = "0"` keeps every snapshot.

### rebind_replaces

Makes `cm rebind` end the session it moves a name off, as though `--replace` had been passed.
`--replace=false` overrides it for one call.

Off by default, because the session left behind is a live shell and cm asks before ending one. On is the
right setting when your windows are per-window sessions: the vacated one is then the shell the emulator
made for that window and nothing else refers to it.

Two things are refused even with the setting on, since neither is what it asked for. A session with another
name is not this window's alone, and one running a foreground command has work in it; `--force` overrides
both. The busy check is skipped when the rebind is typed inside the session being replaced, because there it
cannot mean anything: `cm rebind` is itself a foreground command, so the session always reads busy.

## Directories

cm uses two directories, for different lifetimes. `cm config` prints both, and which rule chose
each.

| | Holds | Resolution |
| --- | --- | --- |
| runtime | sockets, and nothing else | `--runtime-dir`, `$CM_RUNTIME_DIR`, `runtime_dir`, `$XDG_RUNTIME_DIR/cm`, then `$TMPDIR/cm-$UID` |
| state | the database and logs | `--state-dir`, `$CM_STATE_DIR`, `state_dir`, `$XDG_STATE_HOME/cm`, then `~/.local/state/cm` |

The runtime directory holds `server.sock` and one `shim-NAME.sock` per session, and nothing else, so
it can live somewhere the system sweeps. A unix socket path cannot exceed 103 bytes; `cm doctor`
warns through `long-socket-path` before that becomes a failure. Set `$CM_RUNTIME_DIR` or
`runtime_dir` to something shorter if you need the room.

Inside the state directory, diagnostic logs are split by which process wrote them:

```
logs/server/server.log     the server's, of which there is one
logs/client/client.log     every client's, shared, with pid and boot fields
logs/shim/<session>.log    one per session's shim
logs/<session>.log         session output, which is not a diagnostic log
```

Clients share one file rather than having one each, since a client is short-lived and there can be
one per attached window. The `pid` and `boot` fields identify the writer.

## [env]

Controls which environment variables follow a client into a session.

| Key | Effect |
| --- | --- |
| `capture` | Adds patterns to the built-in list. A trailing `*` matches by prefix. |
| `exclude` | Removes patterns from the effective list, including built-ins. |
| `capture_only` | Replaces the built-in list entirely, ignoring `capture`. |

These configure the list captured on **every attach** and served by `cm get-env` to a shell that is
already running. They do not affect what a new session's shell is born with: that is the environment
of the client that created it, less the dynamic linker variables (`LD_PRELOAD`, `LD_AUDIT`,
`LD_LIBRARY_PATH`, and the `DYLD_*` family), and it is not configurable.

Two consequences worth stating. A client's secrets reach the sessions it creates, exactly as they
reach a terminal split started from the same shell; nothing is filtered by name. But nothing
forwarded is written to disk, whereas the captured list is recorded in the session record, which is
a file. That is why the captured list is an allow-list.

`SHELL` is forwarded like anything else, so a session runs the shell its creating client named.

### What is captured by default

Variables a terminal or session manager sets to describe itself, all of which go stale when that
terminal is replaced: `TERM`, `COLORTERM`, `TERMINFO`, `TERM_PROGRAM`, `TERM_PROGRAM_VERSION`,
`WINDOWID`, the `KITTY_*`, `GHOSTTY_*`, `ITERM_*`, `WEZTERM_*`, `ALACRITTY_*` and `FOOT_*` families,
`VTE_VERSION`, `DISPLAY`, `WAYLAND_DISPLAY`, and the `SSH_*` connection and agent variables. See
`internal/sessionenv` for the exact list.

A session's shell captures these once, when it starts, and nothing outside a process can change its
environment afterwards. Reattaching from a different terminal, or from the same terminal after it
restarted, leaves the shell describing a terminal that no longer exists. kitty's `KITTY_LISTEN_ON`
is the sharp case, since every `kitten @` call goes through that socket; `SSH_AUTH_SOCK` has the
same shape, and a long-lived session quietly loses the ability to push to git.

`cm get-env` prints what the most recent client had, for a shell hook to apply.

### Shell integration

```sh
# zsh: refresh before each prompt
precmd() { eval "$(cm get-env --format=posix)" }
```

```bash
# bash
PROMPT_COMMAND='eval "$(cm get-env --format=posix)"'
```

```fish
function cm_env --on-event fish_prompt
    cm get-env --format=fish | source
end
```

With no session argument, `get-env` uses `$CM_SESSION`, so a hook needs no configuration.

By default it prints only variables that differ from the current environment, so a prompt hook is
cheap. `--all` prints everything recorded. Output is plain `name=value` lines with a leading `-`
marking a variable the client no longer has; `--format=posix` or `--format=fish` selects
shell-eval output instead. There is no auto-detection, because `$SHELL` is the login shell rather
than the one running in the session.

A variable that vanished is reported explicitly rather than left at its stale value, and only for
variables cm manages, so a hook can never delete unrelated parts of the environment.

## [persist]

Whether a session's content survives a reboot. Only content can survive: the pty and the shell are
gone unconditionally. See [persistence.md](persistence.md) for how restore works.

| Key | Values | Default |
| --- | --- | --- |
| `enabled` | boolean | `false` |
| `sessions` | name patterns; a trailing `*` matches by prefix | none |
| `on_restore` | `shell`, `none`, `command` | `shell` |
| `safe_commands` | program names | none |
| `max_lines` | integer; `0` means the default | `10000` |
| `max_bytes` | integer; `0` means the default | `16777216` (16 MiB) |
| `expire_after` | Go duration, must be positive | `168h` (a week) |
| `forget_unpersisted_after` | Go duration, must be positive | `5m` |

- **`enabled`** turns persistence on for sessions matching `sessions`, or for any session started
  with an explicit request.
- **`on_restore`** decides what happens when a dead session is attached to. `shell` starts a fresh
  shell in the recorded directory, `none` leaves the restored content as history and starts nothing,
  and `command` re-runs the recorded command verbatim.
- **`safe_commands`** lists program names that may be re-run on restore without a per-session
  request. It is a convenience, not a safety boundary: it matches the program name only, so listing
  `nvim` also matches an nvim invocation that writes files.
- **`max_bytes`** applies regardless of `max_lines`, so one very long line cannot fill the disk.
- **`expire_after`** and **`forget_unpersisted_after`** bound how long records are kept. Both reject
  zero, which would delete a session's record the moment it ended.
