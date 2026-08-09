package client

import (
	"reflect"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Options.Open must carry every session-shaping option onto the wire.
//
// The whole message is asserted rather than a few fields, because the bug this guards against is a
// field being *absent*: `attach --no-attach` built its own Open and did not list Tags, so tags were
// accepted, validated, and then silently dropped. Checking the fields someone remembered to check is
// exactly the test that would have passed while that was broken.
func TestOptionsOpenCarriesEveryField(t *testing.T) {
	opts := Options{
		Own:       true,
		ReadOnly:  true,
		Command:   []string{"/bin/zsh", "-l"},
		Dir:       "/home/user/projects",
		Env:       []string{"KEY=value"},
		ClientEnv: map[string]string{"TERM": "xterm-kitty"},
		Persist:   true,
		OnRestore: "command",
		Tags:      map[string]string{"project": "cm", "review": ""},
		NoRestore: true,
	}

	got := opts.Open("work")
	want := &serverv1.Open{
		Session:   "work",
		Own:       true,
		ReadOnly:  true,
		Command:   []string{"/bin/zsh", "-l"},
		Cwd:       "/home/user/projects",
		Env:       []string{"KEY=value"},
		ClientEnv: map[string]string{"TERM": "xterm-kitty"},
		Persist:   true,
		OnRestore: "command",
		Tags:      map[string]string{"project": "cm", "review": ""},
		NoRestore: true,
		// Rows, Cols, and ResumeFromSeq are deliberately not set here: only the caller knows a
		// terminal's size or where a reconnecting client left off.
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Options.Open() = %+v\nwant %+v", got, want)
	}
}

// Every field of Open that Options can describe must actually be set.
//
// The test above proves the fields it lists are carried; this one proves the list is complete. Without
// it, adding a field to both Options and Open and forgetting the mapping between them passes: the
// value is zero on each side, so a whole-message comparison of a fully populated Options still
// matches.
//
// Works by leaving Open's own fields at their zero values and checking which ones a fully populated
// Options fails to set. A field named here that Options has no counterpart for is listed as skipped,
// with the reason, so this fails when something new appears rather than silently tolerating it.
func TestOptionsOpenSetsEveryWireField(t *testing.T) {
	// Fields only the caller can supply, so Open leaves them zero on purpose.
	skip := map[string]string{
		"Rows":          "from the terminal, or a convention when there is none",
		"Cols":          "from the terminal, or a convention when there is none",
		"XPixel":        "from the terminal, alongside Rows and Cols",
		"YPixel":        "from the terminal, alongside Rows and Cols",
		"ResumeFromSeq": "only a reconnecting client knows where it left off",
		// Set per call site rather than from Options: `cm run` always captures so its output outlives
		// the command, while an interactive attach never should.
		"CaptureOutput": "decided by the command, not by the attachment",
	}

	// Every field set to something non-zero, so a field the mapping forgets stays zero and is caught.
	opts := Options{
		Own:       true,
		ReadOnly:  true,
		Command:   []string{"/bin/zsh"},
		Dir:       "/tmp",
		Env:       []string{"KEY=value"},
		ClientEnv: map[string]string{"TERM": "xterm"},
		Persist:   true,
		OnRestore: "shell",
		Tags:      map[string]string{"k": "v"},
		NoRestore: true,
	}
	open := opts.Open("work")

	v := reflect.ValueOf(open).Elem()
	typ := v.Type()
	for i := range typ.NumField() {
		field := typ.Field(i)
		// Skip protobuf's own bookkeeping fields, which are unexported or generated.
		if !field.IsExported() || field.Name == "state" || field.Name == "sizeCache" ||
			field.Name == "unknownFields" {
			continue
		}
		if _, ok := skip[field.Name]; ok {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("Open.%s is zero after Options.Open(); either map it from Options "+
				"or add it to the skip list with a reason", field.Name)
		}
	}
}
