# CLI

Notes on using cm from a script or a shell startup file. For the config file, see
[config.md](config.md).

## Naming a session, and referring to one

Every command that takes a session takes a *reference*, which is either a name or an ID with an `@` in
front of it. `@` cannot appear in a name, so the two can never be confused, whatever a session is called.

```
cm attach work            # by name
cm attach @a7k2m9x4       # by identity
```

Which to use in a script is a real choice rather than a style question. A name is a binding and can be
pointed at a different session at any time, by `cm bind` or by `cm switch`, so anything that records a
session and comes back to it later should record the ID: `cm list --json` reports it as `id`, and a session
answers to it for as long as it exists. `CM_SESSION` inside a session is the ID for exactly this reason, so
`cm read $CM_SESSION` cannot end up reading somewhere else.

A session may also have no name at all, which is what `cm attach` with no argument and `cm run -d` produce.
`cm list` shows such a session by its ID reference in the NAME column, so whatever is printed can be pasted
straight back into another command.

`cm switch` moves this window's client to another session and leaves every name alone, so a restarted
terminal returns to the session it always named. `cm rebind` moves the window's name as well, which is what
makes it stick. `cm rebind --replace` also ends the session the name came off, and `rebind_replaces` makes that the
default. `cm bind` and `cm unbind` manage names: between them they cover renaming a session, giving it a second
name, and moving a name to another session. `cm bind --borrow` marks a name whose kill releases the name
instead of killing the session, which is what a per-window name wants once its window is borrowing a
session that lives elsewhere. `cm kill --json` reports those as `unbound` rather than `killed`, and a
teardown script should treat them apart: the session named there is still running.

## JSON output

`list`, `info`, `kill`, `get-env`, `tag`, `bind`, `unbind`, `switch`, `wait`, `send`, `run`, `status`,
`doctor`, `config`, `version`, `detach`, `upgrade`, `rebind`, `clients`, `clients list`, and
`clients current` accept
`--json`.

The shape is a contract, defined in `cmd/cm/output.go` rather than by marshalling the wire messages.
Fields are only ever added, never renamed or removed, and a test asserts the exact key set.

```
$ cm list --json | jq -r '.[] | select(.state=="running") | "\(.name) -> \(.cwd)"'
work -> /home/user/projects
```

Details worth knowing when scripting against it:

- An empty list is `[]`, never `null`, so a script can iterate unconditionally. Same for `killed`
  and `errors` in `kill --json`.
- `state` is `running`, `exited`, or `dead`. Prefer it over `exit_code`, which means nothing for a
  dead session, since that outcome is unknown rather than observed.
- `cwd` is empty when the session reported a directory on another host, because acting on a remote
  path locally would open the wrong place or fail. `cwd_uri` still carries the reported value, so a
  caller can distinguish "remote" from "nothing reported".
- `cwd` is absolute, and stays absolute. The `cm list` table abbreviates a path under home to
  `~/...`, but that is a rendering for a person: `~` only expands in a shell, so a Go or Python
  caller handed one would create a directory named `~`. `cm info --field cwd` is absolute for the
  same reason.
- `kill --json` reports partial failure in the payload *and* exits non-zero, so a script can check
  the status without parsing.
- Ordering is stable: oldest first, ties broken by name.
- `active` marks the client someone is using and is set on at most one client per session, so a script
  can take the first match. It is unset on every client until something is typed, which is the state a
  freshly attached session is in, so a caller has to handle "nobody". `cm clients current` exits
  non-zero there rather than printing an empty record.
- `last_input_at` is `null` for a client that has never typed, rather than rendering the epoch. Read it
  alongside `active`, since the mark alone does not say how old it is: a client that last typed days ago
  is active only in the sense that nothing else has typed since.

## Timestamps in JSON

Every timestamp is one field, RFC 3339, in the machine's local zone, and `null` when there is no instant
to report. `created_at` is the exception that is never null, because a session always has one.

They were pairs until recently, a string beside a `_unix` integer, and dropping the integer has one cost
worth knowing because the obvious workaround is silently wrong. jq's date builtins accept only the `Z`
form:

```
$ cm list --json | jq '.[0].created_at | fromdateiso8601'
jq: error: date "2023-11-14T14:13:20-08:00" does not match format "%Y-%m-%dT%H:%M:%SZ"
```

Reaching for `sub("[+-][0-9][0-9]:[0-9][0-9]$";"Z")` to fix that produces a *plausible wrong answer*:
measured against a `-08:00` timestamp it returns 1699971200 where the instant is 1700000000, an
eight-hour error with nothing to indicate it. Use `date` instead, which handles the offset:

```sh
t=$(cm info work --field created_at)
date -d "$t" +%s                                      # GNU
date -j -f '%Y-%m-%dT%H:%M:%S%z' "${t%:*}${t##*:}" +%s  # BSD: %z wants the offset without its colon
```

Local time is deliberate: these are read by a person as often as by a script, and UTC would mean
mentally converting every one of them. The `_unix` twin was the alternative and it made every instant two
fields that could disagree.

## Shell startup

`cm shell-init` prints the completions ahead of the shell integration, so a startup file needs one
cm invocation for both rather than two, at a measured 23ms each:

```
eval "$(cm shell-init zsh)"
```

In zsh that has to come after `compinit`, since the completion half asks the shell to register a
completion function. Where that ordering is awkward, the two halves are separable:

```
cm completions zsh > "${fpath[1]}/_cm"
eval "$(cm shell-init zsh --no-completions)"
```

Caching the output is worth it either way, since both halves are the same bytes on every startup.
Loading the completions twice is harmless.

Session names complete dynamically for every command that takes one, annotated with each session's
state, so it is visible that attaching to a `dead` session will restore it rather than join it.
`kill` drops names already on the command line.

Completion never starts a server. It runs on every tab press, and with none running there is nothing
to complete anyway.
