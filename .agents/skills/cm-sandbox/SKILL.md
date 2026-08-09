---
name: cm-sandbox
description: "Run cm by hand without touching the developer's own sessions. Use whenever manually testing cm during development: starting a server, creating sessions, attaching, or reproducing a bug. Required before any cm command that mutates state, because a bare cm in this repo talks to the live server holding real work."
---

# cm sandbox

The person working on cm is usually *using* cm. Their server holds sessions with real work in them, and
a bare `cm` command in this repo talks to that server. `cm kill --all` or `cm server stop` would end
their sessions.

So manual testing happens in a sandbox: its own runtime dir, state dir, and config, which is all the
isolation cm needs because it resolves its socket from `CM_RUNTIME_DIR`.

```
.agents/skills/cm-sandbox/scripts/cm-sandbox.sh
```

## Workflow

```sh
S=.agents/skills/cm-sandbox/scripts/cm-sandbox.sh

$S new t1                                  # create it, start its server
$S check t1                                # prove it is isolated (do this the first time)
$S run t1 list                             # any cm command
$S run t1 attach --no-attach worker
$S run t1 run -- /bin/sh -c 'echo hi'
$S ps t1                                   # this sandbox's server and shims
$S rm t1                                   # sessions, then server, then directories
```

To use `cm` directly in your own shell rather than through `run`:

```sh
eval "$($S env t1)"
cm list
```

`new` builds `bin/cm` from the working tree and uses that, so a sandbox tests your changes rather than
the installed binary. Testing an installed `cm` while editing the source is a false pass and the output
does not say which ran. Override with `CM_SANDBOX_BIN` if you need a specific build.

## Prove it, do not assume it

`$S check` prints the directories cm actually resolved and where each came from, which is the difference
between isolated and hoping:

```
runtime_dir   .../cm-sandbox/t1/r ($CM_RUNTIME_DIR)
state_dir     .../cm-sandbox/t1/s ($CM_STATE_DIR)
detach_key    ctrl-\
```

If `runtime_dir` is not under the sandbox, it is not isolated. `cm config` is the authority here because
it reports resolved values with their source, so a mistyped variable shows up as "default" rather than
silently doing nothing.

`detach_key` is a second, independent signal. It reads `ctrl-\` in a sandbox and the developer's own
setting outside one, so the wrong value there means the real config file is being read.

## The `CM_CONFIG` trap

`CM_CONFIG` must point at a file that **does not exist**, not be empty.

An empty value means *unset*, so cm falls through to `XDG_CONFIG_HOME` and then to the real config file.
This is not hypothetical: an entire e2e suite read the developer's `detach_key = ctrl-o` for weeks while
looking isolated, and nothing failed, because the tests did not assert on the key.

Verified both ways:

```sh
CM_CONFIG= cm config | grep detach_key                     # ctrl-o  <- the developer's file
CM_CONFIG=/nonexistent.toml cm config | grep detach_key    # ctrl-\  <- the default
```

The script sets it to `absent.toml` inside the sandbox for this reason.

## Cleaning up

`$S rm NAME` does it in the order that matters: kill sessions, stop the server, then delete the
directories.

Sessions before the server, because a shim outlives its server and holds a pty. macOS caps ptys at 511
system-wide, and exhaustion arrives as `pty.Start() error = device not configured` in whatever runs
next, which looks like a bug there rather than a leak here. A previous version of the e2e harness leaked
one per session and had accumulated 437 stray ptys before anything noticed.

`$S rm-all` removes every sandbox this script created. It only touches paths under
`$TMPDIR/cm-sandbox`, so it cannot reach the developer's sessions or another tool's.

## Never pattern-match on "cm" to kill things

`$S ps` and the script's cleanup match on the sandbox's **own runtime directory**, never on the string
`cm`. A bare `pkill -f cm` would match the developer's real server, every other sandbox, and unrelated
processes.

If you have to clean up something outside a sandbox, do not guess. Read what the process actually is:

```sh
ps -o command= -p PID
```

The developer's own server is the one whose `--runtime-dir` is under `$TMPDIR/cm-501` (or similar) with
a `--state-dir` in `~/.local/state/cm`. Leave it alone.

Before killing anything you did not start, verify it is really abandoned. `clients=0` is not enough: a
detached session can hold a live shell with unsaved work. Check whether the directory still exists,
whether the process has children, and prefer `cm kill NAME` over signalling a pid.

If in doubt, leave it running and say so. An orphaned server costs a few MB; killing a live one costs
someone's work.

## Testing a terminal, not just a session

A sandbox gives you an isolated cm, not a terminal. Anything that depends on real rendering, real
keypresses, or a real pty on the other end -- attach, detach, screen restore, the detach key -- needs a
terminal too. Use the `kitty-sandbox` skill for that, and point the cm inside it at a sandbox from here.

Do not run `cm attach` from a tool call against the developer's terminal. It takes over the terminal
that invoked it, and the detach key belongs to whatever is outermost, so the window can end up
unusable.

## When a session behaves oddly

Inside a sandbox these are safe and are usually faster than reasoning about it:

```sh
$S run t1 doctor            # checks that encode previous incidents; exits non-zero on a finding
$S run t1 status            # pid, uptime, session counts
$S run t1 logs server -n 50
$S run t1 logs shim NAME    # one session's own log
```

`doctor` exiting non-zero is a report, not a malfunction. Read what it printed.

Two findings are expected in a sandbox and mean nothing is wrong: `no-shell-integration`, because a
`/bin/sh` session emits no OSC 133, and `long-socket-path` if `$TMPDIR` is deep.
