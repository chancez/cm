package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/keymap"
	"github.com/chancez/cm/internal/paths"
)

// keysJSON is what `cm keys --json` prints.
type keysJSON struct {
	// Detach and Prefix are the keys a client intercepts, which belong to no interface: they are matched
	// in the byte stream before the overlay or anything else sees them.
	Detach []string `json:"detach"`
	Prefix []string `json:"prefix"`
	// Overlay and TUI are the bindings in effect, in the order a keypress is matched against them.
	Overlay []bindingJSON `json:"overlay"`
	TUI     []bindingJSON `json:"tui"`
	// Problems are the settings that were not usable, which is also what makes this command exit
	// non-zero. Empty on a healthy config.
	Problems []string `json:"problems"`
}

type bindingJSON struct {
	Action string   `json:"action"`
	Keys   []string `json:"keys"`
	Does   string   `json:"does"`
}

// newKeysCommand prints the keys in effect.
//
// Its own command rather than more lines in `cm config`, which is already a screen of settings: this is
// forty bindings, and the question it answers is "what does this key do" rather than "what is configured".
// It is also the answer to a rebinding that appears not to work, since it prints what cm resolved rather
// than what the file says.
func newKeysCommand(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Show the keys cm's overlay and picker are bound to",
		Long: `List every action in cm's own interfaces and the keys bound to it.

Bindings come from [keys] in the config file, where a list replaces an
action's defaults and an empty list unbinds it. Anything unusable is
reported here and this command exits non-zero, while cm itself carries
on with the bindings it understood.

The detach and prefix keys are separate: a client matches them in the
byte stream before either interface sees a keypress.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := g.config()
			if err != nil {
				return err
			}

			out := keysJSON{
				Detach:   orDefault(cfg.DetachKeys(), keymap.DefaultDetachKey),
				Prefix:   orDefault(cfg.PrefixKeys(), keymap.DefaultPrefixKey),
				Problems: []string{},
			}
			for _, ctx := range []keymap.Context{keymap.Overlay, keymap.TUI} {
				m, problems := cfg.Keymap(ctx)
				bindings := make([]bindingJSON, 0, len(m.Actions()))
				for _, def := range m.Actions() {
					keys := make([]string, 0, len(m.Chords(def.Action)))
					for _, c := range m.Chords(def.Action) {
						keys = append(keys, c.Name)
					}
					bindings = append(bindings, bindingJSON{
						Action: string(def.Action),
						Keys:   keys,
						Does:   def.Label,
					})
				}
				if ctx == keymap.Overlay {
					out.Overlay = bindings
				} else {
					out.TUI = bindings
				}
				for _, p := range problems {
					out.Problems = append(out.Problems, p.String())
				}
			}

			if asJSON {
				if err := writeJSON(os.Stdout, out); err != nil {
					return err
				}
				return keyProblemsError(out.Problems)
			}

			fmt.Fprintf(os.Stdout, "detach  %s\n", join(out.Detach))
			fmt.Fprintf(os.Stdout, "prefix  %s\n", join(out.Prefix))
			// Through a tabwriter because the key column's width depends on what is bound: "ctrl-k, ctrl-p,
			// up" against "s". Aligning to a guess leaves the labels ragged the moment anything is rebound.
			for _, section := range []struct {
				title    string
				bindings []bindingJSON
			}{
				{title: "overlay, after " + join(out.Prefix), bindings: out.Overlay},
				{title: paths.Name + " tui", bindings: out.TUI},
			} {
				fmt.Fprintf(os.Stdout, "\n%s\n", section.title)
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
				for _, b := range section.bindings {
					keys := join(b.Keys)
					if keys == "" {
						// Named rather than omitted: an action with no key is a thing somebody did on purpose
						// with an empty list, and a missing row reads as a cm that has lost the action.
						keys = "(unbound)"
					}
					fmt.Fprintf(w, "  %s\t%s\t%s\n", b.Action, keys, b.Does)
				}
				w.Flush()
			}

			if len(out.Problems) > 0 {
				fmt.Fprintln(os.Stdout, "\nproblems")
				for _, p := range out.Problems {
					fmt.Fprintf(os.Stdout, "  %s\n", p)
				}
			}
			return keyProblemsError(out.Problems)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

// keyProblemsError makes an unusable binding an exit status without printing twice.
//
// Same split as an unknown setting in `cm config`: a person is reading this, and a binding that does
// nothing is the question they came with, while anything holding a shell up carries on regardless.
func keyProblemsError(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return &exitCodeError{code: 1, reported: true}
}

// orDefault names the built-in key when the config sets none, so the output never has a blank where a
// live key is.
func orDefault(keys []string, fallback string) []string {
	if len(keys) > 0 {
		return keys
	}
	return []string{fallback}
}

func join(keys []string) string {
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}
