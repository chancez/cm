# Using libghostty-vt from Go

libghostty-vt provides terminal emulation: VT parsing, screen and scrollback state with
reflow, and formatting screen contents back out as plain text, VT sequences, or HTML. It
does not provide pty allocation, process spawning, or an event loop. Those are internal
to the Ghostty app and not exported, so cm implements them.

cm reaches it through `go.mitchellh.com/libghostty`, the Go binding, rather than cgo of its own.

## internal/vt is the only package that uses it

Everything else works with cm's own types and never sees a binding type or a Ghostty handle.
Errors are translated at the boundary.

This is worth enforcing rather than merely intending. Both libghostty's API and the binding's are
unstable by their authors' own description, so a breaking change should mean editing one package.

No package in cm imports "C" any more; the binding owns the cgo. What that removed is the part that
was easiest to get wrong: callback trampolines installed from C, because a Go local's address cannot
be handed to libghostty, and function pointers passed by value where userdata is passed by address.

cgo is still required, and there is no `CGO_ENABLED=0` build. See `docs/architecture.md` for why.

## Why cgo is not a performance problem here

A multiplexer's hot path moves bytes and never inspects cells. libghostty is called once per chunk
of pty output, so the cost is one cgo call of roughly 100ns per chunk. Cell-level reads, where cgo
overhead would actually matter and why the C API offers batch getters, happen only on attach and when
dumping history.

## Two pins, one commit

libghostty has no tagged release, so it is pinned by commit, and two places name that commit:
`GHOSTTY_REF` in `mise.toml`, which every build reads, and the `GIT_TAG` in the binding's own
`CMakeLists.txt`, which moves when `go.mod` does. They must agree, since the binding is written against
the C API at its pin, and `TestGhosttyRefMatchesBinding` fails when they do not.

To bump: `go get go.mitchellh.com/libghostty@<version>`, set `GHOSTTY_REF` to the `GIT_TAG` in that
version's `CMakeLists.txt`, and build. The build fetches and compiles the new pin itself.

## Building

`mise run build`, `test`, `vet` and `install` depend on the `libghostty` task, which builds the pinned
commit when it has not been built yet and returns in about 0.1s when it has. A first build takes about
a minute with a warm zig cache.

Each pin builds into its own directory, `third_party/libghostty-vt/<ref>/`, and `PKG_CONFIG_PATH` in
`mise.toml` points the binding there. A branch on another pin, or a worktree sharing `third_party`
through a symlink, therefore uses its own build rather than rebuilding the one every other checkout
links, and switching back costs nothing. The pkg-config files record their prefix, so a build has to
stay where it was made.

`GHOSTTY_DIR`, `third_party/ghostty`, is only a git repository to fetch into. The build runs from an
exported copy of the pinned commit, so a real ghostty checkout there keeps its working tree, and a
complete clone is not made shallow.

`scripts/libghostty.sh` is the only place the build is written down. The mise task, `Dockerfile.test`,
and both workflows run it, and take the pin, the zig version and the glibc floor from `mise.toml`.
Each used to carry its own copy of the build, the copies drifted, and the task's copy had no coverage
in CI, which is how it shipped broken twice.

Three of its flags are load-bearing. `-Demit-xcframework=false` is required because the default is
"xcodebuild is on PATH", which is true even with only the Command Line Tools stub, and configuring the
iOS target then fails without full Xcode. `-Demit-macos-app=false` avoids building the GUI app that cm
does not use.

`-Doptimize=ReleaseSafe` is the third, and omitting it is a correctness problem rather than a
missed optimization.

Zig defaults to `Debug`, and ghostty derives `slow_runtime_safety` from the optimize mode. In
Debug that turns on integrity verification which walks an entire page, and `insertLines` calls
it once per row it shifts, each call standing up a fresh `DebugAllocator`. A reverse index with
the cursor on the top row goes through `insertLines`, so it cost 14ms at 50x120 against 10us for
the same sequence anywhere else. The cost tracks cell count: 1.8ms at 10x40, 78ms at 100x200. It
does not track scrollback depth, so it is per-operation rather than proportional to history.

The symptom pointed nowhere near the cause. `less` emits home plus reverse index once per line
when paging up, and plain lines when paging down, so scrolling up in `git log` or `git diff`
lagged while scrolling down stayed instant. Measured with a real `less` over a real pty at
50x120, one keypress cost:

