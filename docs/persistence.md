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

There is one revive path, whatever ended the session: it exited, it died with a reboot or a crash, or
`cm shelve` ended it. `cm attach` or `cm restore` brings it back in its recorded directory, unless `--dir`
names another, with the saved output replayed and the restore behavior below deciding what runs. A
command after `--` on the attach is ignored, with a notice on stderr. A terminal restoring a window it had
opened with `cm attach NAME -- ARGV` replays that argv, so letting it win meant the window reran its
original program after a reboot and buried the resume command its program had declared, while the same
session shelved came back with it. The states now differ only in how the session got there, its label,
and its retention.

This matches how the terminal integration already works: kitty's session file replays one
`cm attach <name>` per window, so restore is already driven by the terminal opening rather than by
cm. A global restore at boot would additionally spawn a shell for every session that ever existed,
including the ones never reopened.

## Which sessions persist

Opt-in, either per session or by name pattern. The two are independent: `--persist` on one session
needs no config, and `enabled` turns on only the patterns in `sessions`.

```toml
[persist]
enabled = true
sessions = ["kitty.*"]      # name patterns that persist automatically
on_restore = "shell"        # shell | none | prefill | command
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

**The choice is made when the session's shim spawns, and cannot be changed later.** The log path goes
into the shim's argv, so `--persist` on an already running session is ignored, and editing the config
and restarting the server changes nothing: adoption reconnects to the surviving shims rather than
respawning them. Making a session persist means ending its shell and reattaching, which starts a new
shim, and the content from before is gone because it only ever lived in the old shim's memory. What
that costs, and the two ways out, are in [ideas.md](ideas.md).

## What happens on restore

Four behaviors, chosen by config with a per-session override. The override is `--on-restore` on the attach
that creates a session, and it is kept with the session's record, shown by `cm info` as `on_restore`, so a
reboot or shelving does not lose it: a revive uses its own `--on-restore` if it passes one, which then
becomes the session's, else the session's, else the config's. A session's own `command` counts as asking
only for what its attach started. On a plain attach that is the spawn argv, so `cm attach
--on-restore=command NAME -- kitten ssh host` reconnects without `safe_commands`. With `--in-shell` it is the
command the attach typed, and a declaration that command makes, until it returns; the record keeps which
frame that is. Whatever is typed into the session later gets the config's `safe_commands`, and is prefilled
otherwise. A revive's own `--on-restore=command` covers everything that revive brings back, and a command it
types back keeps the grant. Before it was kept, only the reviving attach's value was seen, and a terminal
restoring a window passes none, so `--on-restore=command` lasted one attach.


- `shell` (default) starts a fresh shell in the recorded directory. Safe, and right for per-window
  sessions.
- `none` leaves the restored content as read-only history and starts nothing until asked. Nothing is
  re-executed.
- `prefill` puts the command in a fresh shell's line editor, unrun.
- `command` runs the command when the session asked for it or its program is in `safe_commands`, and
  prefills it otherwise.

The command is, in order: the one a program declared with `cm restore-with`; else the typed command, the
line the shell's outermost command frame was running when the session ended, which cm keeps with the
record as `typed_command`; else the spawn argv, kept as an argv list so a quoted argument comes back as one,
when it is a program rather than just a shell; else nothing. A `restore-with --clear` in a frame still open
means nothing comes back for it, not even the typed command.

A declared command is how a program whose state is not in its argv comes back as itself: `claude` re-run
is a new conversation, `claude --resume <id>` is the old one. A typed command is how everything else does:
`sleep 999`, a `watch`, a `gr` typed at a prompt. Both start in the shell at its first prompt, never exec'd
as the session's command, so quitting them leaves a prompt; a spawn argv that runs is exec'd in place, as it
always was. cm's shell integration starts the command itself, with nothing typed after the prompt; a shell
without it has the line typed in. See [restore-command.md](restore-command.md).

### The allowlist is a convenience, not a safety boundary

`safe_commands` lets matching sessions restore their command without a per-session flag. It matches
the **program name only**, so `nvim` in the list also matches `nvim -c ':!rm -rf /tmp/x'`. That is
acceptable on a personal machine but must not be mistaken for a guarantee: the per-session override
is the actual control, and the default remains `shell`.

A typed command goes to a shell, which runs all of it, so the allowlist alone would let `sleep` in the list
run `sleep 1; rm -rf /tmp/x`. A typed line therefore runs only when it is a plain program invocation, a
program word and argument words, whatever the allowlist or the session's `--on-restore=command` says;
anything else is prefilled at most. Words may be quoted with `'...'` or `"..."`, and the program word is
matched against the list with its quotes removed. Not plain, and so never run:

- operators and grouping: `;` `|` `&` `<` `>` `(` `)` `{` `}`, which covers `&&`, `||`, `>>` and here-documents;
- expansions: `$` and a backtick, outside quotes or inside double quotes, which covers `$(...)`;
- globs: `*` `?` `[` outside quotes, and `#` and `^`, which zsh's extended globbing reads as patterns;
- history: `!` outside single quotes, and a line starting with `^`, which is quick substitution;
- a backslash outside single quotes, which covers a continuation, and a newline;
- a `~` that is not a word on its own or the start of `~/`, so `~user` and zsh's `a~b` exclusion;
- a word starting with `=`, which zsh expands to a command's path;
- a first word that assigns, `VAR=value`;
- an unbalanced quote, an empty line, and a line at the 256-character cut.

What it cannot see is what a word means to the shell: an alias or function with an allowlisted program's
name runs as itself. The spawn argv is exec'd rather than given to a shell, so none of this applies to it. Because the command is re-run
*verbatim*, a session whose program cannot resume from scratch should be left off the list. `gemini` restarted fresh loses its conversation, so it restores a shell
instead, which is the honest outcome.

## Where the log is written

The shim writes it. It already owns the raw pty bytes and appends them to an in-memory log, so
persisting is one extra write. It also keeps working when the server dies, and the raw byte log is
exactly what the server replays through libghostty to rebuild a screen.

The server was the alternative, since it holds the terminal model and could write a compact screen
snapshot instead of raw bytes. Rejected because it dies with the server and cannot capture output
the server never consumed.

## Retention

Bounded by lines, matching how `scrollback.lines` is expressed, with a byte ceiling as a backstop. The
shim deliberately knows nothing about terminals, so a line is a newline in the byte stream, and a session
emitting very long lines would otherwise produce a far larger log than its line count suggests. Defaults
are 10000 lines and 16 MiB. A full-screen program positions the cursor rather than writing newlines, so
for nvim the byte ceiling is the limit that applies.

The log is a directory, `logs/<id>.log.d/`, of segment files named by the sequence number of their first
byte, zero-padded so `cat *` in name order is the log. The shim seals a segment at the first newline past
1 MiB, or at 2 MiB if none comes, and retention deletes the oldest sealed segments while what remains still
covers both limits. So the log keeps up to one segment more than the limits ask for, and an append is a
write to the end of one file however much is retained. There is no manifest: it would be a second record
that can disagree with the files.

The alternative, one file trimmed from the front, has to rewrite everything retained to drop a prefix, and
once the log is full it does that on every append, on the shim's pty read loop. Measured on an M3 Max, one
1022-byte pty read at the default limits:

```
single file, full   5085374 ns/op   0.20 MB/s
segments, full         2895 ns/op    353 MB/s
```

That was 50 to 150ms of stalled output per half-page scroll in an nvim vertical split, which is how it was
found. `BenchmarkAppendAtCap` in `internal/seqlog` measures both.

Older shims write the single file, `logs/<id>.log`, and are given `--persist-path` for it. A current server
passes `--persist-dir` instead, reads either layout, and moves a single file into segments when it revives
the session that wrote it, keeping the numbering. A session started before an upgrade keeps its old shim and
its single file until it ends.

Older scrollback is lost across a reboot. A restored session has the most recent output, not all of
it.

## Lifecycle

A dead session with a saved log stays listed, marked restorable, so it can be found and revived.
It is removed after `expire_after`, defaulting to a week.

A shelved session is the deliberate form of that: `cm shelve` ended it so it could be restored as it
was, and it is listed as `shelved` rather than `exited` or `dead`. It does not expire unless
`--expire-after` gave it a retention, and `cm forget --ended` leaves it. Those are the only differences: it
revives on the same path as any other ended session, under "What triggers a restore". `cm run --session` refuses a shelved
session rather than restoring it, while it revives an exited or dead one with the run's own command, as it
always has. Shelve is not SIGTSTP. See [cli.md](cli.md#shelving-a-session-and-restoring-it).

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

### A session's own retention

`cm kill NAME --keep --expire-after DURATION` replaces both intervals for one kept session, with a Go
duration or `never`. It exists for suspending: kspaces stops a session's programs with `cm kill --keep` and
revives it later with `cm attach`, and on the schedule a session suspended for longer than `expire_after`
lost its record, its saved output and its declared resume command. `never` keeps it until `cm forget` or
`cm kill` removes it:

```sh
cm kill work --keep --expire-after never   # keep it restorable indefinitely
cm attach work                             # revives it, and clears the retention
```

The retention lives in the session's row (`expire_after`, nanoseconds, `-1` for never), so it survives a
server restart or upgrade, and it is measured from the same `updated_at` as the schedule. A revive
recreates the record without it, so a revived session that ends later follows the schedule again unless it
is kept with a retention of its own again. `cm info` shows it as `expire_after`, empty when unset.

An older server reads the field as absent and would keep the session on the schedule while reporting
success, so the client checks the `kill.expire-after` capability first and refuses when the server
conclusively lacks it.

Rejected: a config-level pattern (`persist.keep_forever = ["kspaces-*"]`). Whether a session should outlive
the schedule is decided at the moment it is suspended, by the tool suspending it, not by its name.

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
path, which re-transmits with `a=t` after the screen and then places with `a=p`.

Rejected: replay into a throwaway terminal, serialize it, hand the bytes to the first client to attach,
discard them. It breaks all three. A later client gets a session with no history, a read sees only what
the new shell has printed, and the blob bypasses the transmit-then-place split, so a restored image is
drawn wherever the cursor happens to be and then erased by the screen that follows it.

The seeded content carries no sequence position. Those numbers belonged to the dead incarnation, and the
model starts its own log at zero, so it holds history with no position plus, once the pump runs,
everything the new log does account for. That is the pairing attach already relies on.

The seed stops where the previous run ended, as read before the new shim is spawned. A revived session
keeps its ID and so its log directory, and the new shim appends to it, continuing the numbering so a stored
position keeps naming the same byte. The seed runs after the spawn, and a program prints faster than it
does, so reading to the end fed the new run's first output from disk and then again from the pump: every
revived session showed its first line twice in `cm read` and `cm history`, three revivals out of three in
a sandbox. The bound is explicit rather than zero meaning "to the end", since a previous run that printed
nothing leaves a real bound of zero. `TestRevivedSessionShowsEachRunOnce` drives a real revive, the only
path that exercises the read-before-spawn order: with the read moved after the spawn it failed 7 runs of
7.

Query responses the emulator generates during replay are dropped. They answer questions a program
asked before the reboot, and that program no longer exists, so delivering them would inject stray
input into a new shell.

The program's terminal modes are reset after the replay, for the same reason. A reboot kills it without
its teardown, so a log ending inside nvim left the model on the alternate screen with kitty keyboard flags
3, focus and mouse reporting on. Every client attaching was put in those modes: kitty sent ctrl-c as
`CSI 99;5u` plus a release event, and the new zsh read both as typing. `vt.Terminal.ResetModes` sets every
mode to a new terminal's value, empties both keyboard stacks, and resets the scroll region, pen, charset,
modifyOtherKeys and cursor style. Defaults come from a fresh terminal rather than a list of known
offenders, which is tmux's approach on `respawn-pane` (`screen_reinit`).

The alternate screen is left, and the dead program's last screen with it. kitty's save-on-logout, iTerm2
restoring into a new process, and tmux's respawn all do the same and show the main screen. Flattening it
onto the main screen was considered: zellij and tmux-resurrect end up there, because they capture whichever
screen is active rather than by choice. Rejected because a frozen editor above a prompt reads as a live
one, and a shell left drawing on the alternate screen has no scrollback.

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
