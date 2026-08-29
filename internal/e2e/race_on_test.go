//go:build race

package e2e

// raceEnabled reports whether this test binary was built with -race.
//
// The point is the cm binary the e2e tests spawn, not this one. buildCM ran a plain `go build`, so every
// e2e test drove an uninstrumented cm: the client, server and shim wiring was the one part of the
// codebase the race detector never saw, while the unit tests that do run under it exercise types in
// isolation rather than the three processes talking to each other. Use-after-close between a detaching
// client and a live session is exactly the shape that only appears in the wiring.
//
// Keyed off the test binary's own build so `go test -race ./internal/e2e/` instruments both halves and a
// plain `go test` stays fast, rather than adding a flag someone has to remember.
const raceEnabled = true
