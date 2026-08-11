# Configuration

Configuration is optional. cm works with no file, and every setting has a default that suits the
common case.

The file is TOML, read from `$XDG_CONFIG_HOME/cm/cm.toml` if that is set, otherwise the platform's
config directory (`~/.config/cm/cm.toml` on Linux, `~/Library/Application Support/cm/cm.toml` on
macOS). Override with `--config` or `$CM_CONFIG`.

`XDG_CONFIG_HOME` is honoured before the platform directory because Go's `os.UserConfigDir` ignores it
on macOS, so a file in `~/.config/cm` was silently not read. Nothing reported that, since a missing
config is not an error. Run `cm config` to see which path is in use.

An unknown key is an error rather than being ignored, because a misspelled setting is otherwise
indistinguishable from one that has no effect.

## Directories

cm uses two, for different lifetimes. `cm config` prints both, and which rule chose each.

| | Holds | Resolution |
| --- | --- | --- |
| runtime | sockets, and nothing else | `--runtime-dir`, `$CM_RUNTIME_DIR`, `runtime_dir`, `$XDG_RUNTIME_DIR/cm`, then `$TMPDIR/cm-$UID` |
| state | the database and logs | `--state-dir`, `$CM_STATE_DIR`, `state_dir`, `$XDG_STATE_HOME/cm`, then `~/.local/state/cm` |

Inside the state directory, diagnostic logs are split by which process wrote them:

```
logs/server/server.log     the server's, of which there is one
logs/client/client.log     every client's, shared, with pid and boot as fields
logs/shim/<session>.log    one per session's shim
logs/<session>.log         session output, which is not a diagnostic log
```

Read them with `cm logs server`, `cm logs client`, and `cm logs shim <session>`.

Clients share one file rather than having one each. A client is short-lived and there can be one per
attached window, so a file each would accumulate for diagnostics that are read only when something is
wrong; `pid` and `boot` fields identify the writer instead. `boot` distinguishes a reused pid from the
same pid in this boot, since the log outlives a reboot.

Session output sits directly in `logs/` because it is not a diagnostic: it is what the shell printed.
Keeping it out of the subdirectories is what stops `cm doctor` from reporting a build that printed the
word ERROR as a cm fault.

The runtime directory holds `server.sock` and one `shim-NAME.sock` per session. Nothing else is ever
written there, which is why it can live somewhere the system sweeps.

It deliberately does not honour `XDG_DATA_HOME`. That directory is for persistent application data,
which is what the state directory holds; sockets belong in a runtime directory, which is what
`XDG_RUNTIME_DIR` is for, and macOS sets no such variable. Keeping sockets under `TMPDIR` also means an
abandoned one is cleaned up without cm doing anything, where one in a persistent directory would
accumulate. cm copes with a stale socket either way -- a new server binds over it, and `cm doctor`
reports it as `stale-socket` -- but not having to is better.

The cost of that choice is path length. A unix socket path cannot exceed 103 bytes, and the macOS
per-user `TMPDIR` is around 55 characters, which leaves roughly 37 for a session name. Real names are
far shorter than that (`kitty.260` is 9), and `cm doctor` warns through `long-socket-path` before it
becomes a failure. Set `$CM_RUNTIME_DIR` to something shorter if you need the room.

## Example

```toml
# Scrollback retained per session, in lines. 0 means unlimited.
# libghostty prunes at page granularity, so the effective limit is somewhat higher.
scrollback_lines = 10000

# Which of several attached clients sets the session's size:
# "leader" (default), "last-attach", "first-attach", or "smallest".
resize_policy = "leader"

# Diagnostic log level: debug, info, warn, error, or off. Defaults to info.
log_level = "info"

# The key that detaches a client. "none" disables detaching by key, which is useful when a
# program inside the session wants that key for itself.
detach_key = "ctrl-\\"

[env]
# Added to the built-in list. A trailing "*" matches by prefix.
capture = ["MY_TERMINAL_VAR", "PROJECT_*"]

# Removed from the effective list, including built-ins.
exclude = ["SSH_AUTH_SOCK"]

# Replaces the built-in list entirely, ignoring `capture`.
# capture_only = ["TERM", "KITTY_*"]
```

