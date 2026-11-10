# Claude Code

Claude Code writes the program status protocol, OSC 7501, itself, so it needs no hook. Seen in 2.1.304: it
sends the support query, `OSC 7501 ; ?`, at startup, which cm answers, and reports its state as
`app=claude-code`, with a `kind` when blocked. cm reads those from the session's output and passes them
through unchanged, so an outer terminal that implements the protocol, kitty or ghostty, gets them too.

## Do not add the hook as well

An earlier version of this page wired Claude Code's `Notification` and `Stop` hooks to `cm-report-hook`.
Alongside the native reports that is worse than nothing:

- Both write the session's root record, so whichever came last replaces the other. The hook's carries a
  detail and `source=hook`, and drops the `kind`, progress and app Claude Code sent.
- `cm report` is an RPC and writes no bytes, so the hook's state reaches `cm list` and never the outer
  terminal. The two then disagree about the same session.
- The `idle_prompt` notification fires when Claude Code is waiting for the next message, which the hook
  reported as `blocked`. Claude Code reports the same moment as `idle`.

If you have those entries in `~/.claude/settings.json`, remove them.

## A Claude Code that does not write OSC 7501

`cm list` showing `running` with no state while Claude Code waits on a permission prompt means this
version does not report. Then the hooks are the fallback:

```json
{
  "hooks": {
    "Notification": [
      {
        "matcher": "permission_prompt",
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
`cm list`. There is no `working`: no hook fires when a turn starts, and without a report cm's own OSC 133
view already says a command is running.

## Checking it works

```
cm info <session> --json | jq '{reported_state, reported_source}'   # source "claude-code" is the native report
cm wait <session> --until blocked                                    # blocks until it wants an answer
```
