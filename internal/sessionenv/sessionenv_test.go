package sessionenv

import (
	"reflect"
	"testing"
)

func TestMatcher(t *testing.T) {
	m := NewMatcher([]string{"TERM", "KITTY_*", "SSH_AUTH_SOCK"})

	tests := []struct {
		name string
		want bool
	}{
		{"TERM", true},
		{"KITTY_LISTEN_ON", true},
		{"KITTY_PID", true},
		{"KITTY_", true},
		{"SSH_AUTH_SOCK", true},

		{"TERMINFO", false}, // exact patterns must not match by prefix
		{"MY_TERM", false},
		{"SSH_CONNECTION", false},
		{"PATH", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := m.Match(tt.name); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCapture(t *testing.T) {
	environ := []string{
		"TERM=xterm-kitty",
		"KITTY_LISTEN_ON=unix:/tmp/kitty-123",
		"KITTY_PID=123",
		// Must not be captured: a session record is a file on disk, and a developer's
		// environment routinely holds credentials.
		"AWS_SECRET_ACCESS_KEY=hunter2",
		"PATH=/usr/bin",
		"malformed-no-equals",
		"=novalue",
	}

	got := Capture(environ, NewMatcher([]string{"TERM", "KITTY_*"}))
	want := map[string]string{
		"TERM":            "xterm-kitty",
		"KITTY_LISTEN_ON": "unix:/tmp/kitty-123",
		"KITTY_PID":       "123",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Capture() = %+v, want %+v", got, want)
	}
}

// An empty value is a real value, not an absence, so it must survive capture.
func TestCaptureKeepsEmptyValues(t *testing.T) {
	got := Capture([]string{"TERM="}, NewMatcher([]string{"TERM"}))
	want := map[string]string{"TERM": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Capture() = %+v, want %+v", got, want)
	}
}

func TestCompute(t *testing.T) {
	m := NewMatcher([]string{"KITTY_*", "TERM"})

	recorded := map[string]string{
		"KITTY_LISTEN_ON": "unix:/tmp/kitty-new",
		"TERM":            "xterm-kitty",
	}
	current := map[string]string{
		// Stale: the terminal restarted and this socket is gone.
		"KITTY_LISTEN_ON": "unix:/tmp/kitty-old",
		// Unchanged, so it should not be re-emitted.
		"TERM": "xterm-kitty",
		// The client no longer has this, so it must be unset rather than left stale.
		"KITTY_WINDOW_ID": "7",
		// Not managed by cm, so it must be left entirely alone.
		"PATH": "/usr/bin",
	}

	got := Compute(recorded, current, m)
	wantSet := map[string]string{"KITTY_LISTEN_ON": "unix:/tmp/kitty-new"}
	if !reflect.DeepEqual(got.Set, wantSet) {
		t.Errorf("Set = %+v, want %+v", got.Set, wantSet)
	}
	wantUnset := []string{"KITTY_WINDOW_ID"}
	if !reflect.DeepEqual(got.Unset, wantUnset) {
		t.Errorf("Unset = %v, want %v", got.Unset, wantUnset)
	}
}

// Variables cm does not manage must never be reported for unsetting, or a prompt hook would
// delete unrelated parts of the user's environment.
func TestComputeIgnoresUnmanagedVariables(t *testing.T) {
	m := NewMatcher([]string{"KITTY_*"})
	got := Compute(
		map[string]string{},
		map[string]string{"PATH": "/usr/bin", "HOME": "/home/user", "EDITOR": "vim"},
		m,
	)
	if len(got.Unset) != 0 {
		t.Errorf("Unset = %v, want empty: cm must not touch variables it does not manage", got.Unset)
	}
}

func TestRenderPlain(t *testing.T) {
	d := Diff{
		Set:   map[string]string{"KITTY_PID": "99", "TERM": "xterm-kitty"},
		Unset: []string{"KITTY_WINDOW_ID"},
	}
	// Sorted, so output is stable and diffable.
	want := "KITTY_PID=99\nTERM=xterm-kitty\n-KITTY_WINDOW_ID\n"
	if got := Render(d, FormatPlain); got != want {
		t.Errorf("Render(plain) = %q, want %q", got, want)
	}
}

func TestRenderPosix(t *testing.T) {
	d := Diff{
		Set:   map[string]string{"KITTY_LISTEN_ON": "unix:/tmp/kitty-1"},
		Unset: []string{"KITTY_WINDOW_ID"},
	}
	want := "export KITTY_LISTEN_ON='unix:/tmp/kitty-1'\nunset KITTY_WINDOW_ID\n"
	if got := Render(d, FormatPosix); got != want {
		t.Errorf("Render(posix) = %q, want %q", got, want)
	}
}

// fish shares none of POSIX's syntax for this: a bare assignment is a per-command prefix, export
// is not a builtin, and unset is `set -e`. Emitting POSIX to fish, as tmux does, is broken on all
// three counts.
func TestRenderFish(t *testing.T) {
	d := Diff{
		Set:   map[string]string{"KITTY_LISTEN_ON": "unix:/tmp/kitty-1"},
		Unset: []string{"KITTY_WINDOW_ID"},
	}
	want := "set -gx KITTY_LISTEN_ON 'unix:/tmp/kitty-1'\nset -e KITTY_WINDOW_ID\n"
	if got := Render(d, FormatFish); got != want {
		t.Errorf("Render(fish) = %q, want %q", got, want)
	}
}

// Values come from another process's environment, so they are not trustworthy just because they
// usually look like socket paths.
func TestRenderQuotesHostileValues(t *testing.T) {
	tests := []struct {
		name  string
		value string
		f     Format
		want  string
	}{
		{
			name:  "posix single quote",
			value: `a'b`,
			f:     FormatPosix,
			want:  "export X='a'\\''b'\n",
		},
		{
			name:  "posix command substitution stays literal",
			value: "$(rm -rf /)",
			f:     FormatPosix,
			want:  "export X='$(rm -rf /)'\n",
		},
		{
			name:  "posix semicolon stays literal",
			value: "x; echo pwned",
			f:     FormatPosix,
			want:  "export X='x; echo pwned'\n",
		},
		{
			name:  "fish backslash and quote",
			value: `a\b'c`,
			f:     FormatFish,
			want:  "set -gx X 'a\\\\b\\'c'\n",
		},
		{
			name:  "fish command substitution stays literal",
			value: "(rm -rf /)",
			f:     FormatFish,
			want:  "set -gx X '(rm -rf /)'\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Render(Diff{Set: map[string]string{"X": tt.value}}, tt.f)
			if got != tt.want {
				t.Errorf("Render() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseFormat(t *testing.T) {
	for _, name := range []string{"", "plain"} {
		if got, err := ParseFormat(name); err != nil || got != FormatPlain {
			t.Errorf("ParseFormat(%q) = (%v, %v), want (plain, nil)", name, got, err)
		}
	}
	for _, name := range []string{"posix", "sh", "bash", "zsh"} {
		if got, err := ParseFormat(name); err != nil || got != FormatPosix {
			t.Errorf("ParseFormat(%q) = (%v, %v), want (posix, nil)", name, got, err)
		}
	}
	if got, err := ParseFormat("fish"); err != nil || got != FormatFish {
		t.Errorf("ParseFormat(fish) = (%v, %v), want (fish, nil)", got, err)
	}
	if _, err := ParseFormat("tcsh"); err == nil {
		t.Error("ParseFormat(tcsh) = nil error, want a rejection rather than a silent default")
	}
}

// The default list has to cover what actually goes stale, or the feature does nothing useful out
// of the box.
func TestDefaultCaptureCoversTheStaleCases(t *testing.T) {
	m := NewMatcher(DefaultCapture)
	for _, name := range []string{
		"KITTY_LISTEN_ON", // every kitten call goes through this
		"KITTY_PID",
		"KITTY_WINDOW_ID",
		"TERM",
		"COLORTERM",
		"WINDOWID",
		"SSH_AUTH_SOCK", // stale agent socket breaks git in a long-lived session
		"DISPLAY",
		"WAYLAND_DISPLAY",
		"GHOSTTY_RESOURCES_DIR",
		"WEZTERM_PANE",
	} {
		if !m.Match(name) {
			t.Errorf("DefaultCapture does not match %q", name)
		}
	}

	// And must not sweep up credentials or general shell state.
	for _, name := range []string{
		"PATH", "HOME", "SHELL", "USER",
		"AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "OPENAI_API_KEY", "ANTHROPIC_API_KEY",
	} {
		if m.Match(name) {
			t.Errorf("DefaultCapture matches %q, which must not be recorded to disk", name)
		}
	}
}
