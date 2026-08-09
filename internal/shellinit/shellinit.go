// Package shellinit holds the shell integration cm asks a shell to load.
//
// The integration reports what OSC 133 cannot express. A shell's prompt markers already tell cm when a
// command starts and stops, which is where busy, idle, and the exit status come from, so none of that is
// duplicated here. What is missing is a program saying it is *blocked* -- waiting for input rather than
// working -- because a shell marks a command as running either way and nothing outside the program can
// tell the difference.
//
// Written straight to the pty rather than shelling out to `cm report`. Measured before choosing: any cm
// invocation costs about 23ms, and a prompt hook runs twice per command, so the command form would add
// roughly 46ms to every prompt. A printf into the terminal costs nothing and needs no server running,
// which also means it works while the server is restarting. `cm report` stays for programs that are not
// shells and for hooks in other languages.
package shellinit

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed scripts/*
var scripts embed.FS

// Shells lists the shells with an integration, in a stable order for help text and completion.
func Shells() []string {
	entries, err := scripts.ReadDir("scripts")
	if err != nil {
		// The scripts are embedded at build time, so this cannot fail in a built binary.
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".sh"))
	}
	sort.Strings(names)
	return names
}

// Script returns the integration for a shell.
func Script(shell string) (string, error) {
	data, err := scripts.ReadFile("scripts/" + shell + ".sh")
	if err != nil {
		return "", fmt.Errorf("no integration for %q, want one of %s",
			shell, strings.Join(Shells(), ", "))
	}
	return string(data), nil
}
