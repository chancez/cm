package spaces

import (
	"slices"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{name: "default", ok: true},
		{name: "cm", ok: true},
		{name: "infra-2_x.y", ok: true},
		{name: strings.Repeat("a", MaxNameLen), ok: true},
		{name: strings.Repeat("a", MaxNameLen+1)},
		{name: ""},
		{name: "."},
		{name: ".."},
		// A repository's directory, as kspaces names a space after it.
		{name: ".dotfiles", ok: true},
		{name: "host:cm"},
		{name: "a b"},
		{name: "a/b"},
		{name: "@a7k2m9x4"},
		{name: "\x1b]0;x\a"},
	} {
		err := ValidateName(tc.name)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestCompareGroupsDefaultFirst(t *testing.T) {
	got := []string{"zeta", "", "alpha", "default", "cm"}
	slices.SortStableFunc(got, Compare)
	want := []string{"", "default", "alpha", "cm", "zeta"}
	if !slices.Equal(got, want) {
		t.Errorf("sorted = %q, want %q", got, want)
	}
}

func TestValidateMetaKey(t *testing.T) {
	for _, tc := range []struct {
		key string
		ok  bool
	}{
		{key: "kspaces/layout/main", ok: true},
		{key: "kspaces/layout/.dotfiles", ok: true},
		{key: "note", ok: true},
		{key: strings.Repeat("a", MaxMetaKeyLen), ok: true},
		{key: strings.Repeat("a", MaxMetaKeyLen+1)},
		{key: ""},
		{key: "a b"},
		{key: "host:key"},
		{key: "\x1b]0;x\a"},
	} {
		err := ValidateMetaKey(tc.key)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateMetaKey(%q) = %v, want ok=%v", tc.key, err, tc.ok)
		}
	}
}