## Resize policy

A session can have several clients attached, which for per-window sessions happens deliberately
rather than often: attaching twice to compare, or following a session to watch it. They may be
different sizes, and only one size can reach the pty.

- **`leader`** (default) gives sizing to the client that last typed. Attaching does not take it from
  whoever is working, and a leader that leaves does not hand it on until someone types, because a
  window nobody touched changing shape is the surprise being avoided.
- **`last-attach`** gives it to the newest client. This was cm's behavior before the setting existed,
  and it means opening a second window on a session reflows the first.
- **`first-attach`** keeps it with the earliest client until it leaves, then passes it to the next
  earliest.
- **`smallest`** fits every client, so nothing is cut off for anyone at the cost of nobody using
  their full window. Each dimension is minimized independently, since a window can be shorter and
  wider at once.

For a single client every policy behaves identically, so the default changes nothing about a normal
session.

Under `leader`, only real typing transfers sizing. A terminal sends much more than keystrokes on the
input channel, and treating any of it as typing would let an idle window take over: mouse motion,
focus changes, and replies to queries the program made are all forwarded to the shell but never claim
sizing. zmx hit exactly this, where a cursor position report handed sizing to whichever window
happened to answer. A key *release* alone does not claim it either, since letting go of a key in a
window you are leaving is not a reason to take it over.

A read-only follower never owns sizing under any policy.

## Logging

The server and shim run detached with their stdio discarded, which is deliberate: inheriting a
client's terminal would tie their lifetime to a window and scribble over the session. Without a log
there is therefore no record of what they did.

That matters more than usual here because cm deliberately swallows a number of errors so that a
failure in something advisory cannot end a session: a title that could not be recorded, a metadata
write that failed, a persisted log that stopped being writable. Each is the right call on its own,
and together they make a system that degrades silently. The rule is that anything swallowed is
logged.

```
cm logs server            # the server's log
cm logs client            # every client's, shared
cm logs shim work         # one session's shim log
cm logs server -f         # follow
cm logs server --all      # include the rotated previous file
```

Logs live under the state directory, are owner-only since they record session names, directories,
and command lines, and rotate at 4 MiB keeping one previous generation. `log_level = "off"` disables
them.

This is the diagnostic log, not session output. `cm history` is what the shell printed.

## Completion

```
cm completions zsh > "${fpath[1]}/_cm"
```

Session names complete dynamically for every command that takes one, annotated with each session's
state, so it is visible that attaching to a `dead` session will restore it rather than join it.
`kill` drops names already on the command line.

Completion never starts a server. It runs on every tab press, and a stray keystroke should not
launch a daemon; with none running there is nothing to complete anyway.

## JSON output

`list`, `info`, `kill`, `get-env`, `tag`, `wait`, `send`, `run`, `status`, `doctor`, `config`, and
`version` accept `--json`. The session-shaped payloads are the ones a script is most likely to build
on, and the notes below are about those.

The shape is a deliberate contract, defined in `cmd/cm/output.go` rather than by marshalling the
wire messages directly. Fields are only ever added, never renamed or removed, and a test asserts
the exact key set so a change cannot slip through unnoticed. Marshalling the protobuf message would
have exposed fields kept only for compatibility, inviting scripts to depend on the ones being
phased out.

```
$ cm list --json | jq -r '.[] | select(.state=="running") | "\(.name) -> \(.cwd)"'
work -> /home/user/projects
```

Details worth knowing when scripting against it:

- An empty list is `[]`, never `null`, so a script can iterate unconditionally. Same for
  `killed` and `errors` in `kill --json`.
- `state` is `running`, `exited`, or `dead`. Prefer it over `exit_code`, which means nothing for a
  dead session, since that outcome is unknown rather than observed.
- `cwd` is empty when the session reported a directory on another host, because acting on a remote
  path locally would open the wrong place or fail. `cwd_uri` still carries the reported value, so a
  caller can distinguish "remote" from "nothing reported".
- `kill --json` reports partial failure in the payload *and* exits non-zero, so a script can check
  the status without parsing.
