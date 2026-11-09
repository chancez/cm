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

# cm_report tells cm what this session is doing, as a program status report (OSC 7501).
#
#   cm_report blocked "waiting for approval"   # needs input, will not progress without it
#   cm_report working "running tests"          # running on its own (busy is accepted too)
#   cm_report done    "tests passed"           # finished, the result is ready to look at
#   cm_report error   "tests failed"           # failed and stopped
#   cm_report idle                             # at rest, waiting for its next instruction
#   cm_report clear                            # withdraw the report, back to what cm derives
#
# working and blocked end at the next prompt, since whatever was working has stopped by then; done and
# error stay until replaced. See `cm report --help`.
#
# A printf rather than a call to `cm report`, which is the entire reason this exists: any cm invocation
# costs about 23ms. Measured in zsh: 0.04ms for a report with no detail, and 4.2ms with one, which is the
# subshell and the base64 the protocol requires for text. It also works with no server running, so a
# report is not lost while the server is restarting.
#
# Written to /dev/tty rather than stdout, so a report still reaches cm when the caller's output is
# redirected to a file or a pipe. cm reads the pty, so anything else would report into the wrong place.
#
# Control characters are removed from the detail first, because the protocol discards a whole report whose
# text contains one: a detail with a newline in it would otherwise make the report vanish.
#
# The detail reaches printf as an argument, never in its format string, which interprets escapes: a
# literal backslash-b would become a backspace and a percent sign a format specifier.
function cm_report --argument-names state detail
    test "$state" = busy; and set state working
    set detail (string replace --all --regex -- '[[:cntrl:]]' '' $detail)
    if test -n "$detail"
        printf '\033]7501;state=%s:app=fish:msg=%s\007' $state \
            (printf '%s' $detail | base64 | tr -d '\n') > /dev/tty 2>/dev/null
    else
        printf '\033]7501;state=%s:app=fish\007' $state > /dev/tty 2>/dev/null
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
