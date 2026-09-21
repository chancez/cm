package client

import (
	"testing"
	"time"

	"github.com/chancez/cm/internal/keymap"
)

// sessionGate builds a gate with detach and prefix at their defaults plus one bound verb.
func sessionGate(t *testing.T, action keymap.Action, specs ...string) *inputGate {
	t.Helper()
	return &inputGate{
		detach:  keys(t, DefaultDetachKey),
		prefix:  keys(t, DefaultPrefixKey),
		actions: []gateActionKeys{{Action: action, Keys: keys(t, specs...)}},
	}
}

// A verb bound in the session performs without the prefix, and says which action it wants.
func TestASessionKeyAsksForItsAction(t *testing.T) {
	g := sessionGate(t, keymap.OverlayKill, "f5")

	got := g.feed([]byte("\x1b[15~"), time.Now())
	if got.Action != gateOverlayAction {
		t.Fatalf("action = %v, want gateOverlayAction", got.Action)
	}
	if got.Do != keymap.OverlayKill {
		t.Errorf("Do = %q, want kill", got.Do)
	}
	if got.Key.Name != "f5" {
		t.Errorf("Key = %q, want f5: a message about the press has to name it", got.Key.Name)
	}
}

// Typing before the key goes to the program first, in the order it was typed. The overlay opening is not a
// reason for the shell to lose what was already on the line.
func TestASessionKeyForwardsWhatPrecededIt(t *testing.T) {
	g := sessionGate(t, keymap.OverlayNext, "ctrl-o")

	got := g.feed([]byte("ls\x0f"), time.Now())
	if string(got.Forward) != "ls" {
		t.Errorf("forwarded %q, want the typing that preceded the key", got.Forward)
	}
	if got.Do != keymap.OverlayNext {
		t.Errorf("Do = %q, want next", got.Do)
	}
}

// detach and prefix are matched before the verbs, which is the order the keymap lists them in and so the
// order a collision resolves in everywhere else.
func TestDetachAndPrefixBeatASessionVerb(t *testing.T) {
	g := sessionGate(t, keymap.OverlayKill, `ctrl-\`)
	if got := g.feed([]byte("\x1c"), time.Now()); got.Action != gateDetach {
		t.Errorf("action = %v, want the detach: it is matched first", got.Action)
	}

	g = sessionGate(t, keymap.OverlayKill, "ctrl-]")
	if got := g.feed([]byte("\x1d"), time.Now()); got.Action != gatePrefix {
		t.Errorf("action = %v, want the prefix", got.Action)
	}
}

// A nested client gets every intercepted key, session verbs included. Without that an outer window would
// take a key from the session the user is actually looking at, which is what the handover exists to prevent.
func TestASessionKeyIsSuspendedWhileNested(t *testing.T) {
	g := sessionGate(t, keymap.OverlayKill, "f5")
	g.setNesting(true, 1)

	got := g.feed([]byte("\x1b[15~"), time.Now())
	if got.Action != gateNone {
		t.Errorf("action = %v while nested, want the key forwarded to the inner client", got.Action)
	}
	if string(got.Forward) != "\x1b[15~" {
		t.Errorf("forwarded %q, want the whole sequence", got.Forward)
	}
}

// A partial encoding of a session key is held back like any other intercepted key, so the press is still
// recognized when its bytes arrive in two reads.
func TestASessionKeyWidensTheHoldback(t *testing.T) {
	g := sessionGate(t, keymap.OverlayKill, "ctrl-q")

	got := g.feed([]byte("ls\x1b[11"), time.Now())
	if string(got.Forward) != "ls" {
		t.Errorf("forwarded %q, want the partial sequence held", got.Forward)
	}
	got = g.feed([]byte("3;5u"), time.Now())
	if got.Do != keymap.OverlayKill {
		t.Errorf("Do = %q after the sequence completed, want kill", got.Do)
	}
}

// Nothing is bound in the session by default except the two keys that always were, since every one of these
// is a key taken from every program in every session.
func TestNothingIsBoundInTheSessionByDefault(t *testing.T) {
	m, problems := keymap.Build(keymap.Session, nil)
	if len(problems) != 0 {
		t.Fatalf("defaults have problems: %v", problems)
	}
	for _, def := range m.Actions() {
		bound := m.Bound(def.Action)
		want := def.Action == keymap.SessionPrefix || def.Action == keymap.OverlayDetach
		if bound != want {
			t.Errorf("%s bound = %v, want %v", def.Action, bound, want)
		}
	}
}

// The chooser's keys cannot be session keys, and saying so beats a key that could never do anything: up and
// down act on a list that nothing has opened yet.
func TestTheChoosersKeysAreNotSessionActions(t *testing.T) {
	_, problems := keymap.Build(keymap.Session, map[string][]string{"down": {"ctrl-o"}})
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want one saying down is not a session action", problems)
	}
	if got := problems[0].Setting; got != "keys.session.down" {
		t.Errorf("problem names %q, want keys.session.down", got)
	}
}
