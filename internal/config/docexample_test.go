package config

import (
	"os"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/keymap"
)

// The example in docs/config.md has to load as itself, with every setting landing where it is
// documented to land.
//
// The trap this guards is TOML's, not cm's: a bare key belongs to whatever table header precedes it,
// so a top-level setting written after [overlay] is read as overlay.log_level. That is an unknown
// setting, which cm warns about and ignores, so somebody who copied the example got the default for
// log_level, both retentions, rebind_replaces, runtime_dir and state_dir and nothing saying why. The
// example had drifted into exactly that shape, one table header at a time, and reading it does not
// show the problem: the file looks like a list of settings with comments.
func TestTheDocumentedExampleLoadsAsItself(t *testing.T) {
	cfg, err := Load(writeConfig(t, exampleFromDocs(t)))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.UnknownSettings(); len(got) != 0 {
		t.Errorf("unknown settings %v: a setting the example documents is being read under a table", got)
	}

	for _, ctx := range []keymap.Context{keymap.Session, keymap.Overlay, keymap.TUI} {
		if _, problems := cfg.Keymap(ctx); len(problems) != 0 {
			t.Errorf("%s keys: problems = %v, want none", ctx, problems)
		}
	}
}

// exampleFromDocs returns the toml block under "## Example" in docs/config.md.
//
// Only that one: later blocks in the file are fragments showing one setting, and a fragment is not
// expected to stand on its own.
func exampleFromDocs(t *testing.T) string {
	t.Helper()
	const path = "../../docs/config.md"
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	_, after, found := strings.Cut(string(doc), "## Example")
	if !found {
		t.Fatalf("%s has no \"## Example\" heading, so this test is reading the wrong thing", path)
	}
	_, after, found = strings.Cut(after, "```toml\n")
	if !found {
		t.Fatalf("%s has no toml block under \"## Example\"", path)
	}
	block, _, found := strings.Cut(after, "```")
	if !found {
		t.Fatalf("%s has an unterminated toml block under \"## Example\"", path)
	}
	return block
}
