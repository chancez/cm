package keymap

import (
	"fmt"
	"strings"
)

// Map is what every key in one context means, after the config file has had its say.
//
// Built once per process and read from the key path, so nothing here allocates or parses per keystroke:
// Lookup walks the context's actions in definition order and compares an already-decoded press.
type Map struct {
	ctx   Context
	bound map[Action][]Chord
	order []Definition
}

// Problem is something wrong with a configured binding, described where the user can fix it.
//
// Collected rather than returned as an error, which is the split docs/config.md already commits to: the
// server reads the same file as every client and one bad value must not leave a session unreachable, so
// cm carries on with what it can understand and `cm config` is the command that fails. A binding is the
// same shape of problem. The cost of the other choice is worse than a wrong key: a config typo that
// refused every attach would take a person's whole terminal away from them.
type Problem struct {
	// Setting is the config path, "keys.overlay.next", so the message names something searchable.
	Setting string
	// Message says what is wrong and, where there is one, what to write instead.
	Message string
}

func (p Problem) String() string { return p.Setting + ": " + p.Message }

// Build resolves one context's bindings from the defaults and whatever the config file said.
//
// overrides is keyed by action name, and a present key replaces that action's defaults rather than adding
// to them. Replacing is the only rule that can take a default away: with adding, "n" would be bound to
// next forever. An empty list is therefore meaningful and means unbound.
func Build(ctx Context, overrides map[string][]string) (Map, []Problem) {
	defs := Definitions(ctx)
	m := Map{ctx: ctx, bound: make(map[Action][]Chord, len(defs)), order: defs}
	var problems []Problem

	known := make(map[string]bool, len(defs))
	for _, def := range defs {
		known[string(def.Action)] = true

		specs := def.Defaults
		if given, ok := overrides[string(def.Action)]; ok {
			specs = given
		}
		specs = append(append([]string(nil), specs...), def.Always...)

		setting := fmt.Sprintf("keys.%s.%s", ctx, def.Action)
		var chords []Chord
		for _, spec := range specs {
			c, err := ParseChord(spec)
			switch {
			case err != nil:
				problems = append(problems, Problem{Setting: setting, Message: err.Error()})
				continue
			case ctx == Session && c.Typing():
				// A character a program needs, refused here so it is reported rather than fatal: a session key
				// is taken from every program in the session, and "j" bound there would be unreachable in vim.
				// The same chord in the overlay is fine, since nothing competes for it while the bar is up.
				problems = append(problems, Problem{
					Setting: setting,
					Message: fmt.Sprintf(
						"%s is a character a program needs; a key taken from the session has to be a control "+
							"combination like ctrl-o or a named key like f5", c.Name),
				})
				continue
			case ctx == TUI && c.Tea == "":
				// A key bubbletea does not report is one this action could never fire on. Said here rather
				// than left to look like a key that does nothing.
				problems = append(problems, Problem{
					Setting: setting,
					Message: fmt.Sprintf("%s cannot be bound in the picker: it is not a key bubbletea reports", c.Name),
				})
				continue
			}
			if !hasChord(chords, c) {
				chords = append(chords, c)
			}
		}
		m.bound[def.Action] = chords
	}

	for name := range overrides {
		if !known[name] {
			// Ignored rather than fatal, for the reason Problem gives: one file serves every build on a
			// machine, so an action a newer cm knows and this one does not is ordinary rather than a typo.
			problems = append(problems, Problem{
				Setting: fmt.Sprintf("keys.%s.%s", ctx, name),
				Message: "unknown action for this build, ignored",
			})
		}
	}

	return m, append(problems, m.collisions()...)
}

// collisions reports keys bound to more than one action in this context.
//
// Reported rather than refused, and the winner is the action listed first in definitions, which is what
// Lookup does anyway. Compared with Chord.Same, so the pairs that are one keystroke under two names are
// caught: tab and ctrl-i, enter and ctrl-m, escape and ctrl-[.
func (m Map) collisions() []Problem {
	var problems []Problem
	for i, def := range m.order {
		for _, c := range m.bound[def.Action] {
			for _, earlier := range m.order[:i] {
				for _, other := range m.bound[earlier.Action] {
					if c.Same(other) {
						problems = append(problems, Problem{
							Setting: fmt.Sprintf("keys.%s.%s", m.ctx, def.Action),
							Message: fmt.Sprintf("%s is already bound to %s, which wins", c.Name, earlier.Action),
						})
					}
				}
			}
		}
	}
	return problems
}

// Lookup names the action a press performs, if any.
//
// In definition order, first match winning, so a collision resolves the same way on every machine and in
// every process: the overlay and the picker are separate processes reading the same file, and a map
// iteration here would let them disagree about what a key does.
func (m Map) Lookup(p Press) (Action, bool) {
	for _, def := range m.order {
		for _, c := range m.bound[def.Action] {
			if c.Matches(p) {
				return def.Action, true
			}
		}
	}
	return "", false
}

// Chords returns what an action is bound to, in the order they were configured.
func (m Map) Chords(a Action) []Chord { return m.bound[a] }

// Bound reports whether an action has any key at all.
func (m Map) Bound(a Action) bool { return len(m.bound[a]) > 0 }

// TeaKeys returns bubbletea's names for an action's keys, for a bubbles key.Binding.
func (m Map) TeaKeys(a Action) []string {
	chords := m.bound[a]
	out := make([]string, 0, len(chords))
	for _, c := range chords {
		if c.Tea != "" {
			out = append(out, c.Tea)
		}
	}
	return out
}

// First is the key to show for an action, or empty when it has none.
//
// The first configured key rather than all of them, because that is what a one-line help entry has room
// for: `cm config` is where the full set is readable.
func (m Map) First(a Action) string {
	if chords := m.bound[a]; len(chords) > 0 {
		return chords[0].Name
	}
	return ""
}

// Names lists every key for an action, "n, ctrl-n", or empty when it has none.
func (m Map) Names(a Action) string {
	chords := m.bound[a]
	names := make([]string, 0, len(chords))
	for _, c := range chords {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// Label is an action's help text.
func (m Map) Label(a Action) string {
	for _, def := range m.order {
		if def.Action == a {
			return def.Label
		}
	}
	return ""
}

// Add appends a key to an action, for one a caller supplies rather than the config.
//
// The picker's way back is the case: it is the prefix key of whichever client opened the picker, which is
// a fact about that client rather than a preference, so it cannot be a default and the config cannot know
// it. Ignored when the spec does not parse or is already bound, since a caller passing something unusable
// is not worth failing a picker over.
func (m Map) Add(a Action, spec string) {
	c, err := ParseChord(spec)
	if err != nil || (m.ctx == TUI && c.Tea == "") {
		return
	}
	if !hasChord(m.bound[a], c) {
		m.bound[a] = append(m.bound[a], c)
	}
}

// Actions returns the context's actions in order, for rendering help or `cm config`.
func (m Map) Actions() []Definition { return m.order }

func hasChord(chords []Chord, c Chord) bool {
	for _, have := range chords {
		if have.Same(c) {
			return true
		}
	}
	return false
}
