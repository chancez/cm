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

// sessionEnvFor returns the environment for a session this invocation creates, chosen by which machine the
// session will run on.
//
// The decision has to be here rather than in each command, because attach and run build their own Open and
// nothing makes the two agree, which is the same trap `--tag` fell into. It cost a real leak: `cm run
// --remote` sent this machine's whole environment to the far host while `cm attach --remote` sent sshd's
// posture. Found by reading the environment inside a session that `cm run --remote` had created, where the
// local sandbox's own CM_RUNTIME_DIR, CM_CONFIG and SSH_AUTH_SOCK were all present, so a remote session was
// pointed at directories and a socket that exist only here, and any credential exported in the calling shell
// crossed a network with it.
//
// Keyed on whether a remote was named rather than on a parsed target, since a malformed one has already been
// refused by checkRemote before any command's RunE runs.
//
// environ is passed in for the reason sessionEnvFrom is split out: a test asserting the whole value prints it
// on failure, and failure output goes to terminals, CI logs and bug reports.
func (g *globals) sessionEnvFor(environ, env []string) []string {
	if g.remote == "" {
		return sessionEnvFrom(environ, env)
	}
	return crossHostEnv(environ, env, clientHostname())
}

// crossHostEnv is what a session on another machine is born with: sshd's posture, the name of the machine
// watching, then explicit --env last so it wins.
//
// Shared by sessionEnvFor and applyRemote so there is one statement of the policy rather than two that drift.
func crossHostEnv(environ, env []string, clientHost string) []string {
	return append(sessionenv.CrossHost(environ, clientHost), env...)
}
