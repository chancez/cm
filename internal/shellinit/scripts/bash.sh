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

# No hook reports *state*, and that is still deliberate. A shell at its prompt is idle, not blocked, so
# reporting blocked there would mark every session blocked forever and make the state useless to wait for.
# Blocked cannot be detected from outside the program that is blocked, which is why this defines the cheap
# way to say it and leaves the saying to whatever knows.

# The hooks below report something else: which command this shell is running, as a frame that opens when it
# starts and closes when the shell is next at its prompt. What needs it, and why it is not derived from
# OSC 133, is in the zsh script and in docs/ideas.md on a session's location.
#
# bash has no preexec, so this uses PS0, which bash expands and prints immediately before running a command.
# That needs bash 4.4, and macOS ships 3.2 as /bin/bash: there the frame hooks are simply not installed, and
# cm falls back to what it had before, which is the cwd and the busy flag. Checked rather than assumed
# because an unset PS0 on 3.2 is silently ignored, so the failure would be "no frames, no reason given".
if [ "${BASH_VERSINFO[0]}" -gt 4 ] || { [ "${BASH_VERSINFO[0]}" -eq 4 ] && [ "${BASH_VERSINFO[1]}" -ge 4 ]; }; then
_cm_frame_salt="${RANDOM}${RANDOM}"
_cm_frame_n=0
_cm_frame_id=""

# The id is minted at the prompt rather than when the command starts, which is not how the other two shells
# do it and is forced by what PS0 is.
#
# bash expands PS0 in a *subshell*, so an id assigned there never reaches the shell that has to close the
# frame. The first version of this did exactly that: every frame was announced with the same id, the parent's
# variable stayed empty, and nothing ever closed, which for the collector means an announcement that is never
# discarded. The pty test caught it because it asserts a close arrives, not just an open.
#
# So the prompt hook owns the id and PS0 only prints it. The cost is an exit for a frame that was never
# opened, which is what pressing enter on an empty line produces: PS0 does not run, and the next prompt
# closes an id cm never saw. That is deliberately harmless -- an exit whose id is not on the stack closes
# nothing -- and it is the same tolerance that lets a remote shell's frames travel the same pty.
_cm_frame_prompt() {
  if [ -n "$_cm_frame_id" ]; then
    printf '\033]25453;frame=exit;id=%s\007' "$_cm_frame_id" > /dev/tty 2>/dev/null
  fi
  _cm_frame_n=$((_cm_frame_n + 1))
  _cm_frame_id="${_cm_frame_salt}-${_cm_frame_n}"
}

# _cm_frame_enter prints the open, and runs inside PS0's subshell where it can print but not remember.
#
# The command line comes from the history rather than from an argument, since PS0 is a string bash expands
# rather than a hook it calls. `history 1` is the line just entered, with its history number stripped. With
# history disabled there is none and the frame carries no argv, which is a frame with less information rather
# than no frame: the pairing is what the collector needs.
#
# Escaped like a report's detail, since a semicolon separates fields on the wire, and newlines collapsed so a
# multi-line command cannot put one in a value that reaches `cm list --json`.
_cm_frame_enter() {
  local cmd
  cmd=$(HISTTIMEFORMAT= history 1 2>/dev/null)
  cmd=${cmd#*[0-9]  }
  cmd=${cmd//\\/\\\\}
  cmd=${cmd//;/\\;}
  cmd=${cmd//$'\n'/ }
  printf '\033]25453;frame=enter;id=%s;argv=%s\007' "$_cm_frame_id" "${cmd:0:256}" > /dev/tty 2>/dev/null
}

# Appended rather than assigned, in both cases, because a user's own PS0 and PROMPT_COMMAND are theirs. The
# array form of PROMPT_COMMAND exists from 5.1 and is what bash prefers there; assigning a string to it on
# those versions works but replaces every element, which would silently drop whatever else a prompt
# framework installed.
PS0='$(_cm_frame_enter)'"${PS0-}"
if [ "${BASH_VERSINFO[0]}" -gt 5 ] || { [ "${BASH_VERSINFO[0]}" -eq 5 ] && [ "${BASH_VERSINFO[1]}" -ge 1 ]; }; then
  PROMPT_COMMAND+=(_cm_frame_prompt)
else
  PROMPT_COMMAND="_cm_frame_prompt${PROMPT_COMMAND:+;$PROMPT_COMMAND}"
fi
fi

fi
