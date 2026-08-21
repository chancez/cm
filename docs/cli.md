# CLI

Notes on using cm from a script or a shell startup file. For the config file, see
[config.md](config.md).

## JSON output

`list`, `info`, `kill`, `get-env`, `tag`, `wait`, `send`, `run`, `status`, `doctor`, `config`,
`version`, `detach`, `clients`, and `clients list` accept `--json`.

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
