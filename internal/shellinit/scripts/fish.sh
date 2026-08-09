# cm shell integration for fish. Load with:  cm shell-init fish | source
#
# Provides cm_report, which states what cm cannot work out for itself: that something is blocked, waiting
# for input rather than working. cm already derives busy, idle, the exit status, and command timing from
# the OSC 133 markers a shell or a prompt emits, so none of that is duplicated here.

# The whole body is conditional rather than guarded by an early exit, and that detail is load-bearing. The
# documented way to load this is `cm shell-init fish | source`, where `exit` would end the *user's shell*
# rather than the script. The sibling scripts have the same shape for the same class of reason: a `return`
# in an eval silently skips the rest of a zsh rc file, and bash refuses one outside a sourced file.
#
# The two conditions: nothing to do outside a cm session, which is what makes this safe to load
# unconditionally from config.fish, and nothing to do if already loaded.
if set -q CM_SESSION; and not set -q _cm_loaded
set -g _cm_loaded 1

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
function cm_report --argument-names state detail
    if test -n "$detail"
        set detail (string replace --all -- '\\' '\\\\' $detail)
        set detail (string replace --all -- ';' '\\;' $detail)
        printf '\033]25453;state=%s;detail=%s;source=fish\007' $state $detail > /dev/tty 2>/dev/null
    else
        printf '\033]25453;state=%s;source=fish\007' $state > /dev/tty 2>/dev/null
    end
end

# Deliberately no fish_prompt hook. A shell at its prompt is idle, not blocked, so reporting blocked there
# would mark every session blocked forever and make the state useless to wait for. Blocked cannot be
# detected from outside the program that is blocked, which is why this defines the cheap way to say it and
# leaves the saying to whatever knows.

end
