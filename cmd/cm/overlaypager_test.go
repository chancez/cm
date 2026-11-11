package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// CM_PAGER, then PAGER, then less, and an empty value is unset, as it is for cm's other variables.
func TestPagerCommand(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "neither set", want: defaultPager},
		{name: "PAGER", env: map[string]string{"PAGER": "most"}, want: "most"},
		{name: "CM_PAGER wins", env: map[string]string{"CM_PAGER": "nvim -", "PAGER": "most"}, want: "nvim -"},
		{name: "empty CM_PAGER is unset", env: map[string]string{"CM_PAGER": "", "PAGER": "most"}, want: "most"},
		{name: "empty PAGER is unset", env: map[string]string{"PAGER": ""}, want: defaultPager},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			if got := pagerCommand(getenv); got != tc.want {
				t.Errorf("pagerCommand() = %q, want %q", got, tc.want)
			}
		})
	}
}

// LESS=R only when the user has no LESS of their own.
func TestPagerEnv(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		want    []string
	}{
		{name: "unset", environ: []string{"HOME=/h"}, want: []string{"HOME=/h", "LESS=R"}},
		{name: "set", environ: []string{"LESS=FX"}, want: []string{"LESS=FX"}},
		{name: "set empty", environ: []string{"LESS="}, want: []string{"LESS="}},
		{name: "a longer name is not LESS", environ: []string{"LESSOPEN=x"}, want: []string{"LESSOPEN=x", "LESS=R"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pagerEnv(slices.Clone(tc.environ)); !slices.Equal(got, tc.want) {
				t.Errorf("pagerEnv(%q) = %q, want %q", tc.environ, got, tc.want)
			}
		})
	}
}

// The scrollback reaches the pager on stdin, and the command runs through a shell so it can carry
// arguments and redirections.
func TestOverlayPagerFeedsStdin(t *testing.T) {
	out := filepath.Join(t.TempDir(), "paged")
	t.Setenv("CM_PAGER", "cat > '"+out+"'")

	if err := overlayPager()(context.Background(), []byte("line one\nline two\n")); err != nil {
		t.Fatalf("overlayPager() error = %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "line one\nline two\n" {
		t.Errorf("pager read %q, want the content", got)
	}
}

// A pager that fails is an error, so the client can log it rather than repaint over the reason.
func TestOverlayPagerReportsFailure(t *testing.T) {
	t.Setenv("CM_PAGER", "exit 3")
	if err := overlayPager()(context.Background(), nil); err == nil {
		t.Error("overlayPager() error = nil, want the pager's exit status")
	}
}
