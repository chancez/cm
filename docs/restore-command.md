# Program resume protocol (draft)

**Status: the sequence is a draft and unimplemented; cm implements the rest of the model without it.** A
proposal for a sequence a program writes to say how it should be started again when its window is restored,
and for the rules a terminal follows in restoring it, plus how cm implements the same thing through
`cm restore-with`, its shell integration's command frames, and `on_restore`. Written to be proposed to
terminals beyond cm, ghostty first, so the spec half avoids anything cm-specific. The rules below the wire
format are what implementing it in cm taught; each says what went wrong without it.

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

A record describes the program that is running now, so it ends with that program. A terminal that tracks
command frames, the span from OSC 133;C to OSC 133;D its shell integration reports for each command, or an
equivalent, MUST bind a record to the innermost frame open when the report arrives, and MUST remove it when
that frame closes. That is the precise form of the rule: a frame closes for the command that opened it,
rather than for whichever prompt comes next, and stays open while the command is a stopped job, so a
program suspended with ctrl-z keeps its record. A report that arrives with no frame open is bound to none.

A terminal without frames MUST remove the record:

- at the next prompt mark, OSC 133;A, since a prompt means the program has exited back to a shell;
- when the window's own child process exits;
- on `op=clear`.

A terminal with neither signal keeps the record until it is replaced or cleared. This is the lifetime OSC
7501 gives `working` and `blocked` records, and for the same reason: a record that outlives its program
resurrects something that was deliberately quit.

`op=clear` in a frame leaves a cleared record bound to that frame rather than none: nothing is restored for
that frame, not even an inferred command (see Inferring a command), and it ends with the frame like any
record, so a command run after the program returns is restorable again. A `terraform apply` wrapper clears
so that its window comes back as a shell even under a broad allowlist; without the marker, the inferred
`terraform apply` would fill the gap the clear left.

### Ending a window to restore it later

A terminal or multiplexer that ends a window's programs on purpose so that it can restore them later,
rather than because they exited or the user quit them, MUST freeze the window's record, cleared record and
inferred command as they stood when it began, ignoring every change until the programs are gone. Ending them
is what closes their frames: zsh answers SIGHUP by printing `zsh: hangup` and drawing a prompt before it
dies, and that withdrew a program's record 36ms after the hangup was sent, so the window came back as a bare
shell. An exit hook may also send a clear or a new report as its program dies. If the programs survive the
attempt and keep running, the terminal MUST lift the freeze.

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

### Inferring a command

A terminal MAY infer a command for a window that declared none, from the command line its shell integration
reports for a frame, such as the `cmdline` of OSC 133;C. If it does:

- The inferred command is the line of the outermost open frame, and only in a window whose own program, the
  one the terminal spawned, is a shell. A frame above the outermost is written by a shell further in,
  through the same pty: the far side of an ssh, a nested multiplexer, a subshell. Its line belongs to that
  shell, and run here it would start the remote or nested program locally. The outermost frame is the ssh
  or the nested attach itself, which run again reconnects. A window whose own program is not a shell, such
  as `kitten ssh host`, has no frames of its own, so every frame it shows comes from a deeper shell, and
  nothing is inferred from them.
- It is bound to that frame and removed when the frame closes, like a record.
- It is a line as the shell reported it, not an argv. Restoring types it back verbatim into the same kind of
  shell, which parses it as it did the first time, so nothing is re-quoted.
- It MUST NOT be run unless it is a plain invocation, whatever the user's policy allows for a declared
  command, since it goes to a shell that runs all of it: with `sleep` allowed, `sleep 1; rm -rf x` would run
  in full. Anything else MAY be presented, and is never run. A line at the length the shell or the
  terminal truncates it to is not plain, since its end may be missing.

A plain invocation is a program word followed by argument words, separated by spaces or tabs, each
optionally quoted with `'...'` or `"..."`. A line is not plain if it has:

- an operator or grouping, `;` `|` `&` `<` `>` `(` `)` `{` `}`, which covers `&&`, `||`, `>>` and
  here-documents;
- an expansion, `$` or a backtick, outside quotes or inside double quotes;
- a glob, `*` `?` `[` outside quotes, or `#` or `^`, which zsh's extended globbing reads as patterns;
- history: `!` outside single quotes, or a line starting with `^`, which is quick substitution;
- a backslash outside single quotes, which covers a continuation, or a newline anywhere;
- a `~` that is not a word on its own or the start of `~/`, so `~user` and zsh's `a~b` exclusion;
- a word starting with `=`, which zsh expands to a command's path;
- a first word that assigns, `VAR=value`;
- an unbalanced quote, or nothing at all.

The program word, with its quotes removed, is what an allowlist matches. What the rule cannot see is what a
word means to the shell: an alias or a function named like an allowed program runs as itself, so an
allowlist is a convenience rather than a safety boundary.

### Restoring

When a window is restored, what comes back is, in order:

1. the record, when the window has one; a cleared record for a frame still open stops here, with nothing;
2. else the inferred command, when the terminal infers one;
3. else the window's own spawn argv, when it is a program rather than just a shell;
4. else nothing, and the window gets a shell.

