// Package tags handles the key/value labels attached to a session, and the selectors that
// filter on them.
//
// Tags exist because a session's name can only group it one way, and often cannot group it at
// all. A per-window session is named by the server from a counter, so it is called "s17" and
// carries no meaning for `cm list --prefix` to match on. Even a named session has one name and
// belongs to several groupings at once: a project, a worktree, and the fan-out that created it.
//
// Key/value rather than bare labels, so the same field can hold something a program needs to
// remember about itself rather than only something to filter on. That keeps cm from growing a
// second metadata mechanism later.
//
// A tag is metadata and nothing more. cm never interprets a key: there is no key that changes
// how a session is treated, because inferring meaning from a tag is the same mistake as
// scraping a screen to work out what is running. Config may key off a tag, since the person
// writing the config chose the key.
package tags

import (
	"fmt"
	"sort"
	"strings"
)

// MaxKeyLen and MaxValueLen bound each half of a tag.
//
// A bound is needed because tags are stored in one JSON column that is read and written whole,
// and are printed in a table beside sessions. 63 bytes is what a DNS label allows and what
// Kubernetes uses for the same job, so it is a limit users have met before rather than one
// invented here.
const (
	MaxKeyLen   = 63
	MaxValueLen = 63
)

// ValidateKey reports whether a tag key is usable.
//
// The allowed set is letters, digits, '-', '_', '.', and '/'. Slashes are permitted so a key can
// be namespaced the way an annotation usually is, as in "cm.dev/run".
//
// Narrow on purpose, and for a reason beyond tidiness: a tag comes from a caller and is printed
// straight to a terminal by `cm list`. Excluding everything outside this set excludes escape
// sequences, so a tag cannot repaint or retitle the terminal of whoever lists sessions. In a tool
// whose whole subject is escape sequences that is worth enforcing at the boundary rather than at
// each place a value is displayed.
//
// The same set also keeps a tag usable unquoted in a shell and unambiguous in a selector, since
// neither '=' nor ',' is in it.
func ValidateKey(key string) error {
	if key == "" {
		return fmt.Errorf("tag key is empty")
	}
	if len(key) > MaxKeyLen {
		return fmt.Errorf("tag key %q is %d bytes, limit is %d", key, len(key), MaxKeyLen)
	}
	if err := checkChars("tag key", key); err != nil {
		return err
	}
	return nil
}

// ValidateValue reports whether a tag value is usable.
//
// Empty is allowed, which is what a bare `--tag review` stores: a key alone is a common and
// useful thing to say, and requiring a value would force callers to invent one.
func ValidateValue(value string) error {
	if len(value) > MaxValueLen {
		return fmt.Errorf("tag value %q is %d bytes, limit is %d", value, len(value), MaxValueLen)
	}
	return checkChars("tag value", value)
}

// checkChars enforces the shared character set.
func checkChars(what, s string) error {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == '/':
		default:
			return fmt.Errorf("%s %q contains disallowed character %q; "+
				"allowed are letters, digits, '-', '_', '.', and '/'", what, s, r)
		}
	}
	return nil
}

// Validate checks a whole set of tags.
func Validate(t map[string]string) error {
	for k, v := range t {
		if err := ValidateKey(k); err != nil {
			return err
		}
		if err := ValidateValue(v); err != nil {
			return fmt.Errorf("tag %q: %w", k, err)
		}
	}
	return nil
}

// Parse reads a "key" or "key=value" argument.
//
// "key=" is the same as "key". Storing a key with a deliberately empty value and storing the key
// alone would be indistinguishable to anyone reading the output, so they are made identical here
// instead of being a distinction only the database knows about.
//
// A value containing '=' is rejected by the character set rather than being silently split, so
// `--tag a=b=c` reports the problem instead of storing "b=c" or "b".
func Parse(arg string) (key, value string, err error) {
	key, value, _ = strings.Cut(arg, "=")
	if err := ValidateKey(key); err != nil {
		return "", "", err
	}
	if err := ValidateValue(value); err != nil {
		return "", "", fmt.Errorf("tag %q: %w", key, err)
	}
	return key, value, nil
}

// ParseAll reads repeated "key=value" arguments into a set.
//
// A later argument overwrites an earlier one with the same key, which is how a repeatable flag is
// expected to behave.
func ParseAll(args []string) (map[string]string, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(args))
	for _, arg := range args {
		k, v, err := Parse(arg)
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

// Selector is a filter over a session's tags.
//
// Every term must match, so repeating the flag narrows. There is no "or" and no negation: a
// selector grammar with '!=' and set membership is familiar from elsewhere, but it is more syntax
// than filtering a list of tens of sessions justifies, and it can be added later without changing
// what already works.
type Selector struct {
	// terms are the parsed requirements, in the order given.
	terms []term
}

// term is one requirement. A term with no value matches the key whatever its value, which is what
// makes `--tag project` mean "belongs to some project" and `--tag project=cm` mean "this one".
type term struct {
	key      string
	value    string
	hasValue bool
}

// ParseSelector builds a selector from repeated "key" or "key=value" arguments.
func ParseSelector(args []string) (Selector, error) {
	var sel Selector
	for _, arg := range args {
		key, value, _ := strings.Cut(arg, "=")
		if err := ValidateKey(key); err != nil {
			return Selector{}, err
		}
		if err := ValidateValue(value); err != nil {
			return Selector{}, fmt.Errorf("tag %q: %w", key, err)
		}
		// "key=" is treated as the bare key, matching how Parse stores it. Otherwise `--tag k=`
		// would select only sessions whose value is empty, which is a distinction the set side
		// deliberately does not make.
		sel.terms = append(sel.terms, term{key: key, value: value, hasValue: value != ""})
	}
	return sel, nil
}

// Empty reports whether the selector filters anything out.
func (s Selector) Empty() bool { return len(s.terms) == 0 }

// Match reports whether a session's tags satisfy every term.
func (s Selector) Match(t map[string]string) bool {
	for _, term := range s.terms {
		v, ok := t[term.key]
		if !ok {
			return false
		}
		if term.hasValue && v != term.value {
			return false
		}
	}
	return true
}

// Format renders tags as a stable "k=v,k" string for display.
//
// Sorted by key, because a map iterates in a random order and a table whose columns reshuffle
// between calls is unreadable. A key with an empty value prints alone, matching how it is written.
func Format(t map[string]string) string {
	if len(t) == 0 {
		return ""
	}
	keys := make([]string, 0, len(t))
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if t[k] == "" {
			parts = append(parts, k)
			continue
		}
		parts = append(parts, k+"="+t[k])
	}
	return strings.Join(parts, ",")
}
