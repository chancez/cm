package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// resumeEnvVar carries a client's output position across the re-exec that upgrades it.
//
// The environment rather than the argv, because exec replaces the process image and the argv it was
// given is what the kernel reports from then on. A position is true for one instant, so it does not
// belong in a string that outlives the moment: `ps` showed it on every upgraded client, and anything
// recording a live command line records it too. kitty does that for a window it started with the shell,
// under save_as_session --use-foreground-process, so a window where `cm attach` was run by hand gets
// the position written into the saved session file and handed back at the next startup.
//
// Replaying one is wrong twice over. The position counts bytes in a stream the new process never saw,
// and the server reads a resume as "this client already has the screen on it", so it sends no snapshot:
// the window comes back blank. The recovery does not fire either, because it rides on the Gap flag of
// the next output chunk and a restored window is a shell sitting idle at a prompt. Nor is a stale
// position usually out of range enough to be flagged: the server retains 1 MiB per session, so one
// captured at the last upgrade is still inside the log unless the session has produced a megabyte
// since.
//
// A window kitty launched with a program of its own, which is how cm's terminal integration works,
// records that program instead and never saw this. The argv is still the wrong place for it.
//
// The pid stops the variable from becoming the same bug through a different door: exec preserves the
// pid, so only a process that re-exec'd itself can match, and a value left behind by a shell profile or
// a copied environment is ignored rather than making a fresh attach paint nothing.
const resumeEnvVar = "CM_RESUME_FROM_SEQ"

// resumeEnvEntry formats the handover for pid as a KEY=VALUE entry.
func resumeEnvEntry(pid int, from uint64) string {
	return fmt.Sprintf("%s=%d:%d", resumeEnvVar, pid, from)
}

// takeResumeFrom reports the position handed to this process, and clears the variable.
//
// Cleared whatever it held, and before the caller builds anything, because `cm attach` forwards its
// whole environment to a session it creates: left set, it would be exported in that session's shell
// and inherited by every cm command run inside it, which is the stale value the pid check exists to
// reject. sessionenv.NoInherit lists it as well, so the ordering here is not the only thing standing
// between a handover and a shell that keeps it forever.
func takeResumeFrom(pid int) *uint64 {
	spec, ok := os.LookupEnv(resumeEnvVar)
	if !ok {
		return nil
	}
	_ = os.Unsetenv(resumeEnvVar)
	return parseResumeFrom(spec, pid)
}

// parseResumeFrom reads a "<pid>:<seq>" handover, and reports nil for anything that did not come from
// this process re-execing itself.
func parseResumeFrom(spec string, pid int) *uint64 {
	owner, pos, ok := strings.Cut(spec, ":")
	if !ok || owner != strconv.Itoa(pid) {
		return nil
	}
	from, err := strconv.ParseUint(pos, 10, 64)
	if err != nil || from == 0 {
		// Zero is not a usable resume point: it asks for the whole retained log, which is the
		// opposite of resuming.
		return nil
	}
	return &from
}

// resumeEnviron returns env with the handover for pid set.
//
// Any entry already naming it is dropped rather than left in place. A client upgrading a second time
// has consumed its own handover by then, so this is defensive, but two entries for one name leave
// which of them wins up to whatever reads it.
func resumeEnviron(env []string, pid int, from uint64) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, resumeEnvVar+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, resumeEnvEntry(pid, from))
}