The rules for running it:

- The terminal MUST NOT run the command without the user having allowed it, by configuration or by
  confirming at restore. The default SHOULD be to present the command rather than run it.
- A per-window permission to run without asking, given when the window was opened, covers what that window
  was started with and MUST NOT cover what the user runs in it later. That is the window's spawn argv, or
  the command its opener had typed into its shell, and a record that command makes, until that command's
  frame closes; what follows gets the terminal's configured policy. A restore that runs a covered command
  keeps the permission for it. Unscoped, a window opened once to run one program without asking ran
  whatever was later typed into it, on every restore.
- Restoring MUST ignore the command and the implicit working directory of whatever reopened the window. A
  terminal replaying a saved session reopens each window with the command it was first opened with, and
  letting that command win reran the window's original program after every reboot and buried the record.
  The window comes back in its recorded directory; a directory the opener named explicitly is a request and
  is honored, while one filled in from the opener's own is not.
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

Three parts, each useful without the others.

Built: a declaration through `cm restore-with`, bound to its frame, with the cleared record; the freeze
while `cm kill --keep` or `cm shelve` ends a session; the inferred command, outermost frame only and only in
a session whose own program is a shell, with the plain-invocation rule; the restoring order, the scoped
permission, and ignoring the opener's command and implicit directory, through `on_restore`, `cm attach`
and `cm restore`.

Not built: the sequence itself, so no token is exported, no report is parsed, and there is no feature query;
and cm declaring itself to the outer terminal, as the Multiplexers section allows.

**Inferred: the typed command.** A session's spawn argv is, for a `kitty.*` session, the shell, so
`on_restore = "command"` and `safe_commands` never saw the program running in it. The shell integration
reports what is running as command frames, and the line of the session's outermost open frame is now kept
with the record as `typed_command`: set when that frame opens, dropped when it closes at a prompt, so a
program the user quit is not brought back, and frozen while `cm kill --keep` or `cm shelve` ends the
session, as the declaration is. It also keeps a wrapper's argv out of it: tmux-resurrect restores rbenv's
expanded `ruby` path because `ps` is all it has. `cm info --json` shows it as `typed_command`, beside the live
`command`.

The outermost frame rather than the innermost, which is where a declaration binds. A frame above the
outermost is written by a shell further in, through this pty: the far side of an ssh, a nested `cm attach`,
or a subshell. Its line typed back into this session's new shell after a reboot would run the remote or
nested program here, while the outermost frame is the ssh or the attach itself, which typed back
reconnects. A declaration has no such problem, since only a program on this machine can reach this server
to declare one. For the same reason it is kept only for a session whose own program is a shell: one spawned
as `kitten ssh host` has no shell here, so even its outermost frame is the remote shell's, and its spawn argv
is what brings it back.

