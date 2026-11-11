# Program resume protocol (draft)

**Status: the sequence is a draft and unimplemented; cm's side is implemented without it.** A proposal for a
sequence a program writes to say how it should be started again when its window is restored, plus how cm
implements the same thing through `cm restore-with`. Written to be proposed to
terminals beyond cm, ghostty first, so the spec half avoids anything cm-specific.

## The problem

A terminal or multiplexer that restores windows after a restart can bring back the content and the
directory, and then has to guess what to run. Every tool surveyed guesses from the argv of the foreground
process: tmux-resurrect, zellij, kitty's `save_as_session --use-foreground-process`, resurrect.wezterm.
The argv is the wrong answer for any program whose useful state is not in its argv. `claude` restarted
from its argv is a new conversation; `claude --resume <id>` is the old one, and only the program knows
the id.

The workarounds read the program's private state (agent-manager parses each agent's session files) or
hook the program from outside. Asking the program is a fixed cost; recognizing it is a treadmill, the same
argument that made OSC 7501 worth having.

## Prior art

No escape sequence for this exists or has been proposed, as of 2026-11-10: searched ghostty, kitty,
iTerm2, WezTerm, contour, Windows Terminal and the terminal-wg tracker. Terminals that restore windows stop
short of programs on purpose. kitty's `restore_session_after_logout` does not start programs other than the
shell "since they could have been run for their effects, like `rm`", and kitty discussion #10828 asking for
resume-id handlers was pointed at watchers rather than core support. ghostty's `window-save-state` restores
layout and directory; Windows Terminal's `persistedWindowLayout` the same. All of them are missing the one
fact only the program has.

- zellij `set-resurrect-command`, PR #5769, merged 2026-10-26: a program declares its command through
  the zellij CLI. Out of band, so only zellij can receive it. The declaration lasts until cleared or the
  pane closes, so a pane back at its shell prompt still resurrects into the program that left. The
  discovery hook beside it joins and splits argv on whitespace, losing quoting.
- macOS Terminal `TERM_SESSION_ID`: the terminal hands each session an id through the environment and the
  shell saves its own restore state keyed to it. The precedent for a terminal passing a per-session value
  through the environment for exactly this purpose.
- kitty remote control over the tty requires `remote_control_password` or `KITTY_PUBLIC_KEY`: the
  precedent for authenticating an in-band sequence that acts on the user's behalf.
- Windows `RegisterApplicationRestart` and macOS `NSWindowRestoration`: at the OS level the application
  declares how to rebuild itself rather than the system guessing, and ghostty's window state is built on
  the latter. Windows also only restarts an application that ran for 60 seconds, to avoid restart loops.
- tmux-resurrect's docs on `:all:`: re-running everything restores a `sudo mkfs.vfat /dev/sdb` too, which
  is why every tool restores by allowlist or asks first.

### In ghostty

Read at ghostty `ea8b12a`, 2026-11-10. Nothing proposes a program declaring its restart command; these are
what a proposal there should link or answer.

Motivation:

- macOS restoration keeps no command. A surface encodes `pwd`, `uuid`, `title` and `isUserSetTitle`
  (`macos/Sources/Ghostty/Surface View/SurfaceView_AppKit.swift`), and decoding sets only the working
  directory. Window state is versioned (`TerminalRestorable.swift`, version 7, minimum 5), so a new field is
  a version bump.
