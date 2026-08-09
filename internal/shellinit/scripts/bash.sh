# cm shell integration for bash. Load with:  eval "$(cm shell-init bash)"
#
# Provides cm_report, which states what cm cannot work out for itself: that something is blocked, waiting
# for input rather than working. cm already derives busy, idle, the exit status, and command timing from
# the OSC 133 markers a shell or a prompt emits, so none of that is duplicated here.

# The whole body is conditional rather than guarded by an early `return`, which is not a style choice.
# `return` outside a function is only valid in a *sourced* file, and the documented way to load this is
# `eval "$(cm shell-init bash)"`, where it is not sourced: bash prints "return: can only `return' from a
# function or sourced script" on every shell startup. A conditional is valid both ways.
#
# The two conditions: nothing to do outside a cm session, which is what makes this safe to load
# unconditionally from an rc file, and nothing to do if already loaded, so sourcing an rc file twice does
# not redefine what is there.
if [ -n "$CM_SESSION" ] && [ -z "$_cm_loaded" ]; then
_cm_loaded=1

# cm_report tells cm what this session is doing.
#
#   cm_report blocked "waiting for approval"   # needs input, will not progress without it
#   cm_report busy    "running tests"          # working on something
#   cm_report idle                             # finished, waiting on nothing
#   cm_report clear                            # withdraw the report, back to what cm derives
#
# A printf rather than a call to `cm report`, which is the entire reason this exists: any cm invocation
# costs about 23ms, while this costs nothing, and this works with no server running.
#
# Written to /dev/tty rather than stdout, so a report still reaches cm when the caller's output is
# redirected. cm reads the pty, so anything else would report into the wrong place.
#
# Semicolons and backslashes in the detail are escaped, since they separate and quote fields on the wire.
#
# The detail is passed as an argument to printf and never interpolated into its format string. printf
# interprets escapes in its format, so a detail reaching it that way is mangled twice over: a literal
# backslash-b becomes a backspace byte, and a percent sign becomes a format specifier that consumes
# whatever follows.
cm_report() {
  local state=$1 detail=$2
  if [ -n "$detail" ]; then
    detail=${detail//\\/\\\\}
    detail=${detail//;/\\;}
    printf '\033]25453;state=%s;detail=%s;source=bash\007' "$state" "$detail" > /dev/tty 2>/dev/null
  else
    printf '\033]25453;state=%s;source=bash\007' "$state" > /dev/tty 2>/dev/null
  fi
}

# Deliberately no PROMPT_COMMAND hook. A shell at its prompt is idle, not blocked, so reporting blocked
# there would mark every session blocked forever and make the state useless to wait for. Blocked cannot be
# detected from outside the program that is blocked, which is why this defines the cheap way to say it and
# leaves the saying to whatever knows.

fi