- Ordering is stable: oldest first, ties broken by name.

## Detach key

`detach_key` accepts `ctrl-<key>` for a letter or one of the punctuation characters that have a
control code (`[`, `\`, `]`, `^`, `_`, `?`, `@`), and `none` to disable detaching by key. The
default is `ctrl-\`.

`cm attach --detach-key` overrides this for a single attachment, and takes precedence over the
config file. That exists for the case where something outside the client already claims the key:
attaching to cm from inside another multiplexer means the outer client sees ctrl-\ first and the
inner one never receives it, so the window closes instead of detaching. A per-attachment flag fits
that better than a setting, since it is usually one window rather than every one.

Configurable because that combination is awkward or unreachable on some keyboard layouts, and
disableable because a program running in the session may want the key itself.

`cm detach [session]` is the same operation without a key, and it exists because two cases are
outside what any key can do. A key is delivered to whichever client owns the real terminal, which is
the outermost one, so from a nested attach it detaches the parent however it is bound. And a script
or an agent driving sessions has no keyboard, so before this there was no route to detaching at all.

Making the key itself pick a target was considered and rejected. It would have to choose from
server-side state the user cannot see, so the key would stop meaning one thing, and a wrong guess
against an owned session ends a shell rather than releasing it. Both outcomes of the current
behavior are recoverable by pressing the key again; that asymmetry is what decided it. Naming the
session is explicit and needs no rule.

Whatever key is chosen is detected in three encodings: the raw control byte, the kitty keyboard
protocol form, and xterm's modifyOtherKeys form. All three are necessary. A terminal with either
protocol active reports a modified key as an escape sequence rather than a control byte, so
checking only the byte means the key silently stops detaching for exactly the users most likely to
have those modes on. zmx hit this with a program that enables modifyOtherKeys on startup.

## Session environment

The `[env]` section controls which environment variables follow a client into a session.

### The problem it solves

A terminal emulator describes itself to its child through the environment, and a session's shell
captures those values once, when it starts. Reattaching from a different terminal, or from the
same terminal after it restarted, leaves the shell holding values that describe a terminal which
no longer exists.

kitty's `KITTY_LISTEN_ON` is the sharp case: every `kitten @` call goes through that socket, so
once kitty restarts, remote control, notifications, and anything scripted against kitty's API all
fail at once. `SSH_AUTH_SOCK` has the same shape with a different symptom, since a long-lived
session quietly loses the ability to push to git.

Nothing outside a process can change its environment, so the shell has to ask. cm records what
the most recent client had; `cm get-env` prints it and a shell hook applies it.

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
cheap and does not re-export everything on every prompt. `--all` prints everything recorded.

The default output is plain `name=value` lines, with a leading `-` marking a variable the client
no longer has. `--format` selects shell-eval output instead. There is deliberately no shell
auto-detection: `$SHELL` is the login shell, not the one running in the session, and those differ
precisely when guessing wrong is most confusing. This is also why POSIX is not the default. tmux
always emits POSIX, which is broken in fish on every count, since a bare assignment there is a
per-command prefix, `export` is not a builtin, and unset is `set -e`.

### Removals matter as much as values

A variable that vanished, rather than changed, keeps its old value if only assignments are
emitted. That is worse than having no value at all: a client tries the stale socket and fails
instead of falling back. So `get-env` reports removals explicitly, and only for variables cm
manages, so a hook can never delete unrelated parts of the environment.

### What is captured by default

Variables a terminal or session manager sets to describe itself, all of which go stale when that
terminal is replaced: `TERM`, `COLORTERM`, `TERMINFO`, `TERM_PROGRAM`, `WINDOWID`, the `KITTY_*`,
`GHOSTTY_*`, `ITERM_*`, `WEZTERM_*`, `ALACRITTY_*` and `FOOT_*` families, `VTE_VERSION`,
`DISPLAY`, `WAYLAND_DISPLAY`, and the `SSH_*` connection and agent variables. See
`internal/sessionenv` for the exact list.

It is a curated list rather than the client's whole environment on purpose. A session record is a
file on disk, and a developer's environment routinely holds API tokens and credentials that have
no business being written there.
