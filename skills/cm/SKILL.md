---
name: cm
description: "Run and drive work in cm sessions: persistent terminal sessions that outlive the caller. Use when the user mentions cm, or asks to run something in the background, delegate work to another agent, or orchestrate several agents in parallel. Each session is a real pty, so interactive programs and coding agents work in one."
---

# cm

cm keeps terminal sessions alive independently of whoever started them. A session is a command or shell on a real pty, so a program that checks for a terminal behaves as it would interactively, and it keeps running after the process that created it exits.

That is what makes cm useful for more than backgrounding a build. You can start another coding agent in a session, send it instructions, read what it produced, and wait for it to finish, all from inside a single conversation. One agent can run several others in parallel and collect their results.

## Check cm is available

```bash
command -v cm && cm version
```

The installed binary is the authority on its own syntax. `cm --help` lists the commands, and `cm <command> --help` explains one, including caveats not repeated here. Prefer reading that over guessing a flag.

`cm` needs no server started by hand: any command starts one if none is running.

## Sessions and names

A session has a name you choose or one cm allocates. Reusing a name reuses the session, which is what makes cm idempotent and safe to re-run.

```bash
cm list --json                 # every session and its state
cm info <name> --json          # one session
cm info <name> --field cwd     # one value, no header, for scripts
```

Read state from `--json` rather than parsing the table. The fields that matter for driving work:

- `state`: `running`, `exited`, or `dead`. `dead` means cm lost track of the process, which is different from a command that failed.
- `busy` and `command`: whether a command is running and which. Derived from OSC 133, so they only work when the session's shell emits those markers.
- `last_command_exit_code` and `command_finished`: the last command's status, distinct from `exit_code`, which is the *session's*. A failing build in a live session has `exit_code` 0 and `last_command_exit_code` non-zero.
- `reported_state`, `reported_detail`, `reported_source`: what a program inside said about itself. Empty unless something reported.

If `cm info <name>` says the session is not found, it has ended and been forgotten. That is not an error to retry.

## Run one command and get its output

For a single command, this is the whole interface:

```bash
cm run -- make -j4                       # waits, prints output, exits with its status
cm run --session build -- make -j4       # names the session, so state persists between runs
cm run -d -- ./long-thing                # returns immediately, prints the session name
```

`cm run` exits with the command's status, so it composes with `&&` and `||` like a local command. Output has escape sequences stripped, so it is text rather than colour codes; `--raw` keeps them.

Reusing a `--session` name changes how arguments are read, and the difference matters:

```bash
cm run --session build -- make -j4     # creates: an argv, no shell involved
cm run --session build -- 'make -j4'   # reuses: sent to the shell, so quoting is yours
```

Two traps here, both verified rather than theoretical:

- The *creating* call passes an argv straight to `exec`, so `cm run --session build -- 'echo x'` on a session that does not exist yet tries to run a program literally named `echo x` and fails with "shim for build did not become ready". Quote the command only when reusing; give an argv when creating, or create with a shell: `-- /bin/sh -c 'echo x'`.
- Reuse sends to the shell already in that session, which only works while it is still alive. A session created by a command that has since exited is gone, so the "reuse" call creates a new session and you get fresh output; if the old record is still listed you can read the *previous* command's output and mistake it for yours. Check `cm info <name> --field state` if in doubt.

To keep a session around for repeated reuse, create it with a shell rather than a command:

```bash
cm attach --no-attach build --dir /path/to/repo    # a persistent shell
cm run --session build --timeout 10m -- 'make -j4'
```

That shell also needs to emit OSC 133 for `cm run` to know the command finished. `/bin/sh` does not, so pass `--timeout`: without one the reuse path waits indefinitely, having already produced the output. See the OSC 133 note above.

## Drive a long-running or interactive program

Start it detached, then interact:

```bash
cm attach --no-attach worker            # a shell session, nothing attached
cm attach --no-attach worker -- <cmd>   # or run a specific program in it
cm send worker 'echo hello' --enter     # --enter appends the carriage return that runs it
cm read worker --lines 50               # rendered recent output
cm history worker                       # everything, scrollback included
```

