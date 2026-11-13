//go:build !darwin

package main

import "github.com/chancez/cm/internal/paths"

// startServer starts a server as a detached child of this process.
//
// darwin goes through launchd instead, for a reason that has no equivalent here: see serverstart_darwin.go.
func startServer(exe string, dirs paths.Dirs) (func(), error) {
	return func() {}, spawnServer(exe, dirs, nil)
}
