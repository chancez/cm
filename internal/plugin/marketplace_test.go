// Tests for the Claude Code plugin marketplace this repo declares, which is how skills/cm reaches
// users who install it with `/plugin marketplace add chancez/cm`.
//
// Nothing in cm reads these manifests, so a mistake in one is invisible to every other test and to
// anyone working in the repo. The first symptom is someone else's `/plugin install` failing, or worse,
// succeeding while shipping the wrong files. `claude plugin validate` catches schema errors, but only
// where claude is installed, and it cannot know whether the paths point at what this repo intends.
// That consistency is what these tests assert.
package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// repoRoot returns the module root, since tests run in their own package directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// internal/plugin -> internal -> root
	return filepath.Join(wd, "..", "..")
}

type owner struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	URL   string `json:"url,omitempty"`
}

type marketplaceEntry struct {
	Name        string   `json:"name"`
	Source      string   `json:"source"`
	Description string   `json:"description"`
	Skills      []string `json:"skills,omitempty"`
	Version     string   `json:"version,omitempty"`
}

type marketplace struct {
	Schema      string             `json:"$schema"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Owner       owner              `json:"owner"`
	Plugins     []marketplaceEntry `json:"plugins"`
}

type pluginManifest struct {
	Schema      string   `json:"$schema"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Author      owner    `json:"author"`
	Homepage    string   `json:"homepage"`
	Repository  string   `json:"repository"`
	Keywords    []string `json:"keywords"`
	Version     string   `json:"version,omitempty"`
}

func readJSON[T any](t *testing.T, path string) T {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v T
	// DisallowUnknownFields is deliberately not used: the schema grows fields, and a test that fails
	// when a new optional one is added would punish the person adding it.
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return v
}

// TestMarketplaceManifest asserts the whole catalog, so a field silently dropped by an edit fails here
// rather than shipping. The install command in README.md is `/plugin install cm@cm`, which is
// `<plugin name>@<marketplace name>`: both names are public, so changing either breaks that line and
// every user's `enabledPlugins` key.
func TestMarketplaceManifest(t *testing.T) {
	got := readJSON[marketplace](t, filepath.Join(repoRoot(t), ".claude-plugin", "marketplace.json"))

	want := marketplace{
		Schema:      "https://json.schemastore.org/claude-code-marketplace.json",
		Name:        "cm",
		Description: "The cm skill: driving persistent terminal sessions and orchestrating agents in them.",
		Owner: owner{
			Name: "chancez",
			URL:  "https://github.com/chancez",
		},
		Plugins: []marketplaceEntry{{
			Name:        "cm",
			Source:      "./.claude-plugin/plugins/cm",
			Description: "Run and drive work in cm sessions: persistent terminal sessions that outlive the caller, each a real pty, so interactive programs and coding agents work in one.",
		}},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("marketplace.json = %+v\nwant %+v", got, want)
	}
}

// TestPluginManifest asserts the whole plugin manifest.
//
// Version is deliberately absent, and that is load-bearing rather than an omission. With no `version`
// in either manifest, Claude Code derives one from the plugin source's git commit SHA, so an edit to
// SKILL.md reaches installed users on the next update. Setting a version pins the plugin to that
// string: users would receive nothing until someone remembered to bump it, and a skill fix that no
// user receives is the failure mode this repo would hit, since cm's releases are tagged for the binary
// rather than for the skill. Verified: an install of this plugin reported version 53cde22afa7e.
func TestPluginManifest(t *testing.T) {
	got := readJSON[pluginManifest](t, filepath.Join(repoRoot(t), ".claude-plugin", "plugins", "cm", ".claude-plugin", "plugin.json"))

	want := pluginManifest{
		Schema:      "https://json.schemastore.org/claude-code-plugin.json",
		Name:        "cm",
		Description: "Run and drive work in cm sessions: persistent terminal sessions that outlive the caller, each a real pty, so interactive programs and coding agents work in one.",
		Author: owner{
			Name: "chancez",
			URL:  "https://github.com/chancez",
		},
		Homepage:   "https://github.com/chancez/cm",
		Repository: "https://github.com/chancez/cm",
		Keywords:   []string{"terminal", "multiplexer", "sessions", "pty", "orchestration"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("plugin.json = %+v\nwant %+v", got, want)
	}
}

// TestPluginSkillsResolve is the test that would catch the mistake most likely to happen: the plugin
// directory holds skills/cm as a *symlink* into the repo's single skills/cm, so there is one SKILL.md
// to edit rather than two that drift. A rename or a move on either side leaves a dangling link, which
// installs cleanly as a plugin with no skills in it.
//
// os.Stat rather than Lstat, on purpose: it follows the link, so a broken one fails here. Claude Code
// dereferences symlinks when it copies a plugin into its cache, so what Stat sees is what a user gets.
func TestPluginSkillsResolve(t *testing.T) {
	root := repoRoot(t)

	// Every plugin in the catalog, so adding a second one does not escape this check.
	mp := readJSON[marketplace](t, filepath.Join(root, ".claude-plugin", "marketplace.json"))
	for _, entry := range mp.Plugins {
		pluginDir := filepath.Join(root, filepath.FromSlash(entry.Source))

		// The default scan: skills/<name>/SKILL.md under the plugin root. This repo declares no custom
		// `skills` paths, so an empty skills/ directory means the plugin ships nothing.
		skillsDir := filepath.Join(pluginDir, "skills")
		names, err := os.ReadDir(skillsDir)
		if err != nil {
			t.Fatalf("plugin %s: read skills dir: %v", entry.Name, err)
		}
		if len(names) == 0 {
			t.Fatalf("plugin %s: skills/ is empty, so the plugin would install with no skills", entry.Name)
		}

		for _, name := range names {
			skill := filepath.Join(skillsDir, name.Name(), "SKILL.md")
			info, err := os.Stat(skill)
			if err != nil {
				t.Errorf("plugin %s: %s does not resolve, so the installed plugin would have no skill: %v", entry.Name, skill, err)
				continue
			}
			if info.Size() == 0 {
				t.Errorf("plugin %s: %s is empty", entry.Name, skill)
			}
		}
	}
}
