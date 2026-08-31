package client

import (
	"fmt"
	"strconv"
	"strings"
)

// Style is the escape sequence that opens one of the overlay's regions.
//
// A parsed value rather than a string of SGR parameters in a config file, so a mistake is reported when
// the client starts rather than drawn on somebody's screen.
type Style string

// The overlay's default appearance, and the reasoning is contrast rather than taste.
//
// The first attempt at two shades used reverse video for the bar and reverse *plus faint* for the rows
// under it, on the theory that faint would read as a dimmer version of the same swap. It does not. Faint
// dims the foreground, and reverse then draws that dimmed colour as the background, so the rows came out
// grey text on a grey field: legible in the terminal it was written in and very nearly invisible in the
// next one. Screenshot, not inference.
//
// So the rows name their colours instead, where the *pair* carries the contrast rather than the theme. The
// background is color238, a fixed entry in the 256-colour greyscale ramp: dark on any theme, and unlike
// palette entry 8 it does not depend on what a colour scheme decided "bright black" means -- measured,
// white on bright-black came out around 3:1 in kitty's default scheme, which is legible and no better than
// that. Near-white on a fixed dark grey is closer to 10:1.
//
// The bar keeps reverse video, which is the one thing guaranteed to contrast with the theme it is drawn in
// and which looks like a bar on a light background as well as a dark one.
//
// The three are a ramp -- reverse, then grey 241, then grey 236 -- rather than two tones with the selection
// borrowing the bar's. Borrowing it looked right until the cursor sat on the first row, where the bar and
// the selection then merged into one block with no boundary. Contrast against near-white stays above 5:1 at
// both greys.
//
// Every one of these is overridable, because no default survives every theme: see ParseStyle.
const (
	DefaultBarStyle      = "reverse"
	DefaultBodyStyle     = "bright-white on color236"
	DefaultSelectedStyle = "bright-white on color241"
)

// ParseStyle reads a style from the config file.
//
// The grammar is a few words: colour names set the foreground, "on <colour>" sets the background, and
// attribute names turn attributes on.
//
//	reverse
//	white on bright-black
//	bold black on white
//	color15 on color238
//	#ffffff on #303030
//
// Named colours come from the ANSI palette, which is what makes a default legible in a theme cm has never
// seen: the terminal maps them, so "white on bright-black" is that terminal's own idea of light-on-grey.
// A number or a hex triple pins the colour exactly, for someone who would rather say.
func ParseStyle(spec string) (Style, error) {
	fields := strings.Fields(strings.ToLower(spec))
	if len(fields) == 0 {
		return "", nil
	}

	var params []string
	for i := 0; i < len(fields); i++ {
		word := fields[i]

		if word == "on" {
			// The next word is a background, and there has to be one: "white on" is a typo for something,
			// and guessing which would paint the wrong thing.
			if i+1 >= len(fields) {
				return "", fmt.Errorf("style %q: \"on\" needs a colour after it", spec)
			}
			bg, err := colorParams(fields[i+1], true)
			if err != nil {
				return "", fmt.Errorf("style %q: %w", spec, err)
			}
			params = append(params, bg...)
			i++
			continue
		}

		if attr, ok := styleAttrs[word]; ok {
			params = append(params, attr)
			continue
		}

		fg, err := colorParams(word, false)
		if err != nil {
			return "", fmt.Errorf("style %q: %w", spec, err)
		}
		params = append(params, fg...)
	}

	return Style("\x1b[" + strings.Join(params, ";") + "m"), nil
}

// styleAttrs are the attributes worth naming.
//
// Faint is deliberately absent, and this is the one entry that is a decision rather than a list: it is what
// made the overlay unreadable, because a terminal renders it against the foreground and the overlay's
// regions are defined by their background. Someone who wants it can say so in numbers.
var styleAttrs = map[string]string{
	"bold":      "1",
	"italic":    "3",
	"underline": "4",
	"reverse":   "7",
	"plain":     "0",
}

// colorParams turns one colour word into SGR parameters.
func colorParams(word string, background bool) ([]string, error) {
	base := 30
	if background {
		base = 40
	}

	if rest, bright := strings.CutPrefix(word, "bright-"); bright {
		if n, ok := ansiColors[rest]; ok {
			// The aixterm range, 90-97 and 100-107, which every terminal cm runs in understands. Bright as a
			// *colour* rather than as bold, since bold would also embolden the text.
			return []string{strconv.Itoa(base + 60 + n)}, nil
		}
		return nil, fmt.Errorf("no colour called %q", word)
	}
	if n, ok := ansiColors[word]; ok {
		return []string{strconv.Itoa(base + n)}, nil
	}
	if word == "default" {
		return []string{strconv.Itoa(base + 9)}, nil
	}
	if rest, ok := strings.CutPrefix(word, "color"); ok {
		n, err := strconv.Atoi(rest)
		if err != nil || n < 0 || n > 255 {
			return nil, fmt.Errorf("colour %q is not color0 to color255", word)
		}
		return []string{strconv.Itoa(base + 8), "5", strconv.Itoa(n)}, nil
	}
	if hex, ok := strings.CutPrefix(word, "#"); ok {
		r, g, b, err := parseHex(hex)
		if err != nil {
			return nil, fmt.Errorf("colour %q: %w", word, err)
		}
		return []string{
			strconv.Itoa(base + 8), "2",
			strconv.Itoa(r), strconv.Itoa(g), strconv.Itoa(b),
		}, nil
	}
	return nil, fmt.Errorf("no colour called %q", word)
}

// ansiColors are the eight palette entries, by their offset from 30 and 40.
var ansiColors = map[string]int{
	"black":   0,
	"red":     1,
	"green":   2,
	"yellow":  3,
	"blue":    4,
	"magenta": 5,
	"cyan":    6,
	"white":   7,
}

// parseHex reads rrggbb.
func parseHex(hex string) (r, g, b int, err error) {
	if len(hex) != 6 {
		return 0, 0, 0, fmt.Errorf("want six hex digits, got %d", len(hex))
	}
	v, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("not hexadecimal")
	}
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff), nil
}

// styleOr falls back to a default, which is parsed rather than written out as an escape sequence so the
// defaults above are checked by the same code a config file goes through.
//
// A default that failed to parse would be a bug in this file rather than in anybody's configuration, so it
// resolves to nothing rather than being reported: an unstyled overlay is legible, and a client that refused
// to attach over it would not be.
func styleOr(style Style, fallback string) Style {
	if style != "" {
		return style
	}
	parsed, err := ParseStyle(fallback)
	if err != nil {
		return ""
	}
	return parsed
}