- Linux has no restoration and is waiting on GTK's session API, expected in GTK 4.24
  ([#12055](https://github.com/ghostty-org/ghostty/discussions/12055)). The only attempt, with stable window
  ids, was closed unmerged ([#12962](https://github.com/ghostty-org/ghostty/pull/12962)). So this is
  macOS-first there.
- Demand: [#10584](https://github.com/ghostty-org/ghostty/issues/10584),
  [#4396](https://github.com/ghostty-org/ghostty/discussions/4396),
  [#3358](https://github.com/ghostty-org/ghostty/discussions/3358).

Prior discussion the token will be compared with:

- A per-surface id in the environment was asked for twice,
  [#9084](https://github.com/ghostty-org/ghostty/discussions/9084) (`TERM_SESSION_ID`) and
  [#10603](https://github.com/ghostty-org/ghostty/discussions/10603) (`GHOSTTY_SESSION_ID`), and a UUID
  exported per surface was closed unmerged ([#8200](https://github.com/ghostty-org/ghostty/pull/8200)). In
  #10603 a UUID was "deemed too complicated" and an opaque random u64 preferred. The token here is a
  different thing, a secret that gates who may write rather than a name for the surface, and should be
  presented that way; its format fits the u64 preference.
- The scripting API ([#2353](https://github.com/ghostty-org/ghostty/discussions/2353)) is the expected
  counterargument: declare through IPC instead. The answer is the one zellij's CLI demonstrates, that an IPC
  channel reaches one terminal and a sequence reaches whichever one the program is in, including through a
  multiplexer.
- Inference is already possible: the surface exposes its foreground process
  ([#11639](https://github.com/ghostty-org/ghostty/pull/11639)), and the OSC 133;C parser accepts `cmdline`,
  though ghostty's own integrations send a bare 133;C. Both give the argv, which is the problem statement.

Precedents the policy can copy:

- `clipboard-read = ask | allow | deny`, default `ask`. A resume policy of the same shape is the natural
  form there: `ask` for prefill, `allow` for run.
- Clipboard confirmations wait until the surface is focused and a new request replaces a pending one
  ([#13744](https://github.com/ghostty-org/ghostty/pull/13744)), which avoided focus stealing and prompt
  loops. A restore prompt for several windows wants the same.
- Program-supplied data that leads to execution is confirmed: untrusted OSC 8 links
  ([#13634](https://github.com/ghostty-org/ghostty/pull/13634),
  [#14683](https://github.com/ghostty-org/ghostty/pull/14683)), the title report made opt-in over command
  injection ([#3908](https://github.com/ghostty-org/ghostty/pull/3908),
  [#13632](https://github.com/ghostty-org/ghostty/pull/13632)), and OSC 52 injected through OSC 7
  ([#14859](https://github.com/ghostty-org/ghostty/pull/14859)). The last is the threat the token exists
  for, arriving through a sequence that was assumed harmless.
- The `input` option writes bytes to the pty before any user input, and its docs warn it "can be used to
  execute programs in a shell". It is what a prefill would most likely reuse.

OSC 7501 itself went in as [#14560](https://github.com/ghostty-org/ghostty/pull/14560) with no public
proposal issue first: the spec was circulated privately to application and terminal developers. Neither the
spec nor the [gist](https://gist.github.com/mitchellh/7acae3abd8355c1c00287d67e96c913a) says how 7501 was
chosen.

## Specification

The keywords MUST, SHOULD and MAY are used as in RFC 2119 and RFC 8174 only when in capitals. The
conventions follow the program status protocol, OSC 7501, so a terminal implementing one can share a
parser with the other.

### Wire format

```
OSC N ; pairs ST
pairs := pair (":" pair)*
pair  := key "=" value
key   := [a-z]+
value := [A-Za-z0-9_.,+/=-]*
```

`N` is unassigned. ST is `ESC \` or BEL.

### Keys

| Key     | Format                      | Meaning                                              |
|---------|-----------------------------|------------------------------------------------------|
| `op`    | `set` or `clear`            | Required.                                            |
| `token` | `[A-Za-z0-9_-]{16,64}`      | Required. See Authentication.                        |
| `argv`  | base64                      | Required with `set`. The arguments, each terminated by NUL, then base64. |
| `cwd`   | base64 of a UTF-8 path      | Optional. Absent means the window's directory at restore. |
| `app`   | `[A-Za-z0-9_.+-]{1,32}`     | Optional. Stable program identifier, as in OSC 7501. |
| `title` | base64 of UTF-8             | Optional. What resuming would bring back, for a confirmation prompt. |

`argv` is NUL-separated because an argument to `exec` cannot contain NUL, so the encoding round-trips any
argv without quoting rules, and a terminal never has to parse a command line.

There is deliberately no key for environment variables, and none for whether the terminal should run the
command or ask. The first would put secrets in saved window state. The second is the user's decision, and a
program that could choose it would always choose "run".

Unknown keys MUST be ignored. A repeated key takes its last value. A report that is malformed in any other
way MUST be discarded whole. The whole sequence MUST NOT exceed 4096 bytes.

### The record

A terminal keeps at most one record per window. `op=set` replaces it, `op=clear` removes it.

A record describes the program that is running now, so it MUST be removed:

- at the next prompt mark, OSC 133;A, since a prompt means the program has exited back to a shell;
- when the window's own child process exits;
- on `op=clear`.

A terminal with neither signal keeps the record until it is replaced or cleared. This is the lifetime OSC
7501 gives `working` and `blocked` records, and for the same reason: a record that outlives its program
resurrects something that was deliberately quit.

### Authentication

A terminal that supports the protocol MUST export a random token to the window's child in the environment
variable `TERM_RESUME_TOKEN`, and MUST discard a report whose `token` does not match. It MUST NOT reply to,
log or otherwise reveal a mismatch.

The reason is that a report executes code later. Without the token, any bytes that reach the window could
plant one: a file shown with `cat`, a `curl` response, a log line, a hostile or compromised host over ssh.
Those can write escape sequences but cannot read the environment of the program the terminal spawned.

- A token MUST be generated per window per start. A restored window gets a new one, so a token recorded in
  saved scrollback or a log is useless afterwards. The token MUST NOT be saved with the record.
- A program MUST NOT emit a report when `TERM_RESUME_TOKEN` is unset.
- A program running on another host over ssh does not have the token unless the user forwards it. That is
  the intended default: a remote host should not choose what runs locally.

### Restoring

When a window with a record is restored, the terminal runs `argv` in `cwd`, subject to the user's policy.

- The terminal MUST NOT run the command without the user having allowed it, by configuration or by
  confirming at restore. The default SHOULD be to present the command rather than run it.
- The terminal MUST pass `argv` to `exec` as given, or to a shell quoted so that it parses back to the same
  arguments. It MUST NOT join the arguments with spaces.
- The terminal SHOULD run the command so that exiting it leaves a shell, as kitty does for
  `cmd_at_shell_startup`. A user who quits a resumed program expects the window, not a closed one.

### Feature detection

`OSC N ; ? ST` is answered with `OSC N ; ? ST`. The presence of `TERM_RESUME_TOKEN` is the usual check; the
query exists for a program that wants to confirm the terminal is still the one that exported it.

### Multiplexers

A multiplexer is the terminal for this protocol from the point of view of the programs inside it. It exports
its own token to each pane and MUST NOT forward a report to the outer terminal.

It MAY declare itself to the outer terminal: cm declaring `cm attach kitty.55` lets the outer terminal
restore the window as that attach, and cm then resumes what ran inside it. The protocol composes through
nesting without either layer knowing what the other runs.

## How cm implements it

Three parts, each useful without the others. The declared part and the policy are built.

**Inferred: the typed command.** A session's recorded command today is its spawn argv, which for a
`kitty.*` session is the shell, so `on_restore = "command"` and `safe_commands` never see the program
running in it. The shell integration already reports what is running as command frames, kept in memory
only. Writing the innermost frame's argv to the session row when a frame opens or closes gives restore the
typed command, which also keeps a wrapper's argv out of it: tmux-resurrect restores rbenv's expanded
`ruby` path because `ps` is all it has.

**Declared: the sequence and `cm restore-with`.** Two channels, the same split as OSC 7501 and `cm report`:

- The sequence, for a program that writes it itself. cm exports the token, validates it, and does not
  forward the sequence to the client's terminal.
- `cm restore-with [session] -- ARGV...` and `cm restore-with --clear`, for hooks and wrappers. It
  authenticates by reaching the server's socket, and defaults to `CM_SESSION` as `cm report` does.

A hook must use the command, not the sequence. A hook writing to `/dev/tty` while its program renders is a
second writer on the program's stream, and lands inside the program's own sequences: the stale-cells bug
from one writer per stream in [architecture.md](architecture.md), in a new place.

A declaration binds to the command frame open when it arrives, the way an announced nesting does, so the
frame closing at the next prompt removes it. That is the protocol's OSC 133;A rule with a precise owner,
since a frame closes for the command that opened it rather than for whichever prompt comes next, and stays
open while the command is a stopped job: a claude suspended with ctrl-z keeps its declaration.

The declaration is written to the store when it arrives. It cannot be recovered from the persisted log the
way an OSC 7501 report is, because a declaration made at a program's startup has been trimmed out of the
16 MiB log long before the reboot, and replaying one would apply a record whose token is dead.

**Policy: `on_restore`.** Gains `prefill`:

- `shell` starts a shell and ignores the declaration.
- `none` shows the restored content and runs nothing.
- `prefill` types the command onto the new shell's prompt, unrun.
- `command` types the command and submits it, so exiting it leaves the prompt.

`command` runs a declared command only with the same permission it takes to re-run a recorded one: the
session asked with `--on-restore=command`, or `safe_commands` matches the program name. Without either it
prefills, which keeps the command one keypress away rather than dropping it. `prefill` never runs, whatever
the allowlist says, so it is the mode to choose for seeing what would be resumed before it is.

The command is typed once the new shell sends its first OSC 133 prompt mark, and after 10 seconds without
one it is typed anyway: the tty holds it until the line editor reads it, so a shell with no prompt marks
still gets it, and only an rc file that reads stdin could take it first. Waiting for the mark is not
cosmetic. Measured in the e2e test, a revive that missed the mark took 13.8s against 3.8s.

The declaration carried into the revived session binds to the next frame to open, which is the typed
command's. So quitting the resumed program ends it whether or not the program declares again, and a
prefilled line the user replaces with something else hands the declaration to that instead, which ends
with it.

Not yet built: the inferred part above, and fish's quoting is chosen from the server's `SHELL` when the
session runs the default shell, since the server does not otherwise know which shell the shim started.

## Examples

Claude Code, as a SessionStart hook. `/clear` and a fork start with a new `session_id`, so the declaration
follows them:

```sh
[ -n "$CM_SESSION" ] || exit 0
id=$(jq -r .session_id)
cm restore-with -- claude --resume "$id"
```

Not agents:

- **nvim**: a `SessionWritePost` autocmd declares `nvim -S <session file>`, so a restored window reopens
  the buffers and layout it had.
- **ssh to a remote cm session**: a wrapper declares `ssh -t host cm attach kitty.55`, so the window
  reattaches to the remote session instead of opening a fresh remote shell.
- **A database REPL**: a psql wrapper declares the connection it actually made.
- **Watchers**: `watch kubectl get pods`, `journalctl -fu svc`, `npm run dev` need no declaration; the
  typed command is the right one, and the inferred part restores it.
- **Never re-run**: a `terraform apply` wrapper runs `cm restore-with --clear`, so its frame restores a
  shell even under a broad allowlist.

## Open questions

- The sequence number. It should be coordinated with the OSC 7501 authors rather than claimed: neither the
  7501 spec nor terminal-wg has a registry (terminal-wg work item 10 asks for one). Not new keys on 7501,
  which is how that spec says it extends, because a 7501 report describes state and this one causes
  execution, and a terminal should be able to support one without the other.
- Whether a program should be able to read its own record back, for a hook that wants to know whether it
  has already declared.
- Restoring inside a shell needs the command handed to the shell integration. If that goes through an
  environment variable, its name must not collide with a flag: `noEnvFlags` in `cmd/cm` exists because
  two did.
