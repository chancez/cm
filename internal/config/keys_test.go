package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/keymap"
)

// A file with no [keys] leaves every default in place, which is what makes this setting safe to add: an
// existing config keeps the keys its user already has.
func TestNoKeysSectionLeavesTheDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "scrollback_lines = 100\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	m, problems := cfg.Keymap(keymap.Overlay)
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none", problems)
	}
	if got, want := m.Names(keymap.OverlayNext), "n"; got != want {
		t.Errorf("next is bound to %q, want the default %q", got, want)
	}
	if cfg.DetachKeys() != nil {
		t.Errorf("DetachKeys() = %v, want nil so the caller's default is used", cfg.DetachKeys())
	}
}

// Both spellings of a key setting decode, because detach_key has always been a bare string and a
// list-only [keys.session] would make the same setting look different for no reason.
func TestAKeySettingTakesAStringOrAList(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[keys.session]
detach = "ctrl-q"

[keys.overlay]
next = "j"
previous = ["P", "ctrl-y"]
`))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got, want := cfg.DetachKeys(), []string{"ctrl-q"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DetachKeys() = %v, want %v", got, want)
	}
	m, problems := cfg.Keymap(keymap.Overlay)
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none", problems)
	}
	if got, want := m.Names(keymap.OverlayNext), "j"; got != want {
		t.Errorf("next is bound to %q, want %q", got, want)
	}
	if got, want := m.Names(keymap.OverlayPrevious), "P, ctrl-y"; got != want {
		t.Errorf("previous is bound to %q, want %q", got, want)
	}
}

// A wrong type is a problem rather than a parse failure that takes the whole file with it. Every other
// mistake in a binding is survivable and this one has no reason not to be.
func TestAKeySettingOfTheWrongTypeIsReportedNotFatal(t *testing.T) {
	_, err := Load(writeConfig(t, "[keys.session]\ndetach = 42\n"))
	if err == nil {
		t.Fatal("Load() error = nil, want a message naming the setting")
	}
	if !strings.Contains(err.Error(), "string or a list of strings") {
		t.Errorf("Load() error = %v, want it to say what a key setting may be", err)
	}
}

// The session table wins where a file sets both spellings, and the old one still works on its own: every
// existing config uses detach_key, and an upgrade that ignored it would take away the only way some people
// have of leaving a session.
func TestTheOlderKeySettingsStillWork(t *testing.T) {
	both, err := Load(writeConfig(t, "detach_key = \"ctrl-q\"\n[keys.session]\ndetach = [\"ctrl-g\"]\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := both.DetachKeys(), []string{"ctrl-g"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DetachKeys() = %v, want %v: the list wins", got, want)
	}

	old, err := Load(writeConfig(t, "detach_key = \"ctrl-q\"\nprefix_key = \"ctrl-o\"\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got, want := old.DetachKeys(), []string{"ctrl-q"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DetachKeys() = %v, want %v", got, want)
	}
	if got, want := old.PrefixKeys(), []string{"ctrl-o"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PrefixKeys() = %v, want %v", got, want)
	}
}

// An empty list unbinds, and it has to survive decoding as an empty list rather than as nothing: a nil
// list means the setting is absent, which means the default.
func TestAnEmptyListReachesTheKeymapAsUnbound(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[keys.overlay]\nkill = []\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	m, problems := cfg.Keymap(keymap.Overlay)
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none", problems)
	}
	if m.Bound(keymap.OverlayKill) {
		t.Errorf("kill is bound to %q, want nothing", m.Names(keymap.OverlayKill))
	}
}

// An action name this build does not know is reported and ignored, never fatal. One config file serves
// every build on a machine, which is the incident UnknownSettings records.
func TestAnUnknownActionIsReportedAndIgnored(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[keys.tui]\nteleport = [\"z\"]\n"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	m, problems := cfg.Keymap(keymap.TUI)
	if len(problems) != 1 || !strings.Contains(problems[0].String(), "keys.tui.teleport") {
		t.Errorf("problems = %v, want one naming keys.tui.teleport", problems)
	}
	if got, want := m.Names(keymap.TUIAttach), "enter"; got != want {
		t.Errorf("attach is bound to %q, want %q: the rest of the table still applies", got, want)
	}
}
