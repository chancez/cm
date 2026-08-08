# Configuration

Configuration is optional. cm works with no file, and every setting has a default that suits the
common case.

The file is TOML, read from `$XDG_CONFIG_HOME/cm/cm.toml` (`~/.config/cm/cm.toml` on Linux,
`~/Library/Application Support/cm/cm.toml` on macOS). Override with `--config` or `$CM_CONFIG`.

An unknown key is an error rather than being ignored, because a misspelled setting is otherwise
indistinguishable from one that has no effect.

## Example

```toml
# Scrollback retained per session, in lines. 0 means unlimited.
# libghostty prunes at page granularity, so the effective limit is somewhat higher.
scrollback_lines = 10000

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

## Detach key

`detach_key` accepts `ctrl-<key>` for a letter or one of the punctuation characters that have a
control code (`[`, `\`, `]`, `^`, `_`, `?`, `@`), and `none` to disable detaching by key. The
default is `ctrl-\`.

Configurable because that combination is awkward or unreachable on some keyboard layouts, and
disableable because a program running in the session may want the key itself.

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
