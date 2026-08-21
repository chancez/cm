# Why ttrpc

cm uses [ttrpc](https://github.com/containerd/ttrpc) with protobuf, generated via
`buf`. The `.proto` files are the contract, so this is reversible: moving to gRPC or
Connect later is a codegen change plus adapting call sites, not a redesign.

## Measured alternatives

A program linking a client and server for both cm services, built on darwin/arm64,
against a 2.5 MB do-nothing baseline:

| Stack | Binary | Over baseline |
| --- | --- | --- |
| ttrpc | 7.2 MB | +4.7 MB |
| Connect | 13.4 MB | +10.9 MB |
| gRPC | 14.8 MB | +12.3 MB |

Size matters more here than in a typical service, though less than the wording above once implied. The
binary re-execs itself as a shim once per session, so a larger binary does cost resident memory per
session, but not one-for-one: text pages are shared and demand-paged.

Measured rather than assumed. Two otherwise identical Go programs, one padded to be 34 MB larger, differed
by **8.6 MB RSS** per process, roughly a quarter of the size difference. An idle `grpc.NewServer()` and
nothing else measures 14.0 MB on disk and 13.1 MB resident. cm today is 24.6 MB on disk with shims at about
6.4 MB resident each.

So the size argument holds in direction and is weaker in magnitude than "every session pays for the whole
binary". At around 20 sessions, linking gRPC unconditionally would cost roughly 70-90 MB resident for a
transport only a remote client would use, which is the reason it is a build-time choice rather than a
runtime flag.

## Reasoning

ttrpc targets exactly this shape: local, trusted, unix-socket RPC between a daemon and
per-workload processes. Running gRPC over a unix socket means paying for HTTP/2 flow
control and windowing on a same-machine byte pipe. Connect is further still from the
use case, since its strengths, browser clients, `curl`-ability, and REST-ish semantics,
are irrelevant to a terminal on a unix socket.

The precedent is direct rather than analogous: containerd uses ttrpc for a daemon
talking to per-workload shims over unix sockets, where the shims outlive the daemon.
That is cm's problem statement with different nouns.

One claim that does **not** hold up, and is worth recording so it is not repeated: ttrpc
does not avoid gRPC as a dependency. It imports `google.golang.org/grpc/status` for
error codes, so gRPC is in the module graph either way. The size win is real; the
dependency-isolation win is not.

## Alternatives ruled out for a specific reason

[grpchan](https://github.com/fullstorydev/grpchan) looks attractive at first: keep gRPC's
generated code and semantics but swap HTTP/2 for a lighter transport. It offers two, and
neither works here. `inprocgrpc` is for a client and server in the same process. Its
HTTP/1.1 transport, per its own README, "supports all stream kinds other than
full-duplex bidi streams".

`Attach` is a full-duplex bidi stream and cannot be anything else: keystrokes flow up
while output flows down, continuously and independently. A half-duplex approximation
would mean not being able to type while output is streaming.

The goal behind considering it, gRPC-like semantics without HTTP/2, is what ttrpc already
provides. It reaches that by not being gRPC rather than by replacing gRPC's transport.

## Costs accepted

- Small community and thin documentation. It is, in practice, the containerd RPC
  library.
- No HTTP/2, so no `grpcurl`, no server reflection, and no browser or `curl` access.
  Debugging the wire means writing Go.
- Remote access would work, since `Serve` takes any `net.Listener`, but authentication
  would be ours to build rather than inherited from gRPC's credential ecosystem. cm
  treats SSH as the remote story, so this is the decision most likely to be revisited
  if that changes.

## Verified before adopting

A throwaway program exercised the generated stubs for both services over a real unix
socket: a unary call, a server-streaming `Subscribe` starting from a non-zero sequence,
and a bidirectional `Attach` with interleaved keystroke round-trips. It also confirmed
that `Open.resume_from_seq` distinguishes absent from present, which the resume path
depends on to tell a fresh attach from a reconnect.

## A handler must not trigger its own server's shutdown

ttrpc decides whether a connection is idle by a state it recomputes only when the connection's write
loop next wakes from its `select`. While a connection's only request is still inside its handler, the
recorded state is therefore still *idle*, even though a call is plainly in flight. `Server.Shutdown`
closes idle connections, so a handler that starts its own server's shutdown races its own reply, and
the caller sees `ttrpc: closed` for work that completed.

The shim's `Shutdown` did exactly that: it closed the channel `Serve` waits on, and `Serve` went
straight to `srv.Shutdown`. Measured by widening the gap with a 50ms sleep between closing the channel
and returning, which took a `shutdownShim` call from 0 failures in 30 to 30 in 30. The shim was
confirmed gone every time the error came back, which is what made it damaging: the shell is signalled
before the reply, so the work happens and the report says it did not. `cm doctor --repair` took the
error as failure and printed "did 0 things" after reaping an orphan.

The fix is to signal the exit on a short timer rather than inline, so the reply is on the wire first.
50ms covers one local socket write and is generous rather than tuned: nothing waits on it, since the
process is already leaving and its shell is already signalled, so erring long is free while erring
short brings the lost reply back.

Callers still tolerate the error rather than relying on that alone, because a shim outlives the server
that spawned it and an upgraded server still talks to shims from the old build. `isTransportClosed`
marks those three places, in `cm kill` and in `cm doctor --repair`. The reasoning that makes it safe is
narrow and worth restating wherever it is used: the transport carries the *reply*, not the shutdown, so
by the time a reply can be lost the shell has already been signalled.

## Conventions

Services are named `Shim` and `Server`, not `ShimService`. The ttrpc generator appends
its own suffix, so buf's `SERVICE_SUFFIX` rule would yield `ShimServiceService` and
`RegisterServerServiceService`. `buf.yaml` documents both lint exclusions.

## The transport seam

`internal/transport` sits between cm's RPC logic and the wire protocol. Nothing above it names a
transport: every `ttrpc.` reference outside that package is gone, down from 14 across five files.

The seam is over construction rather than over calls, which the generated code forces.
`protoc-gen-go-ttrpc` and `protoc-gen-go-grpc` emit different, incompatible service interfaces for the same
proto, each embedding its own stream type, so no single interface can describe both. What they share is the
plumbing around them: build a server, register, serve, shut down; dial, get a typed client, close.

This was only cheap because of something worth recording: cm's handlers use typed `Send`/`Recv`
exclusively and never the transport's `SendMsg`/`RecvMsg`. Those appear in one test fake and nowhere else,
so the stream types were already nearly portable.

`DialShim` is fixed to ttrpc and deliberately not swappable. The server-to-shim hop is a local unix socket
by construction, so there is no remote case to serve, and the shim is the process that multiplies per
session, which is where a heavier transport's memory cost would land.

### Why there is no gRPC implementation yet

Attempted, and stopped at a real obstacle rather than a matter of effort. `protoc-gen-go-grpc` generates
stubs that assume the message types live in the same Go package. Generating both stub sets into one package
is a redeclaration error, since both emit `ShimClient`, `NewShimClient`, and a `Subscribe` stream type;
remapping the gRPC package means the stubs no longer see the messages. The plugin has exactly one option,
`require_unimplemented_servers`, so there is no supported way to split stubs from messages.

The ways through all have real cost. Generating the messages twice duplicates about 3,500 lines, and worse,
`Service.Attach` takes `*serverv1.AttachRequest` while a gRPC handler would take a different Go type for
the same wire message, so every handler would need conversion or generics -- exactly the code the seam
exists to leave alone. A separate `.proto` per transport means duplicate protos or a build step rewriting
`go_package`. Patching the generated file to import the messages is the smallest change and is a codegen
hack that breaks on plugin updates.

None of that is worth paying before there is a concrete need. `cm attach` over SSH already covers remote
use without a second transport, which was part of the original reasoning.

### Benchmarks

`internal/transport/bench_test.go` measures the transport in isolation, serving a do-nothing service so the
numbers describe the wire rather than a pty, an emulator, and sqlite. On an M3 Max:

| | ns/op | allocs/op | notes |
|---|---|---|---|
| raw unix socket round trip | 2,400 | 0 | control, no RPC |
| unary round trip | 17,800 | 34 | what `list` and `report` cost |
| unary with 4 KiB payload | 20,900 | 34 | 196 MB/s |
| stream, 256 x 4 KiB | 1,295,000 | 1,588 | 810 MB/s |
| open a stream | 18,700 | 36 | paid per attach |

The raw socket row is the point of including a control: it puts the RPC cost in perspective, showing ttrpc
adds about 14.5us over the socket itself rather than leaving 17.8us as a number with no scale.

4 KiB is one pty read, taken from `ptyReadSize` rather than picked, so the throughput figures describe
messages cm actually sends.
