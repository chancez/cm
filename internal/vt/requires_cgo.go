//go:build !cgo

// This file exists only to explain a failed build.
//
// Without it, `CGO_ENABLED=0 go build ./...` reports "build constraints exclude all Go files in
// internal/vt", which names the symptom rather than the cause: nothing constrains the real file, it is
// excluded because it uses cgo. That message sends people looking for a build tag that does not exist.
//
// cgo is required because the emulator is not optional. `cm read`, `cm history`, and screen restore on
// reattach all depend on it, and they are most of what cm does. There used to be a stub here that let a
// no-cgo build succeed, which produced something worse than a failure: those commands returned empty
// *successfully*, and that silence cost real debugging time twice.
package vt

// The undefined identifier is the message. Referencing it fails the build with its own name in the error,
// which is the shortest way to get an explanation in front of whoever ran the command.
var _ = cm_requires_cgo_build_with_CGO_ENABLED_1
