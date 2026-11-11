package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// TestSearchTUIOpensThePickerOnTheQuery checks the handoff carries what was searched for, since a picker
// opened without it is `cm tui` and looks like the flag being ignored.
func TestSearchTUIOpensThePickerOnTheQuery(t *testing.T) {
	picker, err := filteredTUICommand(&globals{}, "work vim", []string{"project=cm", "role"})
	if err != nil {
		t.Fatal(err)
	}
	tagArgs, err := picker.Flags().GetStringArray("tag")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{
		"filter": picker.Flags().Lookup("filter").Value.String(),
		"tag":    tagArgs,
	}
	want := map[string]any{
		"filter": "work vim",
		"tag":    []string{"project=cm", "role"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("picker flags = %v, want %v", got, want)
	}
}

// TestSearchOutputKeepsTheRanking guards the order: the list printers sort by age, which for a search
// would put the best match wherever its creation time lands.
func TestSearchOutputKeepsTheRanking(t *testing.T) {
	newer := &serverv1.Session{Name: "worker", Id: "a7k2m9x4", CreatedAtUnix: 200}
	older := &serverv1.Session{Name: "notes", Id: "b8l3n0y5", CreatedAtUnix: 100}

	var buf bytes.Buffer
	if err := writeSessionsJSON(&buf, []*serverv1.Session{newer, older}); err != nil {
		t.Fatal(err)
	}
	// Compared as encoded bytes: decoding gives CreatedAt a different *time.Location than
	// toSessionJSON's time.Unix, which reflect.DeepEqual rejects wherever Local is UTC, the Linux image.
	var want bytes.Buffer
	if err := writeJSON(&want, []sessionJSON{toSessionJSON(newer), toSessionJSON(older)}); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != want.String() {
		t.Errorf("JSON order = %s, want %s", got, want.String())
	}

	buf.Reset()
	if err := writeSessionsTable(&buf, []*serverv1.Session{newer, older}, 200); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n")[1:] {
		names = append(names, strings.Fields(line)[0])
	}
	if want := []string{"worker", "notes"}; !reflect.DeepEqual(names, want) {
		t.Errorf("table order = %q, want %q", names, want)
	}
}
