package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/chancez/cm/internal/paths"
)

// defaultPager is the pager when nothing else names one.
//
// -R because the scrollback carries its colors as SGR, which less otherwise shows as literal ESC[31m. +G
// starts at the end, which is where the session was: the reason to open scrollback is what scrolled off
// the top, and that is read upwards from the bottom.
const defaultPager = "less -R +G"

// pagerCommand is the shell command that shows the scrollback, and where it came from.
//
// CM_PAGER, then [scrollback] pager from the config file, then PAGER, then less. cm's own two come before
// PAGER so cm can have a pager without changing the one git and man use, and the variable before the file
// as everywhere else in cm. kitty's scrollback_pager is the model: the user's choice of program, fed the
// text on stdin, which is how `nvim -` works as a scrollback viewer. Empty counts as unset, as it does for
// cm's other variables.
//
// The source is returned for `cm config`, since with four places to set it, "my setting does nothing" is
// a question about which one won.
func pagerCommand(getenv func(string) string, configured string) (command, source string) {
	if v := getenv(paths.Env("PAGER")); v != "" {
		return v, "$" + paths.Env("PAGER")
	}
	if configured != "" {
		return configured, "config file"
	}
	if v := getenv("PAGER"); v != "" {
		return v, "$PAGER"
	}
	return defaultPager, "default"
}

// pagerEnv is the environment the pager runs in.
//
// LESS=R when LESS is unset, which is what git does for the same reason: PAGER=less is common, and a plain
// less shows the scrollback's colors as escape codes. A LESS the user set is theirs and is left alone.
func pagerEnv(environ []string) []string {
	for _, kv := range environ {
		if strings.HasPrefix(kv, "LESS=") {
			return environ
		}
	}
	return append(environ, "LESS=R")
}

// overlayPager returns the function the overlay uses to show the scrollback in a pager.
//
// Through `sh -c`, as git runs PAGER, so the value can carry arguments and quoting: `nvim -c 'set ft=sh' -`
// is one command line, not one word. The text goes on stdin and the pager reads keys from the terminal,
// which this process has stopped reading for as long as the pager runs.
func overlayPager(configured string) func(context.Context, []byte) error {
	return func(ctx context.Context, content []byte) error {
		// Not CommandContext, for the reason overlayPicker gives: a pager killed mid-frame leaves the
		// terminal in whatever mode it had set.
		_ = ctx
		command, _ := pagerCommand(os.Getenv, configured)
		child := exec.Command("/bin/sh", "-c", command)
		child.Stdin = bytes.NewReader(content)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		child.Env = pagerEnv(os.Environ())
		if err := child.Run(); err != nil {
			return fmt.Errorf("running the pager: %w", err)
		}
		return nil
	}
}
