# Alternatives, and where cm sits among them

cm exists because of tmux, zmx, and libghostty rather than in spite of them. Two of the three are
what a reader is most likely already using, and the third is the reason cm was worth writing at all.

This file is for deciding whether to use cm. It is not a benchmark or a feature matrix: those go stale
and they flatter whoever wrote them. Where a claim here is measurable it names the measurement, and
where cm is behind, that is stated rather than omitted.

Compared against tmux 3.5a, and zmx at `c33945d`, which is past v0.7.0. The zmx version matters more
than it looks: an earlier draft of this page said zmx needed shell hooks for title and directory, which
was true of the released 0.7.0 binary and not of the source, where OSC 7 tracking landed in `85b045c`.
A comparison written against whatever happens to be installed is a comparison with a date on it, so
check before trusting any claim here about another tool.

The short version, if you read nothing else:

- **tmux** if you want one tool that does everything, including windows and splits, or you work over
  ssh on machines where it is already installed.
- **zmx** if you want session persistence around a terminal that does its own layout, and prefer a
  single daemon-per-session design with no separate server.
- **cm** if you want that same thing plus a server you can restart under running shells, sessions
  whose *content* survives a reboot, and primitives for driving sessions from scripts.
- **libghostty** is not an alternative. It is the terminal emulator inside cm, and inside zmx.

## The one decision behind all the others

cm keeps no windows, tabs, or splits, and never will. Your terminal emulator already has them.

That is the whole reason cm is small: 23 commands against tmux's 90 subcommands. It is also the
reason cm is the wrong choice for some people, which is worth saying plainly. If your terminal cannot
split, or you spend your day on remote machines whose terminal is whatever the ssh client provides,
then a multiplexer that refuses to lay out windows is refusing to solve your actual problem. tmux is
the right tool there and this is not a close call.

The trade goes the other way once your terminal does layout well. Two multiplexers competing over
the same job means two sets of keybindings, two scrollbacks, two notions of a window, and a
copy-and-paste story that works differently depending on which layer you are in. cm gives that job
up entirely.

## tmux

The default, and the one with nearly two decades of accumulated reasons to exist. It is more capable
than cm on almost every axis and will remain so.

**Where tmux wins.** Windows, panes, and layouts. Ubiquity, so it is already on the server you just
ssh'd into. A scripting surface that can drive anything in the program, plus a configuration language
and a plugin ecosystem. Portability far beyond macOS and Linux. Copy mode, which cm has no equivalent
of because the terminal's own scrollback and selection are expected to do that work.

**Where cm differs, and why.**

*Restarting the multiplexer.* cm's shim owns the pty and holds no state the server cannot rediscover,
so `cm server stop` leaves every shell running and the next server adopts them. That is the upgrade
path: replace the binary, restart the server, keep working. In tmux the server owns the ptys, so
restarting it ends the sessions.

*Content across a reboot.* A pty is a kernel object and a shell is a process, so neither survives a
reboot in any multiplexer, cm included. What cm can do is bring the *content* back: scrollback, the
last screen, and the working directory, with a fresh shell started in it. cm is careful not to blur
those two guarantees, because "my session came back" must never be mistaken for "my process is still
running". See [persistence.md](persistence.md).

*Session environment.* A shell captures its terminal's environment once, at startup, so reattaching
from a terminal that has restarted leaves it holding a dead `KITTY_LISTEN_ON` or `SSH_AUTH_SOCK`.
Both tools solve this and they agree on more than they differ: `cm get-env` and tmux's
`show-environment` both report *removals* as well as changes, and both spell a removal `-NAME`,
because a variable that vanished keeps its stale value if only assignments are emitted, and a stale
socket is worse than an absent one.

The one real difference is shell syntax. `show-environment -s` always emits POSIX, which is wrong in
fish on every count: a bare assignment there is a per-command prefix, `export` is not a builtin, and
unset is `set -e`. cm makes `--format` explicit and refuses to guess, because `$SHELL` is the login
shell rather than the one running in the session, and those differ precisely when guessing wrong is
most confusing. cm also limits removals to variables it manages, so a prompt hook cannot delete
unrelated parts of the environment. See [config.md](config.md).

