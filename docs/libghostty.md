# Using libghostty-vt from Go

libghostty-vt provides terminal emulation: VT parsing, screen and scrollback state with
reflow, and formatting screen contents back out as plain text, VT sequences, or HTML. It
does not provide pty allocation, process spawning, or an event loop. Those are internal
to the Ghostty app and not exported, so cm implements them.

The API is explicitly unstable while the behavior is stable, and there is no tagged
release, so `mise.toml` pins a commit. Bumping `GHOSTTY_REF` is deliberate.

## All cgo lives in internal/vt

`internal/vt` is the only package that imports "C". Everything else works with Go types
and never sees a `Ghostty*` handle, an `unsafe.Pointer`, or a `GhosttyResult`. Errors are
translated at the boundary, and C memory is freed there.

This is worth enforcing rather than merely intending. The API is unstable by upstream's
own description, so a breaking change should mean editing one package.

It no longer keeps `CGO_ENABLED=0` builds possible: cgo is required, and the `!cgo` stub that
used to prove the containment was retired along with the degraded build it enabled. See
`docs/architecture.md` for why.

## Why cgo is not a performance problem here

A multiplexer's hot path moves bytes and never inspects cells. libghostty is called once
per pty read, and the kernel caps a read from a pty master at 4KB, so the cost is roughly
one ~100ns cgo call per 4KB of output. Cell-level reads, where cgo overhead would
actually matter and why the C API offers batch getters, happen only on attach and when
dumping history.

## Verified before committing to Go

A throwaway cgo program confirmed the pieces cm depends on, in particular the one
non-obvious requirement: reproducing zmx's two-phase state restore needs
selection-restricted VT formatting, which must be reachable from C rather than only from
Zig. It is:

- `ghostty_terminal_new`, `ghostty_terminal_vt_write`, `ghostty_terminal_resize`
- `ghostty_terminal_grid_ref` with `GHOSTTY_POINT_TAG_SCREEN` and `_ACTIVE`, giving the
  endpoints a restore needs
- `GhosttyFormatterTerminalOptions.selection`, which does restrict output: formatting the
  same terminal produced 404 bytes unrestricted and 192 bytes for a
  screen-top-to-active-top selection

## Building

```
mise run libghostty
```

Three flags are load-bearing and not redundant. `-Demit-xcframework=false` is required
because the default is "xcodebuild is on PATH", which is true even with only the
Command Line Tools stub, and configuring the iOS target then fails without full Xcode.
`-Demit-macos-app=false` avoids building the GUI app that cm does not use.

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

Both build sites must agree: `mise.toml`'s `libghostty` task and `Dockerfile.test`. A timing
assumption that holds in one and not the other is how a test passes locally and fails in CI.

Two things guard this now, because a build flag is easy to drop and its symptom is easy to
misattribute. `internal/vt/scroll_test.go` asserts on the *ratio* between scrolling up and down
rather than an absolute duration, since a ratio is a property of the build and not of the
machine: about 4x correct, about 45x in Debug. And `cm doctor`'s `slow-emulator` check measures a
half-page scroll at startup, so an installation built wrong says so instead of feeling slow.

The build emits `libghostty-vt.a`, a versioned dylib, headers, and pkg-config files under
`zig-out`. Prefer the static archive so cm ships one binary with no runtime library path
to manage.

## Constraints to respect

Effects callbacks (title changed, pwd changed, write-to-pty, bell) fire **synchronously**
inside `vt_write` and must not re-enter `vt_write` on the same terminal. From Go that
means a callback should only enqueue and return, never call back into code that might
touch the same terminal.

`GHOSTTY_TERMINAL_OPT_WRITE_PTY` must be wired for correctness, not convenience: query
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

The value is process-wide, in a C buffer, and that is a constraint rather than a detail.
libghostty requires the reply to stay valid until its callback returns, so it cannot live in
Go memory, and there is one buffer: two terminals constructed with different values both
report the second. Verified. Hence `vt.SetXtversion` is a package function and `Callbacks`
carries only a bool, so the API cannot promise per-terminal identity it does not deliver.
Per-session identity would need a buffer keyed on the handle.

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

Kitty graphics pass through as APC bytes but the formatter does not re-emit them, so
images are absent after a reattach. zmx has the same limitation.
