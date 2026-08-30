# Persistence across a reboot

**Status: implemented.** Verified end to end by killing every cm process, removing the sockets, and
attaching again: the content comes back with a fresh shell beneath it.

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
forget_unpersisted_after = "5m"  # remove ended sessions that saved nothing after this
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

A session that ended *without* saving a log is a different case, and gets `forget_unpersisted_after`,
defaulting to five minutes. Nothing about such a session can be recovered: with no log path,
`cm history` can only report that its output is gone. The only reason to keep the record at all is
that `cm run` reads the exit status back through `list`, which takes seconds.

Two intervals rather than one because sharing them makes `cm list` useless. Every `cm run`, including
every short command in a script, leaves a record, so a single week-long lifetime meant twenty test
invocations buried the sessions actually being worked on. Five minutes is generous for reading an
exit status and short enough that the list stays about live sessions.

The sweep runs every minute, which is set by the shorter of the two intervals: an hourly sweep tuned
for week-old records would leave every finished command listed until the next tick.

Expiry is necessary rather than tidy: without it both the session list and the disk grow forever
across reboots, and the session counter is already past 260 on the machine this is built for.

## How it fits together

The shim mirrors pty output to a file (`internal/seqlog/file.go`). The file records the sequence
number its first retained byte carries, since trimming means the first byte is not sequence zero;
without that a replay would number output from zero and disagree with every stored position.

On attach to a dead session, the server reads the record before deleting it and carries the log path
and directory into the options that recreate the session. The log is then replayed into the new
session's own terminal model before the output pump starts (`seedFromPersistedLog` in
`internal/server/restore.go`), and the images it contains are recorded in the session's graphics store
on the way past.

Revival is deliberately the same operation as adoption after a server restart, differing only in where
the history comes from: a living shim holds it in memory, a rebooted one left it on disk. `Manager.adopt`
seeds from the log in the same place it replays a shim's retained history, so both arrive as a model that
already holds the screen.

Seeding the model rather than producing a blob is what makes the rest of cm work on a revived session
without being taught about persistence. Every client is served from the model, so a second one sees the
pre-reboot screen too; `cm read` and `cm history` see it; and the images travel the ordinary restore
path, which re-transmits with `a=t` and places with `a=p` after the screen.

Rejected: replay into a throwaway terminal, serialize it, hand the bytes to the first client to attach,
discard them. It breaks all three. A later client gets a session with no history, a read sees only what
the new shell has printed, and the blob bypasses the transmit-then-place split, so a restored image is
drawn wherever the cursor happens to be and then erased by the screen that follows it.

The seeded content carries no sequence position. Those numbers belonged to the dead incarnation and the
new shim numbers from zero, so the model holds history with no position plus, once the pump runs,
everything the new log does account for. That is the pairing attach already relies on.

Query responses the emulator generates during replay are dropped. They answer questions a program
asked before the reboot, and that program no longer exists, so delivering them would inject stray
input into a new shell.

`on_restore = "none"` runs a holding process rather than nothing, because the rest of cm assumes a
session has a pty and a child: without one there is nothing to attach to, resize, or read from.

## Failure behavior

Deliberately biased toward keeping a session usable:

- A corrupt or unreadable log is reset rather than rejected, since refusing to open a session
  because of an unreadable cache trades something recoverable for something that is not.
- A replay that fails leaves the session working but empty.
- A write failure mid-session stops persisting rather than ending the session.
- A log that cannot be deleted during expiry does not keep the record alive: the row is what makes a
  session visible, and an orphaned file is the smaller problem.

But an unusable persist *path* fails loudly at creation, because silently not persisting is
discovered only after a reboot, when it is too late to matter.
