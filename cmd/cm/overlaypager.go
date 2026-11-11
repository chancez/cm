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

// defaultPager is the pager when neither CM_PAGER nor PAGER names one.
//
// -R because the scrollback carries its colors as SGR, which less otherwise shows as literal ESC[31m. +G
// starts at the end, which is where the session was: the reason to open scrollback is what scrolled off
// the top, and that is read upwards from the bottom.
const defaultPager = "less -R +G"

// pagerCommand is the shell command that shows the scrollback.
//
// CM_PAGER, then PAGER, then less, so cm can have a pager of its own without changing the one git and man
// use. kitty's scrollback_pager is the model: the user's choice of program, fed the text on stdin, which
// is how `nvim -` works as a scrollback viewer. Empty counts as unset, as it does for cm's other variables.
func pagerCommand(getenv func(string) string) string {
	for _, key := range []string{paths.Env("PAGER"), "PAGER"} {
		if v := getenv(key); v != "" {
			return v
		}
	}
	return defaultPager
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
func overlayPager() func(context.Context, []byte) error {
	return func(ctx context.Context, content []byte) error {
		// Not CommandContext, for the reason overlayPicker gives: a pager killed mid-frame leaves the
		// terminal in whatever mode it had set.
		_ = ctx
		child := exec.Command("/bin/sh", "-c", pagerCommand(os.Getenv))
		child.Stdin = bytes.NewReader(content)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		child.Env = pagerEnv(os.Environ())
		if err := child.Run(); err != nil {
			return fmt.Errorf("running the pager: %w", err)
		}
		return nil
	}
}
