# Using libghostty-vt from Go

libghostty-vt provides terminal emulation: VT parsing, screen and scrollback state with
reflow, and formatting screen contents back out as plain text, VT sequences, or HTML. It
does not provide pty allocation, process spawning, or an event loop. Those are internal
to the Ghostty app and not exported, so cm implements them.

The API is explicitly unstable while the behavior is stable, and there is no tagged
release, so `mise.toml` pins a commit. Bumping `GHOSTTY_REF` is deliberate.

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

Two flags are load-bearing and not redundant. `-Demit-xcframework=false` is required
because the default is "xcodebuild is on PATH", which is true even with only the
Command Line Tools stub, and configuring the iOS target then fails without full Xcode.
`-Demit-macos-app=false` avoids building the GUI app that cm does not use.

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

Kitty graphics pass through as APC bytes but the formatter does not re-emit them, so
images are absent after a reattach. zmx has the same limitation.
