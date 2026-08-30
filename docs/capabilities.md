# Capabilities

What one cm process can know about what another can do.

cm runs three roles that are routinely different builds. A shim holds a pty across every server
restart and upgrade, so a new server talking to a ten-day-old shim is the ordinary state, not a
broken one. Replacing the binary is enough to produce it: a shim is re-exec'd from disk, so
installing a build and carrying on working pairs an old server with new shims and no upgrade command
was involved.

`shim.proto`'s header already states the rules that follow: additive fields only, never renumber,
never make a new RPC load-bearing, treat absence as "the peer predates this". Those rules are correct
and they are not enough, because they leave every caller to work out for itself what a peer's silence
means.

## What is missing

Skew is observable today in exactly two ways, and neither answers the question a caller has.

A version string difference. `checkVersionSkew` and `checkShimVersionSkew` report that builds differ.
They cannot say *what* differs, which is the part that would shorten a debugging session.

An `Unimplemented` substring in an error. `isUnimplemented` in `cmd/cm/version.go` matches on the
message because ttrpc exports no sentinel, and it only works after the call has been made.

Three failure shapes fall out of that, and they need separating because only one of them a probe can
fix:

1. **An absent field reads as its zero value**, indistinguishable from a peer that said no.
   `ResizeRequest.force_signal`, `ShutdownRequest.signal`, `ShutdownResponse.surviving_pids` and
   `Open.outstanding_queries` each carry a proto comment saying exactly this. The consequences are
   already written down: an older shim ignores a requested signal and sends SIGHUP instead, and empty
   `surviving_pids` means both "nothing leaked" and "this shim cannot tell you".

2. **An absent RPC is detectable only by calling it.** Acceptable where the call is cheap and the
   caller is a diagnostic, which is why `cm version` and `cm status` get away with it.

3. **Absent behavior is not detectable at all.** `checks.go` records the case: `cm wait --until
   blocked` against a server that predates state reporting waits forever, because nothing on the wire
   separates "not blocked yet" from "will never report blocked". This is the class that costs real
   time, and the only one a capability probe fixes.

The value here is forward-looking, which is worth being plain about. cm has one user, so there is no
old client to serve and nothing to retrofit. A token declared today is what lets a build from next
month know that a shim from today can do a thing. That is the whole return, and it only exists if the
foundation lands before the features that would use it.

## Reported tokens, not a version comparison

A peer reports a set of capability tokens. A caller checks for the one it needs.

The obvious cheaper design is a monotonic API revision integer per service: `shim_api_revision = 7`,
gates read `rev >= 5`, one field and no bookkeeping. Rejected for a specific reason rather than on
taste: **two cm builds with the same version string already differ in what they can do.**
`vt.Available` is a cgo build property, and a server without the emulator cannot restore a screen or
serve history. The transport choice is about to be the second such case, since linking gRPC is a
build-time decision made to keep its cost off every shim. A revision integer cannot express either,
and it mis-answers a downgrade, which cm supports on purpose: `store` keeps a pre-migration snapshot
for exactly that.

So capability has to be reported rather than inferred, and once it is reported the ordinal is
redundant.

## Where a set is carried

At each hop, in the message that is already the handshake. No new RPC where one can be avoided,
because a new RPC is itself a thing an old peer does not implement.

**Server to shim: `StateResponse`.** The server already calls `State` on every shim at startup and for
health, and already caches `version` from it. Adding a field there costs nothing and is available
before the server acts on any session.

**Shim to server: nothing.** The shim is passive. It answers, it does not ask, so it has no use for
the server's capabilities. Stated so it does not get built speculatively; add it when there is a
caller.

**Server to client: `StatusResponse`, and `Opened` on the attach stream.** `Opened` matters because an
attach is long-lived and cannot afford a second round trip to discover what it is talking to.
`StatusResponse` covers the short-lived commands. A command in class 3 above pays one unary call
before committing, which is 17.8us against the ~23ms a cm invocation costs anyway.

**Client to server: the existing `client_version` pattern extended.** `Open` and `DoctorRequest`
already carry the client's build; a client's capabilities belong beside it. The server uses this only
to report, never to decide, matching what those fields already say about being advisory.

## One registry

`internal/capability` declares every token once, as a Go constant with a doc comment naming what
degrades without it and the incident behind it, same convention and same bar as a `cm doctor` check: a
token corresponds to something with a real consequence, not to every additive field in history.

One `Set` per role, as a literal, because cm is a single binary: a role's capabilities are a property of
the build rather than something to discover at runtime. `Shim()` is the only one so far; `Server()` and
`Client()` arrive with the hop that needs them.

The package is vocabulary only. No wire types, no transport, and no policy about what to do when
something is missing, which is why every role can import it and why a token can be checked in a test
without standing up a peer.

`Set` has no `Has`, deliberately. A bool return is the mistake the type exists to prevent; see "Three
answers, not two" below. `Missing(want ...)` is for a diagnostic naming what cannot be relied on, and it
counts `Unknown` as missing, because an unreportable capability cannot be relied on either.
`Unrecognized` returns the tokens a peer reports that this build does not declare, which is evidence the
*peer* is newer. Nothing else in cm can tell which side of a version difference is ahead, and the
reader's next move depends on it: a peer that is behind gets restarted, and a peer that is ahead means
this binary is the stale one. That is why `Parse` keeps unknown tokens instead of dropping them.

### The rules

- **A token is added by the commit that adds the behavior.** Out of order, the set claims a capability
  the build does not have, which is worse than the silence it replaces: a missing field fails at the
  field, where a lying token makes a caller commit to a path that cannot work.
- **A token is never removed or renamed.** Same rule as a proto field, for the same reason.
- **A token gates a decision, never a report.** Reports already degrade correctly by reading a zero
  value. Gating a report on a token adds a second way to be wrong.

