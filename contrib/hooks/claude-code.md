# Claude Code

An example of wiring an agent's existing hooks to `cm report`. Nothing in cm knows about Claude Code; this
is one adapter, and any program with equivalent hooks is wired the same way.

Claude Code has the two events that matter:

- `Notification` with a `permission_prompt|idle_prompt` matcher fires when it wants an answer, which is
  `blocked`.
- `Stop` fires when it finishes a turn, which is `idle`.

In `~/.claude/settings.json`:

```json
{
  "hooks": {
    "Notification": [
      {
        "matcher": "permission_prompt|idle_prompt",
        "hooks": [
          {
            "type": "command",
            "command": "cm-report-hook blocked \"$(jq -r .message)\"",
            "timeout": 10
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          { "type": "command", "command": "cm-report-hook idle", "timeout": 10 }
        ]
      }
    ]
  }
}
```

The `Notification` payload arrives on stdin as JSON, so `jq -r .message` turns it into the detail shown in
`cm list`. Drop that if `jq` is not available; the state is the useful part and the detail is a nicety.

If you already have scripts on these hooks, add a line rather than replacing them: hooks compose, and
reporting to cm has nothing to do with sending a desktop notification.

There is no `busy` here. Claude Code has no "starting work" hook, and it does not need one: the absence of
a report leaves cm's own OSC 133 view in charge, which already says a command is running. `blocked` and
`idle` are the two states cm cannot work out for itself.

## Checking it works

```
cm list                              # STATE shows running(blocked: ...) when it wants an answer
cm wait <session> --until blocked    # blocks until it does
cm report <session> --state clear    # if a stale report is left behind
```
