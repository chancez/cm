package client

import (
	"strings"
	"testing"
)

func TestParseStyle(t *testing.T) {
	tests := []struct {
		spec    string
		want    string
		wantErr bool
	}{
		{spec: "reverse", want: "\x1b[7m"},
		{spec: "bold reverse", want: "\x1b[1;7m"},
		{spec: "white on bright-black", want: "\x1b[37;100m"},
		{spec: "black on white", want: "\x1b[30;47m"},
		{spec: "bright-white on blue", want: "\x1b[97;44m"},
		{spec: "default on default", want: "\x1b[39;49m"},
		{spec: "color15 on color238", want: "\x1b[38;5;15;48;5;238m"},
		{spec: "#ffffff on #303030", want: "\x1b[38;2;255;255;255;48;2;48;48;48m"},
		// Case and spacing are a hand-written config file's business, not the reader's.
		{spec: "  BOLD   White   ON   Bright-Black  ", want: "\x1b[1;37;100m"},
		// Empty is not an error: it means the caller's default.
		{spec: "", want: ""},

		{spec: "white on", wantErr: true},
		{spec: "chartreuse", wantErr: true},
		{spec: "bright-chartreuse", wantErr: true},
		{spec: "color256", wantErr: true},
		{spec: "color-1", wantErr: true},
		{spec: "#fff", wantErr: true},
		{spec: "#gggggg", wantErr: true},
		// Faint is refused by omission rather than accepted and regretted: it is what made the overlay
		// unreadable, because a terminal applies it to the foreground and these regions are their background.
		{spec: "faint", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := ParseStyle(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseStyle(%q) error = %v, wantErr %v", tt.spec, err, tt.wantErr)
			}
			if tt.wantErr {
				// The message has to name the offending word, since the whole point is fixing a config file.
				if !strings.Contains(err.Error(), tt.spec) && !strings.Contains(err.Error(), "colour") {
					t.Errorf("error %q names neither the style nor the problem", err)
				}
				return
			}
			if string(got) != tt.want {
				t.Errorf("ParseStyle(%q) = %q, want %q", tt.spec, got, tt.want)
			}
		})
	}
}

// The defaults are parsed by the same code a config file goes through, so a typo in one is a test failure
// rather than an unstyled overlay.
func TestDefaultStylesParse(t *testing.T) {
	for _, spec := range []string{DefaultBarStyle, DefaultBodyStyle, DefaultSelectedStyle} {
		got, err := ParseStyle(spec)
		if err != nil {
			t.Errorf("ParseStyle(%q) error = %v", spec, err)
		}
		if got == "" {
			t.Errorf("ParseStyle(%q) produced nothing", spec)
		}
	}

	// And the body's default names a colour pair rather than an attribute, which is the point of it: an
	// attribute is resolved against the theme's own colours, and faint plus reverse is what came out grey on
	// grey. A pair from the palette carries its own contrast.
	// The three defaults are a ramp, and the selection must differ from the bar: they merged into one block
	// when the cursor sat on the first row and the selection borrowed the bar's style.
	if DefaultSelectedStyle == DefaultBarStyle {
		t.Error("the selected row and the bar are the same style, so they merge when adjacent")
	}

	body, _ := ParseStyle(DefaultBodyStyle)
	if !strings.Contains(string(body), "97") || !strings.Contains(string(body), "48;5;236") {
		t.Errorf("the default body style is %q, want near-white on a fixed dark grey", body)
	}
}