### The guard

A self-reported set is only as good as the habit behind it, and the failure mode is a registry that
accumulates tokens nothing consults: a peer then advertises something no caller checks, which is all of
the bookkeeping and none of the benefit, and it reads as coverage.

So the rule is a test rather than a paragraph. `TestEveryDeclaredCapabilityIsUsedSomewhere` parses the
repo and fails on a token nothing outside the package asks about, and a second check fails on a token
that belongs to no role's set, which is the same dead weight from the other direction. Every gate names
a declared constant, which the compiler enforces for free.

Three mutations were run against the tests before believing them: collapsing `Unknown` into `Absent`
(caught by three tests), removing the gate from `Session.Shutdown` (caught by five), and deleting the
`Capabilities` line from `Service.State`. The third initially passed the *whole suite*, because every
server-side test drives a fake shim and none of them can see the real one stop reporting. That gap is
what `internal/shim`'s own capability test covers, and it is worth knowing about: a foundation that
reports nothing while looking wired up is the failure this mechanism is most exposed to.

## Three answers, not two

The type a caller gets back is `capability.Support`, with `Present`, `Absent` and `Unknown`. That third
value is the design, not defensiveness.

A bool would have to fold "this peer says no" together with "this peer told me nothing", and those call
for opposite behavior: the first is a fact to act on, the second is a reason to do what cm always did.
Reading them the same way is not a hypothetical mistake, it is the *default* one, because an older peer
sends an empty list and so does a peer that supports none of them.

cm has been here before with sockets, where `ECONNREFUSED` means both "nobody is listening" and "a live
listener's queue is full", and only `ENOENT` is conclusive. Same shape and same rule: one of the answers
is the absence of information rather than a value.

What makes the distinction possible is the `capabilities` token itself, which every role declares. A
peer that speaks this mechanism always sends at least one token, so an empty list is conclusive: the
peer predates the mechanism. Without that token, `Supports` could only ever answer `Unknown`.

The zero `Set` answers `Unknown` to everything, so code that forgets to populate one degrades to cm's
previous behavior rather than to a confident wrong answer.

### Which answers are reachable, and when

Worth stating because it decides what a first gate can usefully do. On the day this lands, `Absent` is
unreachable on the shim hop: any shim reporting capabilities is a build that has every capability
declared so far, so a shim either reports the token or reports nothing at all.

That is not an argument against the mechanism, it is the shape of its payoff. `Unknown` versus `Present`
is reachable today and useful today, and it is what the first gate acts on. `Absent` becomes reachable
the first time a capability is added *after* a shim build exists without it, which is every capability
from here on.

So a gate is written for all three from the start, and the `Unknown` branch stops firing on its own as
sessions turn over onto builds that report.

## The seed set

Small on purpose. A registry with nothing load-bearing in it is a registry nobody maintains, and one
token per historical field is the noise this is trying to avoid.

- **`capabilities`**. The peer reports capabilities at all. Declared by every role, consulted only by
  `Set.Reports`, and the reason an empty list means something.
- **`shutdown.signal`** (shim). `ShutdownRequest.signal` is honored rather than falling back to what
  `force` selects. A shim without it sends SIGHUP, or SIGKILL when forced, whatever `cm kill --signal`
  asked for.

`shutdown.signal` is seeded because it fixes something rather than demonstrating something.
`shim.proto` claimed for a long time that the server "logs it rather than pretending the signal was
delivered", and no such log existed: `cm kill --signal TERM` against an older shim sent SIGHUP and
reported success. That matters because the reason to name a signal is that the job traps it, so
something catching SIGTERM to finish writing a file got hung up on and nothing recorded the swap. A
compat rule stated in prose and checked nowhere is a rule the next reader believes and the code does not
follow, which is the argument for this whole mechanism in one field.

`terminal` was considered and dropped. It is already `StatusResponse.terminal` and `cm version`'s
`terminal` line, and `vt.Available` is now a constant true since cgo is required, so a token for it
would be a token that is always present: bookkeeping with no consequence. The build-conditional
*argument* still stands, because that is what rules out a revision integer, and the transport choice is
the case that will need it.

## Staging

Three commits. The first is done.

1. **The vocabulary and the shim hop**: `internal/capability`, `StateResponse.capabilities`, the server
   caching it per session, and the `shutdown.signal` gate. The shim hop first because it is where skew is
   worst and where a handshake already exists: the server already calls `State` and already caches
   `version` from it.

   This was planned as two commits, the package alone and then the wiring, and it cannot be: the
   discipline test fails on a token nothing consults, so a commit holding the registry without a gate
   does not pass its own tests. That is the rule working rather than an obstacle, and it is worth knowing
   before planning the next capability: **a token and its gate are one commit**, because a registry
   entry with nothing behind it is exactly what the test refuses to let sit in the tree.
2. **The client hop**: `StatusResponse.capabilities` and `Opened.capabilities`, and the two `wait` gates
   refusing up front with a message naming the restart instead of hanging. `wait.reported-state` covers
   the hang `checks.go` documents; `wait.match` is the same hang one field over.
3. **Diagnostics**: `cm doctor` and `cm version --json` naming which capabilities a peer lacks rather
   than only that versions differ, and using `Set.Unrecognized` to say which side is ahead.

## Not doing

**Retrofitting tokens for historical additive fields.** A dozen tokens for a dozen past fields is
noise, and noise teaches people to ignore the mechanism. The fields already degrade the way their
comments describe.

**Negotiation.** Nothing here agrees on a shared subset or downgrades a protocol. Each side reports,
the caller decides. The asymmetry is correct: only the peer that knows a capability exists can act on
its absence, so this always helps the newer side and never the older one.