| key | Debug | ReleaseSafe |
| --- | --- | --- |
| `d` (down half page) | 1.2ms | 23-41us |
| `u` (up half page) | 145-166ms | 32-91us |

`ReleaseSafe` rather than `ReleaseFast`: measured at 15us against ReleaseFast's 12us for a
half-page scroll, which is not worth giving up bounds and overflow checks in a parser whose input
is whatever a program inside a session decides to print.

Two things guard this, because a build flag is easy to drop and its symptom is easy to
misattribute. `internal/vt/scroll_test.go` asserts on the *ratio* between scrolling up and down
rather than an absolute duration, since a ratio is a property of the build and not of the
machine: about 4x correct, about 45x in Debug. And `cm doctor`'s `slow-emulator` check measures a
half-page scroll at startup, so an installation built wrong says so instead of feeling slow.

The binding links the static archive by default, so cm ships one binary with no runtime library
path to manage. Building it needs `pkg-config`, which macOS gets from Homebrew and Debian has by
default.

## What f54340a changed for cm

Moving from `4d605bf0d` to `f54340a` was meant to change no behavior, and two things had to follow the
emulator to keep that true:

- **It answers more queries itself**: the ANSI form of DECRQM (`CSI 4 $ p`) and DECXCPR (`CSI ? 6 n`).
  `internal/query` decides which queries cm forwards to a client, so it had to learn these too; left as
  unanswered, a client would reply as well and the program would read two answers.
  `TestEmulatorAnsweredQueries` pins the set, and now includes the title report (`CSI 21 t`), which was
  answered before and is opt-in through the binding, so `vt` turns it on.
- **The formatter keeps a screen's trailing blank rows** in VT output, so counting newlines made
  `cm read --raw --lines 1` return a blank row's `\x1b[0m` instead of the last line. `TailVT` counts
  lines with content.

It also needs zig 0.17. What the new version offers beyond that is in `docs/ideas.md`.

## The glibc floor is decided by the zig target, not by the runner

A released Linux binary runs on glibc 2.34 and newer: Ubuntu 22.04+, Debian 12+, RHEL 9+.
`GLIBC_FLOOR` in `mise.toml` holds the number, both workflows and `Dockerfile.test` pass it to
`scripts/libghostty.sh` as the zig target, and release.yml checks the built artifact against it with
`objdump -T` before publishing.

The floor has to be stated because zig decides it silently. Zig's std selects glibc functions at
comptime from the *target* glibc version, so building with the native target on an `ubuntu-24.04`
runner, glibc 2.39, compiled a call to `arc4random_buf`, which appeared in glibc 2.36. That one
symbol made cm 0.4.0 exit with "GLIBC_2.36 not found" on Ubuntu 22.04, whose glibc is 2.35, on both
amd64 and arm64. Pinning the target is the whole fix, measured against the pinned ghostty ref:

| target | symbol referenced |
| --- | --- |
| `x86_64-linux-gnu.2.39` | `arc4random_buf`, glibc 2.36 |
| `x86_64-linux-gnu.2.34` | `getrandom`, glibc 2.25 |

Nothing in ghostty calls either one directly, which is why no amount of reading cm or ghostty
source explains the requirement. It comes from `std.Io.Threaded` and the version check in zig's
`lib/std/c.zig`.

2.34 rather than something older because the floor is a maximum over two link steps, and the zig
one is no longer the higher: the Go and cgo link against the runner's own libc needs 2.34 for
`pthread_create` and the rest of the pthread symbols, which moved into libc there. Lowering the
floor further means doing that link against an older glibc too, either on an older image or with
`zig cc` as the cgo compiler. Deferred rather than rejected; nobody has asked for RHEL 8.

The check on the artifact is the part worth keeping. The release workflow's smoke test runs on the
runner, whose glibc is newer than anything this can break, so a release that cannot start on a
supported distribution otherwise looks entirely green. It was verified both ways against the Linux
test image: a cm built there against the pinned archive references at most GLIBC_2.34 and passes,
while the published v0.4.0 binary fails the same check and prints the symbol that causes it.

## Constraints to respect

Effects callbacks (title changed, pwd changed, write-to-pty, bell) fire **synchronously**
inside `vt_write` and must not re-enter `vt_write` on the same terminal. From Go that
means a callback should only enqueue and return, never call back into code that might
touch the same terminal.

