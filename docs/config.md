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

`cm shell-init` prints the completions ahead of the integration, so a startup file needs one cm
invocation for both rather than two, at a measured 23ms each:

```
eval "$(cm shell-init zsh)"
```

In zsh that has to come after `compinit`: the completion half asks the shell to register a
completion function and needs that machinery in place, while the integration half does not. Where
the ordering is awkward, the two are still separable, and this is what a setup that predates the
bundling looks like:

```
cm completions zsh > "${fpath[1]}/_cm"
eval "$(cm shell-init zsh --no-completions)"
```

Caching the output is worth it either way, since both halves are the same bytes on every startup.
Loading the completions twice is harmless, so a stale cache alongside the bundled form costs
nothing beyond the time to parse it.

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
- `cwd` is absolute, and stays absolute. The `cm list` table abbreviates a path under home to
  `~/...`, but that is a rendering for a person: `~` only expands in a shell, so a Go or Python
  caller handed one would create a directory named `~`. `cm info --field cwd` is absolute for the
  same reason, since a terminal emulator opening a window there passes it to a syscall.
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
server-side state the user cannot see, so the key would stop meaning one thing, while both outcomes
of the current behavior are recoverable by pressing it again. Naming the session is explicit and
needs no rule.

The argument used to be sharper than that, and is recorded because the reasoning changed rather
than the decision: while sessions could be owned, a wrong guess ended a shell instead of releasing
it, so the two options were not equally recoverable. Ownership is gone, so a wrong guess now only
detaches the wrong client. The predictability argument is what still decides it.

Whatever key is chosen is detected in three encodings: the raw control byte, the kitty keyboard
protocol form, and xterm's modifyOtherKeys form. All three are necessary. A terminal with either
protocol active reports a modified key as an escape sequence rather than a control byte, so
checking only the byte means the key silently stops detaching for exactly the users most likely to
have those modes on. zmx hit this with a program that enables modifyOtherKeys on startup.

## Session environment

The `[env]` section controls which environment variables follow a client into a session.

### Two mechanisms, for two different questions

They differ in *when* they apply, which decides everything else about them, and conflating them was a
bug rather than a hypothetical:

- **Forwarded at creation.** A new session's shell starts from the environment of the client that
  created it, less the dynamic linker variables (`NoInherit` in `internal/sessionenv`). It answers
  "what environment is this shell born with", and it is not configurable, because the useful answer is
  "the same as the thing that created it".
- **Captured on every attach**, and served by `cm get-env` to a shell that is *already running*. It
  answers "what has changed about the terminal since this shell started": `TERM`, the `KITTY_*`
  family, `SSH_AUTH_SOCK` and the rest, in `DefaultCapture`. The `[env]` settings below configure
  this list, and only this list.

Forwarding is what makes a session resemble the thing that opened it. Created by hand from a shell, it
gets that shell's environment, the way a subshell would. Created by a terminal emulator's integration,
it gets the emulator's, which is close to fresh because such a client has no shell between it and the
service manager that launched it: 14 variables against a login shell's 60, measured here. Neither
needs a flag, because the client's own ancestry is the signal.

The environment is fixed when the shell starts and never rewritten afterwards. That is the same
contract a terminal split has, and picking up a changed shell config by closing a window and opening a
new one is the intended workflow rather than a limitation. Refreshing a *running* shell is what
`get-env` is for, and it is deliberately limited to the captured list, since rewriting `PATH` under a
live shell would fight whatever it and its tooling had since done to their own.

Two consequences worth stating plainly. A client's secrets reach the sessions it creates, exactly as
they reach a terminal split started from the same shell; nothing is filtered by name. And nothing
forwarded is written to disk: the captured list is recorded in the session record, which is a file, and
it is an allow-list for that reason, while a forwarded environment only ever reaches the shim's own
process environment.

The exception is the dynamic linker variables, which are dropped. `LD_PRELOAD`, `LD_AUDIT`,
`LD_LIBRARY_PATH`, and the `DYLD_*` family choose what code a process loads rather than how it behaves.
sshd defaults `PermitUserEnvironment` to `no` and names `LD_PRELOAD` as the reason; cm has no
equivalent trust boundary to defend, since the client is a local process already running as you, but
the exclusion is cheap and has a precedent where a broader denylist guessing at which names hold
secrets would have neither. An explicit `--env LD_PRELOAD=...` is still honored, because that is a
request rather than something travelling silently.

`SHELL` is forwarded like anything else, which does mean a session runs the shell its creating client
named. That is usually what you want and is worth knowing, since it also decides which program the
shim execs.

### Why a session's environment is not the server's

A session's environment used to come entirely from the server, because the server spawns the shim
and the shim spawns the shell. The server inherits from whatever shell started it, so a server
running for weeks handed every new session a weeks-old environment.

The symptom was a `PATH` entry that had been deleted from the dotfiles the day before still
appearing in sessions created today, and every obvious escape fails: `exec zsh -l` re-reads config
but still inherits the stale value, `typeset -gU path` only reorders and keeps obsolete tail
entries, and `cm server restart` from a shell that already has the stale value pins it again. Only
restarting from a fresh window fixed it, taking `PATH` from 88 entries to 27.

Two measured facts hold the fix up, both asserted in tests rather than assumed:

- Go's `exec` keeps the **last** occurrence of a duplicated name, which is what makes appending an
  override rather than a silent no-op.
- `exec.Command` does **not** resolve a program name against `cmd.Env`. Seeding the variable is
  therefore not sufficient by itself; it works because the server puts these values in the shim's
  own environment, which the shim then inherits, so a bare command name resolves against the right
  `PATH`.

The server's environment remains the fallback for everything nobody else supplies, which is what a
session created by something with no environment worth copying should get.

### The problem the captured list solves

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
