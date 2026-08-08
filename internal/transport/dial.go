package transport

import (
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// DialServer connects to a cm server and returns a typed client.
//
// This is where a transport is chosen, and the only place above this package that has to know one exists.
// Callers get a Conn they can close and a generated client they can call, neither of which names a
// transport, which is what lets a second one be added without touching them.
//
// The generated client is transport-specific and cannot be otherwise: each protoc plugin emits its own
// client type for the same proto. Both satisfy the same generated interface, though, so a caller holding
// serverv1.ServerClient is already transport-agnostic.
func DialServer(socketPath string) (Conn, serverv1.ServerClient, error) {
	cl, err := DialTTRPC(socketPath)
	if err != nil {
		return nil, nil, err
	}
	return cl, serverv1.NewServerClient(cl), nil
}

// DialShim connects to a session's shim and returns a typed client.
//
// Always ttrpc, and deliberately not swappable even in a build carrying a second transport. The
// server-to-shim hop is a local unix socket by construction: a shim exists to hold one pty on this
// machine, so there is no remote case to serve. It is also the process that multiplies per session, which
// is exactly where the memory cost of linking a heavier transport would be felt.
func DialShim(socketPath string) (Conn, shimv1.ShimClient, error) {
	cl, err := DialTTRPC(socketPath)
	if err != nil {
		return nil, nil, err
	}
	return cl, shimv1.NewShimClient(cl), nil
}