What a shell reports is a line, not an argv: zsh's preexec is passed the line as typed, bash reads it back
from `history 1` (so a shell with history off reports nothing), and fish passes the command line, each
escaped for the wire and cut at 256 characters. cm does not turn it into an argv. A revive types it back
verbatim into the session's new shell, which parses it as the same shell parsed it the first time, so
quoting, pipes, variables and aliases come back exactly as typed, in every shell the integration supports;
nothing is re-quoted, which is why fish's quoting differences do not apply to it. It runs only when it is
a plain program invocation, a program word and argument words, with the program word matched against
`safe_commands`; a shell expression, or a line at the 256-character cut, is only ever prefilled. The rule
is in [persistence.md](persistence.md#the-allowlist-is-a-convenience-not-a-safety-boundary).

**Declared: the sequence and `cm restore-with`.** Two channels, the same split as OSC 7501 and `cm report`:

- The sequence, for a program that writes it itself. Not built: cm would export the token, validate it, and
  not forward the sequence to the client's terminal.
- `cm restore-with [session] -- ARGV...` and `cm restore-with --clear`, for hooks and wrappers. It
  authenticates by reaching the server's socket, and defaults to `CM_SESSION` as `cm report` does.

A hook must use the command, not the sequence. A hook writing to `/dev/tty` while its program renders is a
second writer on the program's stream, and lands inside the program's own sequences: the stale-cells bug
from one writer per stream in [architecture.md](architecture.md), in a new place.

A declaration binds to the command frame open when it arrives, the way an announced nesting does, so the
frame closing at the next prompt removes it. That is the protocol's OSC 133;A rule with a precise owner,
since a frame closes for the command that opened it rather than for whichever prompt comes next, and stays
open while the command is a stopped job: a claude suspended with ctrl-z keeps its declaration.

`cm kill --keep` freezes the declaration as it stood when the kill arrived. The hangup is what closes the
program's frame then, not the program quitting: zsh prints `zsh: hangup` and draws a prompt before it dies,
and the declaration went with that frame 36ms after a `cm kill NAME --keep --expire-after never`, so a
suspended session revived into a bare shell. A clear or a new declaration from the dying program is
ignored too, since an exit hook can send either. A plain `cm kill` forgets the session anyway, and a kill
that fails with the shell still running unfreezes it. `cm shelve` freezes it the same way.

Every revive of an ended session that saved output uses the declaration, however the session ended:
exited, dead after a reboot or a crash, or shelved. An attach's command after `--` is ignored, with a
notice on stderr, since the session already says how it comes back, and a terminal restoring a window it
had opened with `cm attach NAME -- ARGV` replays that argv. When the attach's command used to win on the
reboot path, that replay reran the window's original program and the declaration was never used. `cm run
--session` is the one caller whose command still wins on an exited or dead session, and it refuses a
shelved one rather than drop its command.

The declaration is written to the store when it arrives. It cannot be recovered from the persisted log the
way an OSC 7501 report is, because a declaration made at a program's startup has been trimmed out of the
16 MiB log long before the reboot, and replaying one would apply a record whose token is dead.

**Policy: `on_restore`.** Gains `prefill`:

- `shell` starts a shell and brings nothing back.
- `none` shows the restored content and runs nothing.
- `prefill` types the command onto the new shell's prompt, unrun.
- `command` types the command and submits it, so exiting it leaves the prompt.

What "the command" is, in order:

1. the declared resume command, from the sequence or `cm restore-with`; a `restore-with --clear` in a frame
   still open stops here with nothing, so a cleared frame's typed command is not used either;
2. else the typed command;
3. else the spawn argv, when it is a program rather than just a shell like `zsh -l`;
4. else nothing, and the revive is a shell.

`command` runs it only with the permission it always took to re-run a recorded command: the session asked
with `--on-restore=command`, or `safe_commands` matches the program name. A session's own
`--on-restore=command`, kept with its record, is the per-window permission, and covers what its attach
started: on a plain attach the spawn argv, with `--in-shell` the command that attach typed and a declaration
it makes, until its frame closes; the record keeps that frame. A revive's own `--on-restore=command` covers
everything that revive brings back. Without permission it prefills, which
keeps the command one keypress away rather than dropping it. `prefill` never runs, whatever the allowlist
says, so it is the mode to choose for seeing what would be resumed before it is. A declared or typed
command is typed into the new shell; a spawn argv that runs is exec'd in place of it, as a recorded command
always was, and one that is prefilled is typed.

The new shell is handed the command in its environment, `CM_RESTORE_COMMAND` with the line and
`CM_RESTORE_RUN=1` to run it or `0` to offer it, which is kitty's `KITTY_SI_RUN_COMMAND_AT_STARTUP`. cm's
shell integration reads and unsets both while the rc file sources it, so no program the shell starts sees
them, and reports `restore=taken` on OSC 25453. At the first prompt it adds the line to the history and runs
it between the shell's own preexec and precmd hooks, everyone's and not only cm's: zsh calls them, bash
expands PS0, fish emits `fish_preexec` and `fish_postexec`. So cm's frame opens for it, kitty's or starship's
OSC 133;C tells cm it is running, and a title hook names the window, as for a command the user entered.
Offered, it goes in the line editor: zsh `print -z`, fish `commandline -r`. Nothing is typed, so the prompt
is not followed by the line being echoed. bash has no way
to fill the line editor from a prompt hook, so it takes only a command to run, and an offered one is typed
in. fish 3 is the same: its reader discards a `commandline -r` from a `fish_prompt` handler, and it has no
`history append`, so the line is run without being added to the history. bash before 4.4, the 3.2 that
macOS ships as `/bin/bash`, takes neither and has both typed in, but still unsets the variables. The variables go to the shell through the shim and are not recorded as the session's client
environment, so `cm get-env` does not hand them out, and they are dropped from what a shim inherits from the
server, so a server started from a shell that kept them does not pass its command to every new session.

The report arrives while the rc file runs, before the shell's first OSC 133 prompt mark, and the server reads
it ahead of a prompt mark in the same chunk of output. A prompt mark with no report is a shell without the
integration, which has the command typed in at that mark. After 10 seconds without a mark it is typed anyway:
the tty holds it until the line editor reads it, so a shell with no prompt marks still gets it, and only an
rc file that reads stdin could take it first. Waiting for the mark is not cosmetic. Measured in the e2e
test, a revive that missed the mark took 13.8s against 3.8s. A shell without the integration keeps the
variables, and anything it starts inherits them.

Not typed at all, though, once a command frame has opened in the new shell: something is then in the
foreground, and the line would be its input. A revived window whose shell sent no prompt mark had its user
start nvim within the 10 seconds, and the restore command, typed into nvim, began with `/` and opened a
search. The declaration carried for the command is withdrawn with it, since it bound to that program's
frame instead. A shell without cm's integration reports no frames, so it is still typed into.

The declaration carried into the revived session binds to the next frame to open, which is the typed
command's. So quitting the resumed program ends it whether or not the program declares again, and a
prefilled line the user replaces with something else hands the declaration to that instead, which ends
with it.

Quoting a declared command or a prefilled spawn argv for fish is chosen from the spawn argv, or from the
server's `SHELL` when the session runs the default shell, since the server does not otherwise know which
shell the shim started. A typed command needs no quoting.

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
  typed command is the right one, and the inferred part restores it, run if the program is in
  `safe_commands`.
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
