// Package shquote renders values and argument lists as shell words that parse back to exactly what they
// were.
//
// Two dialects, because fish is not POSIX inside single quotes: a backslash there escapes a following
// backslash or quote, so a POSIX-quoted `a\\b` reads back in fish as `a\b`.
package shquote

import (
	"regexp"
	"strings"
)

// POSIX wraps a value in single quotes for sh, bash, and zsh.
//
// Single quotes are literal in all three, so the only character needing care is the quote itself, which is
// closed, escaped, and reopened.
func POSIX(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// Fish wraps a value in single quotes for fish, where backslash and single quote are the two characters
// that remain special inside them.
func Fish(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "'", `\'`)
	return "'" + v + "'"
}

// plain matches a word no shell here treats specially, which is left unquoted so a command reads as it was
// typed: `claude --resume abc` rather than `'claude' '--resume' 'abc'`.
var plain = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// Join renders an argv as one command line for a shell, POSIX unless fish is set.
func Join(argv []string, fish bool) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		switch {
		case plain.MatchString(a):
			words[i] = a
		case fish:
			words[i] = Fish(a)
		default:
			words[i] = POSIX(a)
		}
	}
	return strings.Join(words, " ")
}
