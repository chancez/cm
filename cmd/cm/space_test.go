package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func TestSpaceCandidates(t *testing.T) {
	all := []*serverv1.Space{
		{Name: "cm", Sessions: 3, Running: 2},
		{Name: "default", Sessions: 1, Running: 1},
		{Name: "infra"},
	}
	if got, want := spaceCandidates(all, "", nil), []string{
		"cm\t3 sessions, 2 running", "default\t1 sessions, 1 running", "infra\t0 sessions, 0 running",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("all = %q, want %q", got, want)
	}
	if got, want := spaceCandidates(all, "c", nil), []string{"cm\t3 sessions, 2 running"}; !reflect.DeepEqual(got, want) {
		t.Errorf("prefix c = %q, want %q", got, want)
	}
	// remove offers only what it accepts: empty, and not default.
	if got, want := spaceCandidates(all, "", removableSpace), []string{"infra\t0 sessions, 0 running"}; !reflect.DeepEqual(got, want) {
		t.Errorf("removable = %q, want %q", got, want)
	}
}

func TestMetaValue(t *testing.T) {
	file := filepath.Join(t.TempDir(), "v")
	if err := os.WriteFile(file, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		args    []string
		file    string
		want    string
		wantErr bool
	}{
		{name: "argument", args: []string{"inline"}, want: "inline"},
		{name: "file", file: file, want: "from file"},
		{name: "stdin", file: "-", want: "from stdin"},
		{name: "both", args: []string{"inline"}, file: file, wantErr: true},
		// Neither, which must not fall back to reading a terminal.
		{name: "neither", wantErr: true},
	} {
		got, err := metaValue(tc.args, tc.file, strings.NewReader("from stdin"))
		if (err != nil) != tc.wantErr || string(got) != tc.want {
			t.Errorf("%s: metaValue() = %q, %v; want %q, error %v", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

// A session created on another host from inside one here takes the space of the one here, which the far host
// cannot look up by its reference: by any of its names or its ID, and nothing for default or a session not
// listed.
func TestSessionSpace(t *testing.T) {
	sessions := []*serverv1.Session{
		{Id: "a1", Name: "work", Names: []string{"kitty.5"}, Space: "homelab"},
		{Id: "b2", Name: "plain", Space: "default"},
	}
	got := map[string]string{}
	for _, ref := range []string{"work", "@a1", "kitty.5", "plain", "gone"} {
		got[ref] = sessionSpace(sessions, ref)
	}
	want := map[string]string{"work": "homelab", "@a1": "homelab", "kitty.5": "homelab", "plain": "", "gone": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionSpace = %v, want %v", got, want)
	}
}