*Driving a session from a script.* cm has `wait`, `read`, `send --wait`, and a `report` mechanism a
program uses to say what it is doing. These exist for orchestrating work, and the design constraint
is that cm never learns *what* is running: a reported state is just a state, and nothing in cm
special-cases a build tool or an agent.

**What is genuinely unresolved.** `ssh host cm attach work` works and covers the plain case, but
everything in that session then belongs to the remote terminal, so a local terminal's own layout and
clipboard are not in the picture. tmux over ssh is a workflow millions of people rely on, and the
`--remote ssh://` form that would answer it properly is an idea rather than a feature. See
[ideas.md](ideas.md).

Being new is also a real cost next to a tool that has been deployed everywhere for years. cm has far
fewer users, so fewer of its edge cases have been found, and it runs on macOS and Linux only.

## zmx

The closest relative, and the direct ancestor. cm exists because using zmx made a slightly different
set of trade-offs look worth having, not because zmx is wrong.

zmx and cm agree on the thing that matters most: **the terminal emulator does layout, the multiplexer
does persistence.** Both are built on libghostty for VT emulation. If you are choosing between them,
you are choosing between two implementations of the same idea rather than two ideas.

**Where cm inherited from zmx directly, and says so.** Screen restore is a port of zmx's
`serializeTerminalState`, and nearly every detail in it is a fix for a bug zmx found first: the
scrollback-shift on resize (zmx issue 31), the NUL sentinel kitty writes into its own session file
and then cannot parse (issue 222), OSC 2 placement (issue 224), and the cursor-coordinate case where
the prompt never comes back (issue 111). Those issue numbers are in [restore.md](restore.md) because
credit for a fix belongs with whoever hit the bug. zmx also arrived first at the daemon-per-session
blast-radius argument, at never marking a session dead on a timeout, and at gating query replies on
whether anyone else will answer.

**Where cm differs.**

*A server between clients and sessions.* zmx has a daemon per session and no central server; cm adds
one, and clients never talk to a shim. That buys one place for fanout, session bookkeeping, and
terminal state, and it makes remote access a gateway concern rather than a matter of exposing every
session. It costs a process and a hop.

*Nested sessions.* zmx treats its session environment variable as a request to *switch* the parent
terminal's session, so attaching from inside a session hijacks the window you ran it from; an
upstream fix was withdrawn with the conclusion that nesting is unsupported. cm never treats that
variable as a target, so attaching from inside a session creates a nested one. This matters more than
it sounds for per-window sessions, where every manual attach is nested by construction. cm also
tracks the nesting so a parent stops attributing its child's reports to itself.

*Reboot persistence.* cm can bring a session's content back after a reboot, opt-in per session or by
name pattern. zmx 0.7.0 keeps its sockets under `TMPDIR` and records no content outside its logs, so
its persistence is across a terminal closing rather than across a reboot.

*Saying what a session is doing.* Both read OSC 2 and OSC 7 for title and directory, so this is not
the difference it looks like: zmx tracks cwd from OSC 7 and replays the title on attach, and both
decode the URI's host so a session that has ssh'd away is not treated as local.

The difference is command state, and it is a design choice rather than a gap. cm derives "a command is
running" from the OSC 133 markers a shell or prompt already emits. zmx reads OSC 133 as well, but only
to rewrite `redraw=0` into prompt markers, which cm also does; it does not derive a running command
from them, so describing a session means labelling it with `zmx set`.

Each way costs something. A label is explicit and holds whatever the shell does, including across a
restart; a derived state needs no maintenance but exists only while the markers do, so a cm session
adopted by a new server reports idle until its next command. Derived state is not restricted in what
it can say, though: a zmx label value is limited to `[a-zA-Z0-9-_.]`, so a command line has to be
mangled to fit, where cm reports it as sent.

