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

# No hook reports *state*, and that is still deliberate. A shell at its prompt is idle, not blocked, so
# reporting blocked there would mark every session blocked forever and make the state useless to wait for.
# Blocked cannot be detected from outside the program that is blocked, which is why this defines the cheap
# way to say it and leaves the saying to whatever knows.

# The hooks below report something else: which command this shell is running, as a frame that opens when it
# starts and closes when it returns. What needs it, and why it is not derived from OSC 133, is in the zsh
# script and in docs/ideas.md on a session's location.
#
# fish is the one shell with both events, so the close is precise here: fish_postexec fires when the command
# returns rather than when the next prompt is drawn.
set -g _cm_frame_salt (random)(random)
set -g _cm_frame_n 0
set -g _cm_frame_id ""

# Escaped like a report's detail, since a semicolon separates fields on the wire, and newlines collapsed so a
# multi-line command cannot put one in a value that reaches `cm list --json`.
function _cm_frame_enter --on-event fish_preexec --argument-names cmd
    set cmd (string replace --all -- '\\' '\\\\' $cmd)
    set cmd (string replace --all -- ';' '\\;' $cmd)
    set cmd (string join ' ' (string split \n -- $cmd))
    set -g _cm_frame_n (math $_cm_frame_n + 1)
    set -g _cm_frame_id "$_cm_frame_salt-$_cm_frame_n"
    printf '\033]25453;frame=enter;id=%s;argv=%s\007' $_cm_frame_id (string sub -l 256 -- "$cmd") > /dev/tty 2>/dev/null
end

# Guarded on a frame being open, since an empty command line at the prompt raises fish_postexec with nothing
# to close.
function _cm_frame_exit --on-event fish_postexec
    test -n "$_cm_frame_id"; or return 0
    printf '\033]25453;frame=exit;id=%s\007' $_cm_frame_id > /dev/tty 2>/dev/null
    set -g _cm_frame_id ""
end

end