`WithWritePty` must be wired for correctness, not convenience: query
responses the emulator generates have to reach the pty or programs that probe the
terminal will hang waiting.

These replies are now delivered **unconditionally**, and that is a reversal worth stating,
because this file previously argued at length for the opposite.

The old rule was that cm stayed silent whenever an attached client could answer, so a query
travelled out to the real terminal and its reply came back. That needed cm to elect one client
as the answerer, and the election was wrong in four separate ways, each of which shipped: a
read-only follower elected, so nothing answered; a reserved-but-unattached client elected, the
same; two attached clients both replying, so a single `CSI c` came back as
`\x1b[?62;52;c\x1b[?62;52;c`; and across a server restart cm answering a backlog query that the
reconnecting client answered again, which typed a git branch name into a prompt.

cm now answers everything its model can answer, and *asks* one client only for the queries no
model can answer (`OSC 10/11/12`, `OSC 52`, XTGETTCAP, the XTWINOPS pixel reports), relaying the
reply itself. cm is the only writer of a reply to a pty, so there is no election and an
unsolicited reply is discarded. See `docs/architecture.md` for the full reasoning and the two
rejected alternatives.

What survives from the old reasoning is the ordering hazard, which the queue in
`internal/server/queryproxy.go` now handles. `wallfacer -h` sends OSC 11 and blocks reading the
answer. A zsh prompt hook then sent `CSI 6n`, the emulator answered it, and cm wrote
`\x1b[2;1R` to the pty while wallfacer was mid-read. wallfacer took the cursor report as its own
answer and exited, so the terminal's OSC 11 reply arrived unclaimed and the line editor printed
it as `;rgb:2828/2c2c/3434`. Under zsh's vi mode the leading ESC also dropped the editor into
command mode and wedged the prompt. A reply cm can produce immediately therefore queues behind
any question still out with a client.

One consequence for this package: the set of sequences the emulator answers is now load-bearing
in a new way, since it decides which queries cm asks a client for. `TestQuerySetsAgreeWithEmulator`
sweeps 24294 sequences against the live emulator to pin it, and it immediately caught two
misclassifications that reading the source did not: libghostty answers the `OSC 4` palette query,
which had been assumed terminal-only, and it ignores `OSC 4;c;?`, which parses structurally while
naming no palette entry.

Mouse and focus events are still forwarded from every client, unchanged: they describe one window
rather than the session, so each client sends its own.

## What cm reports about itself

XTVERSION (`CSI > q`) answers `cm <version>`. Unwired it answers `libghostty`, which is true
about the emulator and misleading about the terminal, since a program asking what it is
talking to gets the name of a library rather than the multiplexer holding its pty. It only
reaches a program when nothing is attached, which is exactly when cm *is* the terminal.

The value is process-wide, set once with `vt.SetXtversion`. That used to be forced: libghostty requires
the reply to outlive its callback, so it lived in one C buffer, and two terminals constructed with
different values both reported the second. The binding's callback returns a string per terminal, so
per-session identity is possible now; cm reports its own version from every session and has no use for
it, so the API is unchanged.

DA1 is *not* wired, and the reply cm sends when detached (`?62;22c`, vt220 plus `ansi_color`)
comes from a Zig struct default rather than a decision. Left alone deliberately: with a client
attached the terminal answers for itself, so kitty's real `?62;52;c` including the clipboard
bit reaches programs unchanged, measured identical inside cm and outside. A detached session
has no clipboard, no sixel, and no window, so advertising fewer capabilities there is closer
to true than copying a terminal's answer would be.

OSC 52 needs nothing from cm and works today: verified by writing to the clipboard from inside
a session and reading it back outside. Programs detect it through terminfo (`xterm-kitty` has
`Ms=\E]52;%p1%s;%p2%s\E\\`) rather than through DA1, and cm passes `TERM` through unchanged, so
the capability a program sees is the real terminal's.

An earlier attempt stripped these queries from client-bound output so cm was the sole
answerer. That fixed a visible duplicate DA1 reply but not the injection above, and it is
incompatible with letting the terminal answer, so it was removed.

Kitty graphics are restored on reattach by cm rather than by the formatter, which does not re-emit
them. See "Kitty graphics" in `docs/architecture.md`.
