package main

import (
	"os"

	"github.com/chancez/cm/internal/sessionenv"
)

// sessionEnv returns the environment for a session this client creates: this process's own
// environment, then the caller's --env entries.
//
// Forwarding this process's environment is what makes a session resemble the thing that created it.
// A session created by hand from a shell gets that shell's environment, like a subshell would, and a
// session created by a terminal emulator's integration gets the emulator's, which is close to fresh
// because such a client has no shell between it and launchd. Both fall out of forwarding rather than
// needing a mode flag: the client's own ancestry is the signal, and it is already correct.
//
// Order is relied on rather than assumed. Go's exec dedups the environment it passes, keeping the
// last occurrence of a name, so --env last means an explicit `--env PATH=...` beats the forwarded
// value instead of being silently dropped behind it. Verified both ways, since the failure would be
// quiet: with the order reversed the flag appears to work while doing nothing.
//
// Shared by attach and run because they build separate Open messages, and nothing makes the two
// agree. That is not hypothetical here: --tag worked everywhere except --no-attach for exactly this
// reason, accepted and validated and then dropped.
func sessionEnv(env []string) []string {
	return sessionEnvFrom(os.Environ(), env)
}

// sessionEnvFrom is sessionEnv with the process environment passed in.
//
// Split out so a test can supply a small environment instead of the real one. That is not only for
// convenience: a test asserting on the whole result prints it on failure, and with os.Environ()
// inlined here a failure dumped the developer's actual environment, API tokens included, into the
// test output. Failure output goes to terminals, CI logs, and bug reports.
func sessionEnvFrom(environ, env []string) []string {
	return append(sessionenv.Inherit(environ), env...)
}
