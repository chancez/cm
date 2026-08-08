# Persistence across a reboot

**Status: designed, not implemented.** This records the decisions so the implementation does not
have to relitigate them.

## What can and cannot survive

A pty is a kernel object and a shell is a process, so both are gone unconditionally after a reboot.
What can survive is the *content*: scrollback, the last screen, the working directory, the title,
and the command that was running.

So "restore" only ever means "show me what was there, and optionally start something fresh". It
never means "my process is still running". Content persistence and process persistence are different
guarantees, and blurring them in the UI would be dishonest.

## What triggers a restore

Attaching does. Nothing happens at boot.

`cm attach kitty.55` on a dead session with a saved log replays the log, rebuilds the screen, and
applies the configured restore behavior. A session that is never reopened stays dormant and costs
nothing.

This matches how the terminal integration already works: kitty's session file replays one
`cm attach <name>` per window, so restore is already driven by the terminal opening rather than by
cm. A global restore at boot would additionally spawn a shell for every session that ever existed,
including the ones never reopened.

## Which sessions persist

Opt-in, either per session or by name pattern:

```toml
[persist]
enabled = true
sessions = ["kitty.*"]      # name patterns that persist automatically
on_restore = "shell"        # shell | none | command
max_lines = 10000           # retained output per session
expire_after = "168h"       # remove dead persisted sessions after this
safe_commands = ["nvim", "vim", "less", "htop"]
```

```
cm attach work --persist                    # opt in explicitly
cm attach scratch                           # not persisted
cm attach edit --persist --on-restore=command -- nvim notes.md
```

Opt-in rather than universal because writing every session's terminal output to disk continuously
is a real cost, and most sessions are not worth it. The per-window sessions are.

## What happens on restore

Three behaviors, chosen by config with a per-session override:

- `shell` (default) starts a fresh shell in the recorded directory. Safe, and right for per-window
  sessions.
- `none` leaves the restored content as read-only history and starts nothing until asked. Nothing is
  re-executed.
- `command` re-runs the recorded command verbatim.

### The allowlist is a convenience, not a safety boundary

`safe_commands` lets matching sessions restore their command without a per-session flag. It matches
the **program name only**, so `nvim` in the list also matches `nvim -c ':!rm -rf /tmp/x'`. That is
acceptable on a personal machine but must not be mistaken for a guarantee: the per-session override
is the actual control, and the default remains `shell`.

Because the command is re-run *verbatim*, a session whose program cannot resume from scratch should
be left off the list. `gemini` restarted fresh loses its conversation, so it restores a shell
instead, which is the honest outcome.

## Where the log is written

The shim writes it. It already owns the raw pty bytes and appends them to an in-memory log, so
persisting is one extra write. It also keeps working when the server dies, and the raw byte log is
exactly what the server replays through libghostty to rebuild a screen.

The server was the alternative, since it holds the terminal model and could write a compact screen
snapshot instead of raw bytes. Rejected because it dies with the server and cannot capture output
the server never consumed.

## Retention

Bounded by lines, matching how `scrollback_lines` is expressed, with the oldest output dropped as
the file grows.

One wrinkle to implement carefully: the shim deliberately knows nothing about terminals, so it has
no concept of a line beyond counting newlines in the byte stream. It counts them on append and trims
whole lines from the front. That means a session emitting very long lines produces a larger file
than the line count suggests, so a byte ceiling also applies as a backstop; a single pathological
line must not be able to fill the disk.

Older scrollback is lost across a reboot. A restored session has the most recent output, not all of
it.

## Lifecycle

A dead session with a saved log stays listed, marked restorable, so it can be found and revived.
It is removed after `expire_after`, defaulting to a week.

Expiry is necessary rather than tidy: without it both the session list and the disk grow forever
across reboots, and the session counter is already past 260 on the machine this is built for.

## Implementation order

1. Disk-backed `seqlog` behind the existing interface, with the line and byte bounds.
2. Shim writes to it for sessions marked persistent, and recovers its position on startup.
3. Server replays a saved log through libghostty on attach to a dead session.
4. Restore behaviors, config, and the per-session flags.
5. Expiry, run at server startup and periodically.

Steps 1 and 2 are useful on their own: they make a session survive the shim being killed, not only a
reboot.
