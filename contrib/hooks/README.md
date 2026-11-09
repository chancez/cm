# Reporting state from a program

`cm report` records what a program in a session is doing, so other programs can see it and wait for it:

    cm report --state working --detail "running tests"
    cm report --state blocked --kind permission --detail "needs approval"
    cm report --state done    --detail "tests passed"
    cm report --state error   --detail "tests failed"
    cm report --state idle
    cm report --state clear

These are the states of the program status protocol, OSC 7501
(https://www.superlogical.com/rex/docs/build/program-status), which cm also reads from a session's output.
A program that emits it needs no hook at all. `busy` is accepted for `working`.

With no session name it uses `CM_SESSION`, which cm exports into every session's shell, so anything running
inside a session can report without being told where it is.

Nothing here is specific to any program. cm never learns what is running in a session, and does not try:
it has no list of known agents and no patterns to match against their output. A build script and a coding
agent use the same states, and a program cm has never heard of works exactly as well as one it has.

That is a deliberate difference from the alternative. Detecting agent state by matching the screen means
keeping a description of every agent's UI and updating it whenever one changes, which is a treadmill.
Asking the program is a fixed cost.

## Why `blocked` cannot be derived

cm reads OSC 133 from a session's output, which is how `cm list` knows whether a command is running. That
is enough for a shell, and not enough for anything interactive: the shell reports a command as running
whether it is computing or sitting at a prompt of its own. A coding agent is one long-running command from
the shell's point of view, from the moment it starts until it exits.

So busy and idle are derived when nobody reports them, and `blocked` only exists when something says
so. A report also takes precedence over the derived state, since a program describing itself is better
evidence than a marker its shell emitted.

## Wiring it to a program

Anything that can run a command when its state changes can report. Two patterns:

**A program with hooks.** Point them at `cm report`. The scripts here are examples, not the mechanism.

**A program without hooks.** Wrap it:

    cm report --state working --detail "$*"
    "$@"; status=$?
    if [ $status -eq 0 ]; then cm report --state done; else cm report --state error; fi
    exit $status

## Lifetime

A report lasts until it is changed or cleared, with one exception the protocol requires: `working` and
`blocked` end when the session's shell next prompts or the program exits, since whatever was working has
stopped by then. `done` and `error` stay until replaced.

A report survives a server restart. One restored with the shell back at its prompt and nothing running is
dropped, since no program in that session can still be blocked.
