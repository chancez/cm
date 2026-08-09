package tags

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name      string
		arg       string
		wantKey   string
		wantValue string
		wantErr   bool
	}{
		{name: "key and value", arg: "project=cm", wantKey: "project", wantValue: "cm"},
		{name: "bare key", arg: "review", wantKey: "review"},
		// A trailing '=' is the same as no value, so the two spellings cannot store different
		// things that display identically.
		{name: "trailing equals is a bare key", arg: "review=", wantKey: "review"},
		{name: "namespaced key", arg: "cm.dev/run=abc123", wantKey: "cm.dev/run", wantValue: "abc123"},
		{name: "dots and dashes", arg: "a.b-c_d=1.2-3_4", wantKey: "a.b-c_d", wantValue: "1.2-3_4"},

		{name: "empty", arg: "", wantErr: true},
		{name: "empty key with value", arg: "=cm", wantErr: true},
		// Rejected rather than split, so the caller learns instead of storing "b=c".
		{name: "second equals", arg: "a=b=c", wantErr: true},
		{name: "space in value", arg: "k=a b", wantErr: true},
		{name: "comma in value", arg: "k=a,b", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, value, err := Parse(tc.arg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = (%q, %q, nil), want an error", tc.arg, key, value)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tc.arg, err)
			}
			if key != tc.wantKey || value != tc.wantValue {
				t.Errorf("Parse(%q) = (%q, %q), want (%q, %q)",
					tc.arg, key, value, tc.wantKey, tc.wantValue)
			}
		})
	}
}

// An escape sequence in a tag must be refused at the boundary.
//
// This is the reason the character set is narrow rather than a matter of tidiness. Tags are printed
// straight to a terminal by `cm list`, so a value carrying an escape sequence would let whoever
// created a session repaint or retitle the terminal of whoever lists them.
func TestValidateRejectsEscapeSequences(t *testing.T) {
	hostile := []string{
		"\x1b]2;pwned\x07",      // OSC, retitles the window
		"\x1b[2J",               // CSI, clears the screen
		"a\rb",                  // carriage return, overwrites what was printed
		"a\nb",                  // newline, breaks the table
		"a\tb",                  // tab, breaks tabwriter's columns
		"\x00",                  // NUL
		"\x1b]25453;busy\x1b\\", // cm's own report sequence
	}

	for _, v := range hostile {
		if err := ValidateValue(v); err == nil {
			t.Errorf("ValidateValue(%q) = nil, want an error", v)
		}
		if err := ValidateKey(v); err == nil {
			t.Errorf("ValidateKey(%q) = nil, want an error", v)
		}
		// And through the parsing entry point, which is what the CLI actually calls.
		if _, _, err := Parse("k=" + v); err == nil {
			t.Errorf("Parse(%q) = nil error, want an error", "k="+v)
		}
	}
}

func TestValidateLengthLimits(t *testing.T) {
	atLimit := strings.Repeat("a", 63)
	overLimit := strings.Repeat("a", 64)

	if err := ValidateKey(atLimit); err != nil {
		t.Errorf("ValidateKey(63 bytes) error = %v, want nil", err)
	}
	if err := ValidateKey(overLimit); err == nil {
		t.Error("ValidateKey(64 bytes) = nil, want an error")
	}
	if err := ValidateValue(atLimit); err != nil {
		t.Errorf("ValidateValue(63 bytes) error = %v, want nil", err)
	}
	if err := ValidateValue(overLimit); err == nil {
		t.Error("ValidateValue(64 bytes) = nil, want an error")
	}
	// An empty value is legal: a bare key is a useful thing to say on its own.
	if err := ValidateValue(""); err != nil {
		t.Errorf("ValidateValue(\"\") error = %v, want nil", err)
	}
	if err := ValidateKey(""); err == nil {
		t.Error("ValidateKey(\"\") = nil, want an error")
	}
}