`cm send` writes to the pty exactly as typing would, so the session's own echo and its prompt appear in the output. That is the program's output, not something cm adds.

To send and collect in one step:

```bash
cm send worker 'make' --enter --follow           # stream until the command finishes
cm send worker 'make' --enter --wait idle        # block until done, then read
```

`--follow` and `--wait idle` both need to know when the command ended, which cm learns either from OSC 133 or from the program reporting its own state. **A program that is not a shell emits no OSC 133**, so unless it reports, waiting for `idle` waits forever; cm prints a warning saying so. For those, either use `--timeout`, or make the program report (below). This is the single most common way an orchestration hangs.

Always pass `--timeout` when driving something whose reporting you have not confirmed. It converts a hang into an answer.

## Wait instead of sleeping

```bash
cm wait <name> --until idle --timeout 60s
cm wait <name> --until blocked --timeout 5m
cm wait <name> --until exited
```

`cm wait` exits 0 when the state is reached and 1 on timeout, so it chains:

```bash
cm wait build --until idle --timeout 5m && cm read build
```

The server watches the session's own output, so this cannot miss a transition the way polling `cm list` can. Never substitute `sleep` for it.

Use it to wait on something you did not start, or to check a state now. To wait for work *you* just sent, use `cm send --wait` instead: a standalone wait cannot tell your command's state from the state the session was already in. See the turn-taking section.

Accepted states are `idle`, `busy`, `blocked`, and `exited`. `idle` and `busy` come from OSC 133, or from an explicit report, which takes precedence. `blocked` only ever comes from a report.

## Reporting state, and why blocked matters

cm can tell that a command is running. It cannot tell whether that command is computing or sitting at a prompt of its own waiting for an answer, because both look identical from outside. `blocked` is how a program says which.

For a coding agent, that is exactly the interesting state: "I need your input" versus "I am still working". Wire it to whatever hook the agent has:

```bash
cm report <name> --state blocked --detail "needs approval"
cm report --state busy            # inside a session, name comes from CM_SESSION
cm report --state clear           # withdraw, falling back to what cm derives
```

From a shell, the cheaper form avoids paying ~23ms per call:

```bash
eval "$(cm shell-init zsh)"          # or bash; fish: cm shell-init fish | source
cm_report blocked "waiting for approval"
```

Both end up in the same place. A report takes precedence over what cm derives, because a program describing itself is better evidence than a shell marker.

A program with no OSC 133 and no reporting is invisible to `--wait` and `cm wait`. Making it report is what turns it into something you can orchestrate.

### Make the agent you start report for itself

You do not have to guess when a delegated agent is done. Tell it to say so, in the prompt you send:

```bash
cm send reviewer 'Review the current diff. When you are finished, run: cm report --state blocked --detail "review complete"' --enter
cm wait reviewer --until blocked --timeout 10m
```

Keep the instruction on one line. `cm send` writes exactly what you give it, so an embedded newline submits the prompt early and the rest arrives as a second, separate input.

This is the most reliable way to drive an agent that cm cannot otherwise read, and it is worth preferring over content-matching on its output. The agent knows when it has finished; cm cannot infer it from a screen that may still be redrawing. `CM_SESSION` is already set inside the session, so the agent needs no name and no plumbing.

If the agent has a hook for "finished" or "needs input" (a stop hook, a notification hook), wiring `cm report` to it once is better still, because then every turn reports without being asked. Pass `--source` when you do, so one reporter is distinguishable from another:

```bash
cm report --state blocked --detail 'awaiting input' --source claude-stop-hook
```

To check whether anything is reporting in a session, read `reported_state`, not `reported_source`: the source is optional and empty unless a reporter set it, so a session that reports faithfully can still have no source.

```bash
cm info <name> --field reported_state       # non-empty means something reported
```

Ask for `blocked` rather than `idle` when the agent is waiting for your next instruction: that is what `blocked` means, and it keeps "waiting for me" distinct from "shell at a prompt".

