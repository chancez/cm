# cm

A terminal multiplexer that persists shell sessions, built on
[libghostty-vt](https://github.com/ghostty-org/ghostty) for terminal emulation.

Sessions survive detaching, the client exiting, and the server being upgraded or
restarted. It provides no windows, tabs, or splits: your terminal emulator already does
that better.

## Architecture

```
client  <--ttrpc-->  server  <--ttrpc-->  shim  <-->  pty  <-->  shell
 (tty)              (VT state,          (one per session,
                     scrollback)         holds the pty)
```

The shim exists so the server can be replaced without disturbing a running shell. It
owns the pty and an append-only sequenced log of output, and nothing else: no terminal
emulation, no session policy. Upgrading the binary does not upgrade running shims, which
is the point, so the shim protocol stays small and additive-only.

The server is the single entry point. It holds terminal state via libghostty-vt, so
reattaching restores your screen and scrollback, and multiple clients can share one
session.

Because the shim buffers output while the server is away, restarting the server is a
brief freeze rather than a lost session. Clients resume from their last sequence number
without redrawing.

## Status

Early. See `docs/` for design notes.

## Building

Requires Go and, to build libghostty-vt from source, Zig 0.16.0. Both are pinned in
`mise.toml`.

```
mise install
mise run build
```