*Detaching by name.* Both have a detach command, and both have a way out of the key colliding with an
inner program: zmx added `ZMX_NO_DETACH_KEY`, which passes ctrl+\ through as ordinary input, where cm
has `detach_key` in the config file and `--detach-key` per attachment, including `none`.

They differ in what the command targets. `zmx detach` takes no name and detaches all of a session's
clients; `cm detach` takes a session name, `--all`, or a tag selector. Naming one is what a nested
attach needs, since the key reaches whichever client owns the real terminal. See
[config.md](config.md).

**Where zmx is ahead or simply different.** It is a smaller system with one less moving part, and if
you do not want a server you should not have one. Being written in Zig it links libghostty directly
rather than through cgo, which cm pays for by requiring cgo in every build. Completions cover
bash, zsh, fish, and nu, where cm covers bash, zsh, fish, and powershell, so nu users are better
served there.

zmx also has `print` and `write`, which inject text into a session's display and write stdin to a file
through the session. cm has no equivalent of either.

**A note on testing, which is the practical reason to keep zmx installed.** The right control for a
suspected multiplexer bug is *another multiplexer*, not a bare terminal. Comparing cm against bare
kitty once concluded "not a cm bug" about a bug that reproduced against zmx on the first try. cm's
own leak test was found the same way: cm leaked 2 of 3 runs where zmx leaked 0 of 3. See
[testing.md](testing.md).

## libghostty

Not an alternative, and listed here because it is easy to mistake for one. libghostty-vt is the
terminal emulator cm uses: VT parsing, screen and scrollback with reflow, and formatting contents
back out as text, VT sequences, or HTML. It does not provide pty allocation, process spawning, or an
event loop; those are internal to the Ghostty app, so cm implements them.

Using it rather than writing an emulator is the single largest reason cm is a small program. A
correct VT implementation is years of work and the failures are subtle in ways that make them
expensive to find. zmx made the same call.

Two things follow that a user will notice.

**cgo is required.** `internal/vt` wraps the C API, and `cm read`, `cm history`, and screen restore
all depend on it. There is no pure-Go build.

**The API is unstable by upstream's own description**, so `mise.toml` pins a commit and bumping it is
deliberate.

The cost is not where people expect. A multiplexer's hot path moves bytes and never inspects cells,
so libghostty is called once per pty read, and the kernel caps a read from a pty master at 4KB: about
one 100ns cgo call per 4KB of output. Cell-level reads happen only on attach and when dumping
history. The one place the build *did* matter was optimization mode, where a missing
`-Doptimize=ReleaseSafe` made paging up in `less` cost 145-166ms per keypress against 32-91us. See
[libghostty.md](libghostty.md).

## Shared limitations

Worth stating because they are properties of the approach rather than of any one implementation, and
because a comparison that lists only strengths is not useful.

- **Kitty graphics do not survive a reattach.** They pass through as APC bytes while live, but
  libghostty's formatter does not re-emit them, so images are absent after reattaching. zmx has the
  same gap for the same reason.
- **Alternate-screen scrollback is not recoverable.** A full-screen program draws on the alternate
  screen and lines that scroll off there never entered scrollback. This is correct terminal behavior
  and the most confusing limitation in practice, because the symptom is output that looks truncated
  for no reason.
- **Two multiplexers on one pty is worse than one.** If you nest cm inside tmux or the reverse, the
  detach keys collide and the outer one wins. cm's `--detach-key` exists for exactly this.

## If you are still deciding

Install cm alongside whatever you use now. Nothing here conflicts: cm has its own socket and its own
state directory, and it needs no shell configuration to work. `cm shell-init` exists and adds a
`cm_report` helper, but it is optional, and the command and directory reporting come from OSC 133 and
OSC 7, which a modern shell or prompt already emits.

Start one session in it, work in that session for a day, and see whether giving layout back to your
terminal feels like a loss or a relief. That answer is personal and no comparison table can supply it.
