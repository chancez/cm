package spaces

import (
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
		{name: ".hidden"},
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
