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

// A session on another machine gets the terminal, the locale, and the name of the machine watching it.
//
// Nothing else, and the client host is the only value cm supplies rather than forwards: everything a
// terminal cannot be inferred from belongs to the far host. The order is asserted along with the contents
// because a later entry wins at exec, and --env is appended after this by the caller.
func TestCrossHostSendsTheTerminalAndTheWatchingMachine(t *testing.T) {
	got := CrossHost([]string{
		"TERM=xterm-kitty",
		"COLORTERM=truecolor",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		// Local to this machine and unreachable from the other one, whatever the session would like.
		"KITTY_LISTEN_ON=unix:/tmp/kitty-1",
		"SSH_AUTH_SOCK=/tmp/agent.1",
		"TERMINFO=/opt/homebrew/share/terminfo",
		// The far host builds these itself, and this machine's are wrong there.
		"PATH=/opt/homebrew/bin",
		"HOME=/Users/someone",
		"AWS_SECRET_ACCESS_KEY=hunter2",
	}, "laptop.local")

	want := []string{
		"TERM=xterm-kitty",
		"COLORTERM=truecolor",
		"LANG=en_US.UTF-8",
		"LC_ALL=en_US.UTF-8",
		ClientHostVar + "=laptop.local",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CrossHost() = %q, want %q", got, want)
	}
}

// An unknown hostname omits the variable rather than exporting a placeholder.
//
// Because presence is the signal: starship 1.26.0's hostname module takes
// detect_env_vars = ["SSH_CONNECTION", "CM_CLIENT_HOST"] and tests whether the name is set rather than what
// it holds, so an empty value would put a hostname in a prompt that is nowhere near another machine.
func TestCrossHostOmitsAnUnknownWatchingMachine(t *testing.T) {
	got := CrossHost([]string{"TERM=xterm-kitty"}, "")
	want := []string{"TERM=xterm-kitty"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CrossHost() = %q, want %q", got, want)
	}
}

// A server drops the client host rather than handing it to every session it later creates.
//
// The SSH_CLIENT incident with a different name on it: a server started from a shell inside a remote session
// inherits this, every shim inherits the server's environment, and a name the creating client does not have
// is never overwritten. Every session on that host would then claim to be watched from a machine that has
// never seen it. Membership of DefaultCapture is what makes this true, so this is the guard on that.
func TestClientValuesDropsTheWatchingMachine(t *testing.T) {
	environ := []string{"PATH=/usr/bin", ClientHostVar + "=laptop.local"}
	got := ClientValues(environ, NewMatcher(DefaultCapture))
	want := []string{ClientHostVar}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ClientValues() = %v, want %v", got, want)
	}
}

// And `cm get-env` retires it when the same session is attached on the host itself.
//
// A session created from a laptop and later attached where it lives is no longer watched from anywhere else,
// and a shell keeping the variable would keep saying otherwise for the rest of its life. Compute reports a
// captured name the current client does not have as a removal, which is the same machinery that retires a
// dead KITTY_LISTEN_ON.
func TestComputeRetiresTheWatchingMachine(t *testing.T) {
	got := Compute(
		map[string]string{"TERM": "xterm-kitty"},
		map[string]string{"TERM": "xterm-kitty", ClientHostVar: "laptop.local"},
		NewMatcher(DefaultCapture))
	want := Diff{Set: map[string]string{}, Unset: []string{ClientHostVar}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Compute() = %+v, want %+v", got, want)
	}
}

