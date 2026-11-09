package vt

import (
	"encoding/base64"
	"strings"
	"testing"

	"go.mitchellh.com/libghostty"

	"github.com/chancez/cm/internal/osc"
)

// cm's own program status parser must accept and reject exactly what libghostty does, with the same
// result.
//
// cm parses OSC 7501 in Go rather than through the model, because the session's reported state has to
// survive without one: the adoption replay recovers it with no emulator, and the server's own tests run
// without cgo. That makes two implementations of one specification, and this is what keeps them one.
// Lives here for the reason TestQuerySetsAgreeWithEmulator does: only this package can ask the emulator.
func TestStatusParserAgreesWithEmulator(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	raw := func(s string) string { return base64.RawStdEncoding.EncodeToString([]byte(s)) }

	payloads := []string{
		// The specification's own examples.
		"state=working:app=brew:msg=SW5zdGFsbGluZyB1cGRhdGVz",
		"state=blocked:kind=auth:app=brew:msg=UGFzc3dvcmQgcmVxdWlyZWQgdG8gaW5zdGFsbCB1cGRhdGVz",
		"state=done:app=deploy:msg=RGVwbG95ZWQgdG8gMyByZWdpb25z",
		"state=working:id=us-east:title=VVMgRWFzdA==:progress=40:msg=UHVzaGluZyBpbWFnZQ==",
		"state=clear:id=us-east",
		"state=clear",

		// Every state, and one that does not exist.
		"state=idle", "state=working", "state=done", "state=blocked", "state=error", "state=busy",
		"", "state=", "app=x",

		// Kind: only with blocked, unknown is absent.
		"state=blocked:kind=permission", "state=blocked:kind=question", "state=blocked:kind=nope",
		"state=working:kind=permission",

		// Progress: only with working or blocked, out of range or malformed is absent.
		"state=working:progress=0", "state=working:progress=100", "state=working:progress=101",
		"state=working:progress=-1", "state=working:progress=+5", "state=working:progress=007",
		"state=blocked:progress=50", "state=done:progress=50", "state=working:progress=",
		"state=working:progress=99999999999999999999",

		// ids: segments, depth, length, and characters.
		"state=idle:id=a/b/c", "state=idle:id=a//b", "state=idle:id=/a", "state=idle:id=a/",
		"state=idle:id=" + strings.Repeat("a/", 7) + "a", "state=idle:id=" + strings.Repeat("a/", 8) + "a",
		"state=idle:id=" + strings.Repeat("x", 32), "state=idle:id=" + strings.Repeat("x", 33),
		"state=idle:id=" + strings.Repeat(strings.Repeat("x", 31)+"/", 4) + "x",
		"state=idle:id=a,b", "state=idle:id=a=b", "state=idle:id=",

		// app: length and characters.
		"state=idle:app=" + strings.Repeat("x", 32), "state=idle:app=" + strings.Repeat("x", 33),
		"state=idle:app=a/b", "state=idle:app=", "state=idle:app=claude-code",

		// Text: padding optional, limits encoded and decoded, control characters, invalid base64.
		"state=idle:msg=" + b64("hi"), "state=idle:msg=" + raw("hi"), "state=idle:msg=" + raw("h"),
		"state=idle:msg=!!!!", "state=idle:msg=a", "state=idle:msg=" + b64("a\nb"),
		"state=idle:msg=" + b64("a\x7fb"), "state=idle:msg=" + b64("a\u0085b"), "state=idle:msg=" + b64("\xff"),
		"state=idle:msg=" + b64(strings.Repeat("x", 2048)), "state=idle:msg=" + b64(strings.Repeat("x", 2049)),
		"state=idle:title=" + b64(strings.Repeat("x", 192)), "state=idle:title=" + b64(strings.Repeat("x", 193)),
		"state=idle:msg=" + b64("caf\u00e9 \u202e"), "state=idle:title=" + b64("\t"),

		// Pairs: malformed skipped, unknown ignored, repeats last-wins, key limits.
		"state=idle:junk", "state=idle:=x", "state=idle:Key=x", "state=idle:" + strings.Repeat("k", 16) + "=x",
		"state=idle:" + strings.Repeat("k", 17) + "=x", "state=idle:state=done", "state=idle:app=a:app=b",
		"state=idle:app=a b", "state=idle:zz=1", "state=idle::", "state=idle:app=a;b",

		// The support query is not a report.
		"?",
	}
	// The whole sequence, introducer and ST included, at the 4096-byte limit and one past it. Padded with
	// an unknown key, which is ignored, so only the length decides.
	for _, total := range []int{4096, 4097} {
		head := "state=idle:zz="
		pad := total - len("\x1b]7501;") - len(head) - len("\x1b\\")
		payloads = append(payloads, head+strings.Repeat("x", pad))
	}

	// Through the tracker rather than ParseStatus, so the whole-sequence limit, which only the tracker
	// can see, is held to the emulator too.
	for _, p := range payloads {
		want, wantOK := emulatorStatus(t, p)
		var tr osc.StatusTracker
		tr.Feed([]byte("\x1b]7501;" + p + "\x1b\\"))
		got := tr.Take()
		gotOK := len(got) == 1
		if gotOK != wantOK || (gotOK && got[0] != want) || len(got) > 1 {
			t.Errorf("tracker on %q took %+v; the emulator says %+v, %v", p, got, want, wantOK)
		}
	}
}

// emulatorStatus asks libghostty what it makes of a program status payload.
func emulatorStatus(t *testing.T, payload string) (osc.Status, bool) {
	t.Helper()
	var got []libghostty.ProgramStatus
	term, err := libghostty.NewTerminal(
		libghostty.WithSize(80, 24),
		libghostty.WithProgramStatus(func(_ *libghostty.Terminal, s libghostty.ProgramStatus) {
			got = append(got, s)
		}),
	)
	if err != nil {
		t.Fatalf("NewTerminal() error = %v", err)
	}
	defer term.Close()
	term.VTWrite([]byte("\x1b]7501;" + payload + "\x1b\\"))

	switch len(got) {
	case 0:
		return osc.Status{}, false
	case 1:
	default:
		t.Fatalf("payload %q produced %d reports, want at most 1", payload, len(got))
	}
	s := got[0]
	return osc.Status{
		State: map[libghostty.ProgramStatusState]string{
			libghostty.ProgramStatusStateIdle:    "idle",
			libghostty.ProgramStatusStateWorking: "working",
			libghostty.ProgramStatusStateDone:    "done",
			libghostty.ProgramStatusStateBlocked: "blocked",
			libghostty.ProgramStatusStateError:   "error",
			libghostty.ProgramStatusStateClear:   "clear",
		}[s.State],
		Kind: map[libghostty.ProgramStatusKind]string{
			libghostty.ProgramStatusKindPermission: "permission",
			libghostty.ProgramStatusKindQuestion:   "question",
			libghostty.ProgramStatusKindAuth:       "auth",
		}[s.Kind],
		Progress: int(s.Progress),
		ID:       s.ID,
		App:      s.App,
		Title:    s.Title,
		Message:  s.Message,
	}, true
}
