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
cm_report() {
  local state=$1 detail=${2//[[:cntrl:]]/}
  [[ $state == busy ]] && state=working
  if [[ -n "$detail" ]]; then
    printf '\033]7501;state=%s:app=zsh:msg=%s\007' "$state" \
      "$(printf '%s' "$detail" | base64 | tr -d '\n')" > /dev/tty 2>/dev/null
  else
    printf '\033]7501;state=%s:app=zsh\007' "$state" > /dev/tty 2>/dev/null
  fi
}

# No hook reports *state*, and that is still deliberate.
#
# The obvious thing to write is "report blocked at each prompt, clear it when a command starts", and it is
# wrong: a shell sitting at its prompt is idle, not blocked. Reporting otherwise would mark every session
# blocked forever, which destroys the distinction that makes the state worth having -- `cm wait --until
# blocked` would match every session immediately.
#
# Blocked cannot be detected from outside the program that is blocked. That is not a gap in this script, it
# is the reason cm has a report mechanism at all: only the program knows whether it is computing or waiting
# for an answer. So this defines the cheap way to say it and leaves the saying to whatever knows.

# The hooks below report something else: which command this shell is running, as a frame that opens when it
# starts and closes when the shell is next at its prompt.
#
# Two things need it. cm has no model of *where* a session is: it has a cwd and a busy flag, both single
# values derived from bytes, and no notion of "this session is inside something, entered by this command".
# And a `cm attach` beyond an ssh announces itself over this same sequence so the outer window hands over its
# detach key, with no way to withdraw that if the link drops; an announcement bound to a frame is discarded
# when the frame closes, so the parent stops believing in a client that is gone.
#
# Not derived from OSC 133, which looks like it should serve. kitty's integration does send the command line
# on the 133;C marker, and cm parses it, then clears it when a prompt marker arrives -- because a prompt
# arriving mid-command means either a nested shell or a shell that prompted after an interrupt, and OSC 133
# carries nothing to tell those apart. See docs/ideas.md on a session's location.
#
# The id is a per-shell salt plus a counter rather than a pid. Frames from two shells travel the same pty
# once an ssh is involved, and two hosts can hand out the same pid, so a pid would let a remote frame close
# a local one.
autoload -Uz add-zsh-hook
typeset -g _cm_frame_salt="${RANDOM}${RANDOM}"
typeset -gi _cm_frame_n=0
typeset -g _cm_frame_id=""

# _cm_frame_enter opens a frame for the command about to run. preexec is passed the command line.
#
# The command line is escaped like a report's detail, and for the same reason: a semicolon separates fields
# on the wire, so an unescaped one would truncate the command at it. Newlines become spaces, since a
# multi-line command would otherwise put a newline in a value that reaches `cm list --json`. Truncated to
# 256 characters, which cm also enforces; this keeps the sequence small at the source.
#
# Named cmd rather than argv on purpose: argv is zsh's positional parameters, and a local by that name
# inside a hook shadows them.
_cm_frame_enter() {
  local cmd=$1
  cmd=${cmd//\\/\\\\}
  cmd=${cmd//;/\\;}
  cmd=${cmd//$'\n'/ }
  _cm_frame_id="${_cm_frame_salt}-$(( ++_cm_frame_n ))"
  printf '\033]25453;frame=enter;id=%s;argv=%s\007' "$_cm_frame_id" "${cmd[1,256]}" > /dev/tty 2>/dev/null
}

# _cm_frame_exit closes the open frame, which is what tells cm the command returned.
#
# Guarded on a frame being open, because precmd also runs before the first prompt of a shell, where there is
# nothing to close. Closing by id rather than "the top frame" is what makes an ssh chain work: an id cm does
# not hold closes nothing.
_cm_frame_exit() {
  [[ -n $_cm_frame_id ]] || return 0
  printf '\033]25453;frame=exit;id=%s\007' "$_cm_frame_id" > /dev/tty 2>/dev/null
  _cm_frame_id=""
}

add-zsh-hook preexec _cm_frame_enter
add-zsh-hook precmd _cm_frame_exit

fi