## Turn-taking with an agent in a session

Use `cm send --wait`, in one call, rather than sending and then waiting separately:

```bash
cm attach --no-attach reviewer -- <agent-command>
cm wait reviewer --until blocked --timeout 60s              # it is up and wants input
cm send reviewer 'Review the current diff.' --enter --wait idle --timeout 10m
cm read reviewer --since-commands 1
```

The single call is what makes this correct, and the reason is worth knowing because the alternative fails in a way that looks like success. `cm send --wait` arms the wait *before* writing the input and requires evidence that something happened afterwards, so it cannot be satisfied by the state the session was already in.

A separate `cm wait --until blocked` after a `cm send` has no such qualifier. The agent is already `blocked` when you send to it, so the wait returns immediately, and you read the *previous* turn's output believing it is the new one. Nothing errors.

Waiting for `busy` and then `blocked` looks like it fixes that and does not: a fast turn's `busy` and `blocked` coalesce into one event, so nothing ever observes `busy` and the wait times out on work that has already finished.

If you must wait separately, wait for `idle` with `send --wait`, and treat a standalone `cm wait --until <state>` as answering "is it in this state now" rather than "has my work finished".

For an agent that does not report, fall back to `--timeout` and a content check:

```bash
cm send reviewer 'Review the diff.' --enter
cm read reviewer --since-commands 1 | grep -q 'DONE' || sleep 5
```

## Several agents at once

Sessions are independent, so fan out by starting each and then collecting:

```bash
for area in api ui docs; do
  cm attach --no-attach "review-$area" -- <agent-command>
done
for area in api ui docs; do
  cm wait "review-$area" --until blocked --timeout 60s     # each is up and ready
done

# Each send blocks until its own agent finishes, so run them concurrently.
for area in api ui docs; do
  cm send "review-$area" "Review the $area changes." --enter --wait idle --timeout 10m &
done
wait

for area in api ui docs; do
  echo "=== $area ==="; cm read "review-$area" --since-commands 1
done
```

The `&` and `wait` are the point. `cm send --wait` blocks until that agent's turn is done, so running the sends in sequence gives you no parallelism at all: three agents each taking a minute take three minutes rather than one. Backgrounding them and waiting once is what makes this a fan-out. Verified: three agents doing a second of work each complete in one second in total.

Give each session a name describing its job. Names are how you address them, they appear in `cm list`, and a session outlives the conversation that made it.

### Tag a fan-out so you can address it as a group

The loops above repeat the name list three times, which only works because you generated the names and still have them. Tag the sessions instead and address the group directly, which removes two of the three loops:

```bash
run="review-$$"
for area in api ui docs; do
  cm attach --no-attach "review-$area" --tag "run=$run" --tag "area=$area" -- <agent-command>
done

cm wait --tag "run=$run" --until blocked --timeout 60s   # all of them, concurrently
cm read --tag "run=$run" --since-commands 1              # each under its own header
cm kill --tag "run=$run"                                 # tear the group down
```

`--tag` selects on `list`, `kill`, `wait`, `read`, `history`, and `info`. Three things about it are worth knowing before relying on it:

- **`cm wait --tag` waits concurrently and requires all of them.** It exits 0 only if every session reached the state, which is what makes `cm wait --tag ... && cm read --tag ...` correct. Add `--any` to return as soon as the first one does, for reacting to whichever finishes first. Verified: five sessions each sleeping three seconds complete in 3.02s, not 15s.
- **A selector matching nothing is an error.** So a typo in `--tag run=abc` fails loudly instead of looking like a group with no output, and `cm kill --tag` cannot report success having killed nothing.
- **`cm kill --tag` is the safe form of `cm kill --all`.** It only reaches what the selector matched, so it cannot touch the user's own sessions. Prefer it for teardown.

Two combinations are refused, because their output would be broken rather than just ugly: `cm read --follow` and `cm history --format=html` each need one session. Name it for those.

