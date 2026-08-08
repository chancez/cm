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

Size matters more here than in a typical service. The binary re-execs itself as a shim
once per session, so every live session pays for it in resident memory.

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

## Conventions

Services are named `Shim` and `Server`, not `ShimService`. The ttrpc generator appends
its own suffix, so buf's `SERVICE_SUFFIX` rule would yield `ShimServiceService` and
`RegisterServerServiceService`. `buf.yaml` documents both lint exclusions.
