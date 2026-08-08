# Concurrency

cm's hard parts are not algorithms, they are lifetimes. A pty, a log, and a client connection all
end independently and at moments none of them controls, so most of the real bugs have been "this
was still being used when that went away" rather than wrong logic.

This file records the invariants that are easy to break and were not obvious, including the ones
that were learned by breaking them.

## Detach races the read it is interrupting

A client detaching closes its `seqlog.Reader` while the goroutine streaming output to that client is
blocked in `Reader.Next`. This is not an edge case: it is what happens on *every* detach, because
those are two different goroutines by design. The reading goroutine has to be separate so the
service can also notice a detach or a dropped connection.

So `Reader.Close` and `Reader.Next` are concurrent by construction, and both bugs found here came
from treating them as if they were not.

`sub` is guarded by the log's mutex, not merely set once at construction. Checking it outside the
lock and clearing it afterwards, which is the natural way to write `Close`, leaves a window where
`Next` sees a non-nil subscriber and then dereferences nil. That panic is on a goroutine nothing
recovers, so it takes the whole server down with it.

`Close` also has to wake the subscriber *before* unregistering it. Removing a subscriber that `Next`
is already waiting on means no future append or log close will ever signal it, so `Next` blocks
forever and the goroutine leaks, holding its session's log alive. This one hid behind the first: it
only became reachable once the crash was fixed.

## Not every file operation is safe against Close

`os.File` refcounts its descriptor internally, so `Read` and `Write` are safe against a concurrent
`Close`. It is tempting to generalize that to "the pty is safe", and it is not.

`pty.Setsize` and `pty.GetsizeFull` go through `os.File.Fd()`, which hands out the raw descriptor
with no synchronization at all. Once the pty is closed that number is stale, and the kernel is free
to give the same number to the next file anything opens, so the ioctl lands on an unrelated file.

The shim answers `Size` and `Resize` on RPC goroutines while the shell can exit at any moment, so
this is a live path rather than a theoretical one. Those three call sites go through `withPty`,
which holds a read lock and reports `ErrClosed` once the pty is gone.

The lock is deliberately not the session's existing mutex, and deliberately an `RWMutex`:

- It must never be held across a blocking pty read. Closing the fd is how `Shutdown` unblocks the
  pump, and that has to keep working.
- Several ioctls at once are harmless; a close has to exclude all of them.

## A session can end at any point during an attach

Opening a session and attaching to it are not atomic, so a command short enough to finish in
between will do exactly that. `cm run -- false` hit this reliably enough to matter: roughly one run
in twenty-five on macOS, and about one in ten on Linux for the second window below.

There are two windows, and both report the exit status rather than failing:

- `attach` refusing a session that has already ended. `Opened` is sent first and then `Exited`, so
  the stream looks like every other attach and clients need no special case for it.
- The resize after a successful attach failing against a shim that is gone.

The second is tolerated *only* when the session is known to have ended. A sizing failure on a live
session is a real problem worth reporting, because the client would otherwise be looking at a shell
wrapped for someone else's width. A fix that ignored every sizing error would look correct and be
wrong, which is why the test asserts both directions.

An error is the wrong answer here in general: the session genuinely ran and genuinely exited, so
reporting "session has ended" as a failure discards the exit code and turns a successful short
command into an RPC error. A caller cannot tell that from a real cm failure.

## Testing this

The races above are narrow enough that a single pass through the code usually succeeds even when
the bug is present, so a test that runs the operation once proves nothing.

What has actually worked:

- **Repeat the window.** `TestCloseWhileBlockedInNext` and `TestPtyIoctlsRaceWithClose` loop, since
  one iteration passes with the bug present.
- **Run under `-race`.** Both fd bugs were found by the detector, not by a failure. The pty one
  produced no symptom at all in normal runs.
- **Construct the state instead of racing into it.** For the exit-status races, ending the session
  first and then attaching reaches the same state deterministically. Where a window has no seam to
  hook from outside, a fake shim whose `Resize` ends the session gets inside it.
- **Verify the test against the old code.** Every fix here has a test confirmed to fail before it
  and pass after. This caught one test that passed with the fix reverted, because setting a flag too
  early routed it through a different code path than the one it claimed to cover.
- **Check the fix is not too broad.** Reverting to "always fail" and "never fail" and confirming the
  test rejects both is what distinguishes a real assertion from one that merely passes.

## Where a delay is a diagnostic

Adding a `time.Sleep` inside a suspected window is a cheap way to turn an intermittent failure into
a deterministic one, and to confirm a mechanism rather than guess at it. A 50ms delay before the
output goroutine started took a one-in-many follower failure to a 100% failure, which both proved
the cause and, when the same probe stopped reproducing it, showed the fix was real.

Twice that probe also surfaced a *different* bug than the one being chased, because widening one
window widens everything downstream of it. Worth re-reading the actual failure rather than assuming
the probe reproduced what you expected.
