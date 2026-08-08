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

## Commands

```
cm attach [session]      attach, creating the session if needed (ctrl-\ detaches)
cm list                  list sessions
cm info <session>        one session's details; --field for a single value
cm history <session>     print contents including scrollback; --format=plain|vt|html
cm send <session> <text> send input without attaching
cm get-env [session]     print env vars from the session's latest client
cm kill <session>...     terminate sessions
```

`attach` with no name asks the server to allocate one, and `--own` ends the session when the
client disconnects without detaching. Together those give a terminal emulator a session per
window that cleans itself up on close.

A server starts automatically when needed, so there is normally no reason to run `cm server`.

## Status

Working and usable: persistent sessions, attach/detach, screen and scrollback restore on
reattach, multiple clients per session, resize, session survival across a server restart or
crash, history, send-without-attach, and cwd/title tracking forwarded to clients.

Also tracks the terminal-related environment variables of whichever client attached most
recently, so a long-running shell can refresh values like kitty's `KITTY_LISTEN_ON` that
otherwise go stale when the terminal restarts. See `docs/config.md`.

Not done yet: persistence across reboot and JSON output. See `docs/` for design notes and
trade-offs:

- `docs/architecture.md` - why three layers, and what each owns
- `docs/restore.md` - how screen restore works and why each detail is there
- `docs/config.md` - the config file, and the session environment problem
- `docs/rpc.md` - why ttrpc, measured against gRPC and Connect
- `docs/libghostty.md` - using libghostty-vt from Go, and its constraints

## Building

Requires Go and, to build libghostty-vt from source, Zig 0.16.0. Both are pinned in
`mise.toml`.

```
mise install
mise run build
```
