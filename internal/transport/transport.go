// Package transport is the seam between cm's RPC logic and the wire protocol carrying it.
//
// Everything above this package works with generated service types and typed Send/Recv on streams.
// Nothing above it names a transport. That is what makes a second one possible: the ~7000 lines of
// session, wait, and fanout logic do not change when the bytes travel differently.
//
// The interfaces are over *construction* rather than over calls, which is forced by how the generated
// code works. protoc-gen-go-ttrpc and protoc-gen-go-grpc emit different, incompatible service
// interfaces for the same proto, each embedding its own stream type. No single interface can describe
// both. What they do share is the shape of the surrounding plumbing: build a server, register a
// service on it, serve a listener, shut down; dial an address, get a client, close it. That is what is
// abstracted here.
//
// Why bother, given ttrpc is the right default: two reasons that are not "flexibility for its own
// sake". A benchmark can measure the same operations across implementations, which turns a design
// argument into numbers. And remote access wants a transport with TLS and auth already solved, where
// ttrpc assumes a local trusted socket.
package transport

import (
	"context"
	"net"
)

// Server accepts RPC calls on a listener.
//
// Registration is deliberately not part of this interface. A service is registered with transport
// specific generated code, so each implementation exposes its own typed registration function and this
// interface covers only the lifecycle they share.
type Server interface {
	// Serve handles calls until the listener closes or ctx is cancelled.
	//
	// Returns nil for an ordinary shutdown rather than a sentinel, so callers do not have to know
	// which sentinel a given transport uses. Each implementation maps its own.
	Serve(ctx context.Context, l net.Listener) error

	// Shutdown stops accepting and waits for in-flight calls, bounded by ctx.
	Shutdown(ctx context.Context) error
}

// Conn is a client's connection to a server.
//
// Narrow on purpose. Callers need to close it and nothing else: the typed client that rides on it comes
// from generated code, so it is returned alongside rather than through this interface.
type Conn interface {
	Close() error
}

// Name identifies a transport, for logging and for selecting one.
type Name string

const (
	// TTRPC is the default: ttrpc over a unix socket.
	//
	// Chosen because it targets exactly cm's shape, a daemon talking to per-workload processes over a
	// local socket, and because it is markedly smaller. See docs/rpc.md.
	TTRPC Name = "ttrpc"
	// GRPC is gRPC over HTTP/2, available only in a build that includes it.
	//
	// Not the default, and deliberately not linked unless asked for: an idle grpc.NewServer() measures
	// about 13 MB resident, and cm re-execs itself once per session, so every live session would pay
	// part of that for a transport only a remote client uses.
	GRPC Name = "grpc"
)