func TestInherit(t *testing.T) {
	got := Inherit([]string{
		"PATH=/client/bin:/usr/bin",
		"TERM=xterm-kitty",
		"KITTY_LISTEN_ON=unix:/tmp/kitty-123",
		"HOME=/home/someone",
		// Forwarded like anything else. A session gets what the client had, which is the whole
		// point, and is what a terminal split gives you too.
		"MY_API_TOKEN=shhh",
		// Dropped: these choose what code a process loads rather than how it behaves.
		"LD_PRELOAD=/tmp/evil.so",
		"LD_LIBRARY_PATH=/tmp/evil",
		"LD_AUDIT=/tmp/audit.so",
		"DYLD_INSERT_LIBRARIES=/tmp/evil.dylib",
		"DYLD_LIBRARY_PATH=/tmp/evil",
		// Skipped rather than guessed at.
		"malformed-no-equals",
		"=novalue",
	})
	want := []string{
		"PATH=/client/bin:/usr/bin",
		"TERM=xterm-kitty",
		"KITTY_LISTEN_ON=unix:/tmp/kitty-123",
		"HOME=/home/someone",
		"MY_API_TOKEN=shhh",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Inherit() = %+v, want %+v", got, want)
	}
}

// Order is preserved rather than sorted, so a spawn is reproducible and a duplicated name resolves
// the same way every time. exec keeps the last occurrence, so the order decides the winner.
func TestInheritPreservesOrder(t *testing.T) {
	got := Inherit([]string{"B=2", "A=1", "C=3", "A=4"})
	want := []string{"B=2", "A=1", "C=3", "A=4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Inherit() = %+v, want %+v", got, want)
	}
}

// An empty value is a real value, distinct from the name being absent, so it survives.
func TestInheritKeepsAnEmptyValue(t *testing.T) {
	got := Inherit([]string{"PATH="})
	want := []string{"PATH="}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Inherit() = %+v, want %+v", got, want)
	}
}

// A client with nothing worth forwarding yields nothing, rather than being topped up here. What a
// sparse client's session falls back to is shimEnv's decision, not this function's.
func TestInheritWithAnEmptyEnvironment(t *testing.T) {
	got := Inherit(nil)
	want := []string{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Inherit() = %+v, want %+v", got, want)
	}
}

// The linker exclusions must not accidentally catch ordinary names that merely start the same way.
// LD_ and DYLD_ are short prefixes, and DYLD_* is the only pattern here that matches by prefix.
func TestNoInheritDoesNotOvermatch(t *testing.T) {
	got := Inherit([]string{
		"LDFLAGS=-L/usr/lib",
		"LD_PRELOAD_ISH=nope",
		"MY_LD_PRELOAD=nope",
		"DYLD_PRINT_LIBRARIES=1",
	})
	// LD_PRELOAD_ISH and LDFLAGS survive because the LD_ entries are exact matches, while
	// DYLD_PRINT_LIBRARIES goes because DYLD_* is a prefix pattern and every DYLD_ variable
	// influences loading.
	want := []string{"LDFLAGS=-L/usr/lib", "LD_PRELOAD_ISH=nope", "MY_LD_PRELOAD=nope"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Inherit() = %+v, want %+v", got, want)
	}
}

func TestClientValues(t *testing.T) {
	environ := []string{
		"TERM=xterm-kitty",
		"KITTY_PID=123",
		"SSH_CLIENT=192.0.2.1 51174 22",
		// Kept: the machine's, not a client's, and a shim with no PATH cannot start a shell.
		"PATH=/usr/bin",
		"HOME=/home/someone",
		// Dropped without being in the capture list, since it names one session.
		"CM_SESSION=work",
		"malformed-no-equals",
		"=novalue",
		// Listed twice, which os.Environ can produce, and reported once.
		"TERM=xterm-256color",
	}

	got := ClientValues(environ, NewMatcher([]string{"TERM", "KITTY_*", "SSH_*"}))
	want := []string{"CM_SESSION", "KITTY_PID", "SSH_CLIENT", "TERM"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ClientValues() = %v, want %v", got, want)
	}
}

// Nothing to drop is the ordinary case, and it must not be reported as something to do: the server
// logs the names it dropped, and a line listing none would be noise on every start.
func TestClientValuesEmptyWhenNothingMatches(t *testing.T) {
	got := ClientValues([]string{"PATH=/usr/bin", "HOME=/home/someone"}, NewMatcher([]string{"TERM"}))
	if len(got) != 0 {
		t.Errorf("ClientValues() = %v, want nothing", got)
	}
}