You still need the `&`/`wait` pattern for the *sends*, since `cm send --wait` is per session and there is no `send --tag` (broadcasting keystrokes to several shells is rarely what you want).

Worth tagging when the set is not a literal list you wrote: sessions created in more than one place, or names the server allocated, which `--prefix` cannot match at all. Repeating `--tag` narrows, so `--tag "run=$run" --tag area=ui` picks one out of the group.

Tags are metadata, and cm does not interpret them: no key changes how a session behaves. Keys and values allow letters, digits, `-`, `_`, `.`, and `/`, up to 63 bytes, so use `run=abc123` rather than anything with spaces or punctuation in it. Set them at creation as above, or afterwards with `cm tag <name> key=value`, which also works on a session that has already exited.

## Environment and working directory

The server spawns sessions, so a variable exported in your own process does not reach one. Pass it explicitly:

```bash
cm attach --no-attach worker --env 'KEY=value' --dir /path/to/repo
cm run --env 'CI=1' --dir /path/to/repo -- ./script
```

`--env` applies only when the call creates the session; it is ignored when reusing one. `--dir` defaults to the caller's cwd.

cm exports `CM_SESSION` into every session, so a program inside knows which session it is in without being told, which is what lets `cm report` take no argument there.

## Reading output: which command

- `cm read <name> --since-commands N` returns everything since the last N commands started, each block opening with the prompt and the command line. **Prefer this** when the question is "what happened": a line count is a guess, and this is the actual boundary.
- `cm read <name> --last-output` returns only what the last command printed, with no prompt or echoed command line. Use it when a script is parsing the result.
- `cm read <name>` renders recent output as text, rejoining lines the terminal soft-wrapped. Default 100 lines; `--lines 0` for everything. Use this when the command boundaries are not available.
- `cm read --follow` prints the tail then streams, like `tail -f`.
- `cm history <name>` renders everything including scrollback, with `--format vt` for colours or `--format html` for markup.
- `--raw` on either gives the bytes the program emitted rather than the text they rendered to.

The command-boundary forms need the session's shell to report OSC 133, which is what brackets a command. They say so rather than returning empty output if it does not, and `cm doctor` diagnoses it. They also cannot answer for a session that has already ended, since the boundaries live with the running session: for `cm run`, whose session is gone by the time you read it, use `--lines`.

Neither combines with `--lines`, which is a different bound on the same read.

Prefer rendered text. Escape sequences in captured output are noise at best, and writing them into a file corrupts it.

A caveat with real consequences: a full-screen program (an editor, a TUI) draws on the terminal's alternate screen, and lines that scroll off there are not recoverable by asking for more of them. If raising `--lines` reveals nothing more of a finished response, ask the program to write its output to a file and read the file instead.

## Cleaning up

```bash
cm kill <name>          # end a session and its shell
cm kill --all           # every session cm knows
```

Kill the sessions you created when the work is done. A session left running holds a pty and a process.

Do not `cm kill --all` or `cm server stop` unless you are certain nothing else is using cm: the user's own interactive sessions live in the same server, and stopping it or killing everything takes their work with it. Kill by name.

If you tagged a fan-out, that tag is the safe version of `--all`: it reaches exactly the sessions you created and nothing of the user's.

```bash
cm kill --tag "run=$run"
```

## When something looks wrong

```bash
cm doctor          # checks the installation and reports problems
cm status          # what the running server is doing
cm logs server -n 50
cm logs shim <name>   # one session's log, which is where its own failures land
```

`cm doctor` encodes past incidents as checks, so run it before investigating by hand. It exits non-zero when it finds something, so a failure there is a report rather than a malfunction: read what it printed.

Two failures worth recognizing, since neither looks like its cause:

- A `--follow` or `--wait idle` that never returns almost always means the session's program emits no OSC 133. cm warns about this; believe the warning.
- "session has ended and was not persisting, so its output is gone" means the program exited and its output was not saved. Use `cm run`, which captures output for a few minutes, or `--persist` for longer.
