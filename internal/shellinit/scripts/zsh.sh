# cm shell integration for zsh. Load with:  eval "$(cm shell-init zsh)"
#
# Provides cm_report, which states what cm cannot work out for itself: that something is blocked, waiting
# for input rather than working. cm already derives busy, idle, the exit status, and command timing from
# the OSC 133 markers a shell or a prompt like starship emits, so none of that is duplicated here.

# The whole body is conditional rather than guarded by an early `return`, and that detail is load-bearing.
#
# The documented way to load this is `eval "$(cm shell-init zsh)"`, and a `return` inside an eval does not
# just end the script: in zsh it returns from the enclosing scope, so everything after the eval in the
# user's .zshrc is silently skipped. Verified directly -- `zsh -c 'eval "return 0"; echo AFTER'` prints
# nothing. bash is differently broken in the same place, refusing a `return` outside a sourced file and
# printing an error on every shell startup. A conditional is correct in both, sourced or eval'd.
#
# The two conditions: nothing to do outside a cm session, which is what makes this safe to load
# unconditionally from an rc file, and nothing to do if already loaded, so sourcing twice does not redefine
# what is there.
if [[ -n "$CM_SESSION" && -z "$_cm_loaded" ]]; then
typeset -g _cm_loaded=1

# cm_report tells cm what this session is doing.
#
#   cm_report blocked "waiting for approval"   # needs input, will not progress without it
#   cm_report busy    "running tests"          # working on something
#   cm_report idle                             # finished, waiting on nothing
#   cm_report clear                            # withdraw the report, back to what cm derives
#
# A printf rather than a call to `cm report`, which is the entire reason this exists. Measured: any cm
# invocation costs about 23ms, so a wrapper around a command that runs per prompt or per tool call adds up
# fast, while this costs nothing. It also works with no server running, so a report is not lost while the
# server is restarting.
#
# Written to /dev/tty rather than stdout, so a report still reaches cm when the caller's output is
# redirected to a file or a pipe. cm reads the pty, so anything else would report into the wrong place.
#
# Semicolons and backslashes in the detail are escaped, since they separate and quote fields on the wire.
# Without this a detail containing a semicolon would be silently cut short at it.
#
# The detail is passed as an argument to printf and never interpolated into its format string, which is not
# a style preference. printf interprets escapes in its format, so a detail reaching it that way is mangled
# twice over: a literal backslash-b becomes a backspace byte, and a percent sign becomes a format
# specifier that consumes whatever follows. Both were verified before writing it this way.
cm_report() {
  local state=$1 detail=$2
  if [[ -n "$detail" ]]; then
    detail=${detail//\\/\\\\}
    detail=${detail//;/\\;}
    printf '\033]25453;state=%s;detail=%s;source=zsh\007' "$state" "$detail" > /dev/tty 2>/dev/null
  else
    printf '\033]25453;state=%s;source=zsh\007' "$state" > /dev/tty 2>/dev/null
  fi
}

# Deliberately no precmd or preexec hook.
#
# The obvious thing to write here is "report blocked at each prompt, clear it when a command starts", and
# it is wrong: a shell sitting at its prompt is idle, not blocked. Reporting otherwise would mark every
# session blocked forever, which destroys the distinction that makes the state worth having -- `cm wait
# --until blocked` would match every session immediately.
#
# Blocked cannot be detected from outside the program that is blocked. That is not a gap in this script, it
# is the reason cm has a report mechanism at all: only the program knows whether it is computing or waiting
# for an answer. So this defines the cheap way to say it and leaves the saying to whatever knows.

fi
