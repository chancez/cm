package osc

import (
	"os"
	"strings"
	"testing"
)

func TestParseCwd(t *testing.T) {
	self, err := os.Hostname()
	if err != nil {
		t.Fatalf("Hostname() error = %v", err)
	}

	tests := []struct {
		name  string
		input string
		want  Cwd
		ok    bool
	}{
		{
			name:  "file URI with no host",
			input: "file:///home/user/projects",
			want:  Cwd{Path: "/home/user/projects", IsLocal: true},
			ok:    true,
		},
		{
			name:  "file URI with local host",
			input: "file://" + self + "/home/user",
			want:  Cwd{Path: "/home/user", Host: self, IsLocal: true},
			ok:    true,
		},
		{
			name:  "file URI with localhost",
			input: "file://localhost/tmp",
			want:  Cwd{Path: "/tmp", Host: "localhost", IsLocal: true},
			ok:    true,
		},
		{
			// A session that ssh'd elsewhere reports a path that does not exist here, so
			// treating it as local would open new windows in the wrong place or fail.
			name:  "remote host is not local",
			input: "file://some-other-machine/home/user",
			want:  Cwd{Path: "/home/user", Host: "some-other-machine", IsLocal: false},
			ok:    true,
		},
		{
			// kitty's shell integration uses its own scheme, so the scheme cannot be checked
			// against a fixed value.
			name:  "kitty scheme",
			input: "kitty-shell-cwd://" + self + "/var/tmp",
			want:  Cwd{Path: "/var/tmp", Host: self, IsLocal: true},
			ok:    true,
		},
		{
			name:  "percent-encoded path",
			input: "file:///home/user/my%20projects/a%2Bb",
			want:  Cwd{Path: "/home/user/my projects/a+b", IsLocal: true},
			ok:    true,
		},
		{
			// OSC 9 and OSC 1337 send bare paths rather than URIs.
			name:  "bare path",
			input: "/home/user/plain",
			want:  Cwd{Path: "/home/user/plain", IsLocal: true},
			ok:    true,
		},
		{
			// libghostty appends a NUL sentinel. Forwarding it breaks kitty's session file.
			name:  "trailing NUL is stripped",
			input: "file:///home/user\x00",
			want:  Cwd{Path: "/home/user", IsLocal: true},
			ok:    true,
		},
		{
			// A shell clears the value by reporting nothing.
			name:  "empty clears",
			input: "",
			ok:    false,
		},
		{
			name:  "only a NUL",
			input: "\x00",
			ok:    false,
		},
		{
			name:  "no path",
			input: "file://host",
			ok:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseCwd(tt.input)
			if ok != tt.ok {
				t.Fatalf("ParseCwd(%q) ok = %v, want %v", tt.input, ok, tt.ok)
			}
			if !ok {
				return
			}
			if got != tt.want {
				t.Errorf("ParseCwd(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

// A bare hostname and a fully qualified one refer to the same machine, and treating that as
// remote would wrongly break directory inheritance for ordinary local sessions.
func TestParseCwdMatchesShortAndLongHostnames(t *testing.T) {
	self, err := os.Hostname()
	if err != nil {
		t.Fatalf("Hostname() error = %v", err)
	}
	short := firstLabel(self)

	got, ok := ParseCwd("file://" + short + ".example.com/home/user")
	if !ok {
		t.Fatal("ParseCwd() ok = false, want true")
	}
	if !got.IsLocal {
		t.Errorf("IsLocal = false for %q against local host %q, want true", short+".example.com", self)
	}
}

func TestRewritePromptRedraw(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no OSC 133 passes through untouched",
			input: "plain output\r\n",
			want:  "plain output\r\n",
		},
		{
			// The case that motivates this: the shell claims it will repaint, the outer terminal
			// clears the prompt on resize, and the repaint never arrives usefully.
			name:  "redraw=1 becomes redraw=0",
			input: "\x1b]133;A;redraw=1\x07$ ",
			want:  "\x1b]133;A;redraw=0\x07$ ",
		},
		{
			name:  "redraw=0 is left alone",
			input: "\x1b]133;A;redraw=0\x07$ ",
			want:  "\x1b]133;A;redraw=0\x07$ ",
		},
		{
			// Unset means terminal-defined, so it has to be pinned rather than trusted.
			name:  "missing redraw gets one",
			input: "\x1b]133;A\x07$ ",
			want:  "\x1b]133;A;redraw=0\x07$ ",
		},
		{
			name:  "ST terminator",
			input: "\x1b]133;A;redraw=1\x1b\\$ ",
			want:  "\x1b]133;A;redraw=0\x1b\\$ ",
		},
		{
			name:  "other OSC 133 markers are untouched",
			input: "\x1b]133;B\x07\x1b]133;C\x07",
			want:  "\x1b]133;B\x07\x1b]133;C\x07",
		},
		{
			name:  "multiple prompts",
			input: "\x1b]133;A;redraw=1\x07a\x1b]133;A;redraw=1\x07b",
			want:  "\x1b]133;A;redraw=0\x07a\x1b]133;A;redraw=0\x07b",
		},
		{
			name:  "other parameters are preserved",
			input: "\x1b]133;A;aid=123;redraw=1\x07",
			want:  "\x1b]133;A;aid=123;redraw=0\x07",
		},
		{
			// Rewriting a fragment would corrupt it; the rest arrives in the next chunk.
			name:  "unterminated sequence passes through",
			input: "output\x1b]133;A;redraw=1",
			want:  "output\x1b]133;A;redraw=1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(RewritePromptRedraw([]byte(tt.input)))
			if got != tt.want {
				t.Errorf("RewritePromptRedraw(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// Output surrounding a marker must survive untouched, since this runs over every byte of a
// session's output.
func TestRewritePromptRedrawPreservesSurroundingOutput(t *testing.T) {
	prefix := strings.Repeat("before ", 100)
	suffix := strings.Repeat(" after", 100)
	input := prefix + "\x1b]133;A;redraw=1\x07" + suffix

	got := string(RewritePromptRedraw([]byte(input)))
	if !strings.HasPrefix(got, prefix) {
		t.Error("output before the marker was modified")
	}
	if !strings.HasSuffix(got, suffix) {
		t.Error("output after the marker was modified")
	}
	if strings.Contains(got, "redraw=1") {
		t.Error("redraw=1 survived the rewrite")
	}
}