func TestParseAll(t *testing.T) {
	got, err := ParseAll([]string{"project=cm", "review", "role=reviewer"})
	if err != nil {
		t.Fatalf("ParseAll() error = %v", err)
	}
	want := map[string]string{"project": "cm", "review": "", "role": "reviewer"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseAll() = %v, want %v", got, want)
	}

	// A repeated flag is expected to have the last one win.
	got, err = ParseAll([]string{"project=old", "project=new"})
	if err != nil {
		t.Fatalf("ParseAll() error = %v", err)
	}
	want = map[string]string{"project": "new"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseAll() with a repeated key = %v, want %v", got, want)
	}

	// Nil rather than an empty map, so a caller can tell "no tags given" from "tags cleared".
	if got, err := ParseAll(nil); got != nil || err != nil {
		t.Errorf("ParseAll(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestSelectorMatch(t *testing.T) {
	sessionTags := map[string]string{"project": "cm", "role": "reviewer", "review": ""}

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "no terms matches anything", args: nil, want: true},
		{name: "key and value", args: []string{"project=cm"}, want: true},
		{name: "wrong value", args: []string{"project=zmx"}, want: false},
		{name: "missing key", args: []string{"absent"}, want: false},
		// A bare key matches whatever the value is, which is what makes "belongs to some project"
		// expressible.
		{name: "bare key against a value", args: []string{"project"}, want: true},
		{name: "bare key against an empty value", args: []string{"review"}, want: true},
		// "key=" is the bare key, so it must not select only the empty-valued ones.
		{name: "trailing equals is the bare key", args: []string{"project="}, want: true},
		// Repeating narrows: every term has to match.
		{name: "two terms both match", args: []string{"project=cm", "role=reviewer"}, want: true},
		{name: "two terms one fails", args: []string{"project=cm", "role=builder"}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := ParseSelector(tc.args)
			if err != nil {
				t.Fatalf("ParseSelector(%v) error = %v", tc.args, err)
			}
			if got := sel.Match(sessionTags); got != tc.want {
				t.Errorf("ParseSelector(%v).Match(%v) = %v, want %v",
					tc.args, sessionTags, got, tc.want)
			}
		})
	}
}

// A selector with terms must not match an untagged session, and an empty one must.
func TestSelectorAgainstNoTags(t *testing.T) {
	sel, err := ParseSelector([]string{"project=cm"})
	if err != nil {
		t.Fatalf("ParseSelector() error = %v", err)
	}
	if sel.Match(nil) {
		t.Error("a selector with terms matched an untagged session, want no match")
	}
	if sel.Empty() {
		t.Error("Empty() = true for a selector with a term")
	}

	empty, err := ParseSelector(nil)
	if err != nil {
		t.Fatalf("ParseSelector(nil) error = %v", err)
	}
	if !empty.Match(nil) {
		t.Error("an empty selector did not match an untagged session, want a match")
	}
	if !empty.Empty() {
		t.Error("Empty() = false for a selector with no terms")
	}
}

func TestFormat(t *testing.T) {
	tests := []struct {
		name string
		tags map[string]string
		want string
	}{
		{name: "nil", tags: nil, want: ""},
		{name: "one pair", tags: map[string]string{"project": "cm"}, want: "project=cm"},
		{name: "bare key prints alone", tags: map[string]string{"review": ""}, want: "review"},
		{
			// Sorted, because a map iterates randomly and a table whose columns reshuffle between
			// calls cannot be read.
			name: "sorted by key",
			tags: map[string]string{"role": "reviewer", "project": "cm", "review": ""},
			want: "project=cm,review,role=reviewer",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Format(tc.tags); got != tc.want {
				t.Errorf("Format(%v) = %q, want %q", tc.tags, got, tc.want)
			}
		})
	}
}

// Format must be stable across calls, since Go randomizes map iteration.
func TestFormatIsStable(t *testing.T) {
	in := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}
	first := Format(in)
	for range 20 {
		if got := Format(in); got != first {
			t.Fatalf("Format() = %q on a later call, want %q every time", got, first)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(map[string]string{"project": "cm", "review": ""}); err != nil {
		t.Errorf("Validate() error = %v, want nil", err)
	}
	if err := Validate(map[string]string{"bad key": "cm"}); err == nil {
		t.Error("Validate() with a bad key = nil, want an error")
	}
	if err := Validate(map[string]string{"project": "a b"}); err == nil {
		t.Error("Validate() with a bad value = nil, want an error")
	}
	// The error names which tag was wrong, since a caller passing several needs to know.
	err := Validate(map[string]string{"project": "a b"})
	if err != nil && !strings.Contains(err.Error(), "project") {
		t.Errorf("Validate() error = %q, want it to name the offending key", err)
	}
}
