package graphics

import (
	"strings"
	"testing"
)

// The three probes below are the real bytes `kitten icat` sent through a cm session, taken from a
// `cm read --raw --follow` capture. Using the capture rather than invented commands matters here: the
// medium keys are the whole reason this package exists, and a hand-written example would have been
// written from the same misunderstanding that caused the bug.
const (
	probeDirect    = "\x1b_Ga=q,f=24,s=1,v=1,S=3,i=1;MTIz\x1b\\"
	probeTempFile  = "\x1b_Ga=q,f=24,t=t,s=1,v=1,S=87,i=2;L3Zhci9mb2xkZXJz\x1b\\"
	probeSharedMem = "\x1b_Ga=q,f=24,t=s,s=1,v=1,S=18,i=3;aWNhdC1IMkZUQ0ZaTEJRQ0Y2\x1b\\"
	// The transmission that actually carried the image, PNG and chunked.
	realTransmit = "\x1b_Ga=T,q=2,f=100,m=1,s=1712,v=1294;iVBORw0KGgo\x1b\\"
)

func TestParseProbes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want Command
	}{
		{
			name: "direct",
			in:   probeDirect,
			want: Command{
				Action: ActionQuery, Medium: MediumDirect, ImageID: 1,
				Control: "a=q,f=24,s=1,v=1,S=3,i=1",
				Payload: []byte("MTIz"),
				Raw:     []byte(probeDirect),
			},
		},
		{
			name: "tempfile",
			in:   probeTempFile,
			want: Command{
				Action: ActionQuery, Medium: MediumTempFile, ImageID: 2,
				Control: "a=q,f=24,t=t,s=1,v=1,S=87,i=2",
				Payload: []byte("L3Zhci9mb2xkZXJz"),
				Raw:     []byte(probeTempFile),
			},
		},
		{
			name: "sharedmem",
			in:   probeSharedMem,
			want: Command{
				Action: ActionQuery, Medium: MediumSharedMemory, ImageID: 3,
				Control: "a=q,f=24,t=s,s=1,v=1,S=18,i=3",
				Payload: []byte("aWNhdC1IMkZUQ0ZaTEJRQ0Y2"),
				Raw:     []byte(probeSharedMem),
			},
		},
		{
			name: "transmit",
			in:   realTransmit,
			want: Command{
				Action: ActionTransmitAndDisplay, Medium: MediumDirect, Quiet: 2, More: true,
				Control: "a=T,q=2,f=100,m=1,s=1712,v=1294",
				Payload: []byte("iVBORw0KGgo"),
				Raw:     []byte(realTransmit),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n, ok := Parse([]byte(tc.in))
			if !ok {
				t.Fatalf("Parse() ok = false, want a complete command")
			}
			if n != len(tc.in) {
				t.Errorf("Parse() consumed %d bytes, want %d", n, len(tc.in))
			}
			if got.Action != tc.want.Action {
				t.Errorf("Action = %q, want %q", got.Action, tc.want.Action)
			}
			if got.Medium != tc.want.Medium {
				t.Errorf("Medium = %q, want %q", got.Medium, tc.want.Medium)
			}
			if got.ImageID != tc.want.ImageID {
				t.Errorf("ImageID = %d, want %d", got.ImageID, tc.want.ImageID)
			}
			if got.Quiet != tc.want.Quiet {
				t.Errorf("Quiet = %d, want %d", got.Quiet, tc.want.Quiet)
			}
			if got.More != tc.want.More {
				t.Errorf("More = %v, want %v", got.More, tc.want.More)
			}
			if got.Control != tc.want.Control {
				t.Errorf("Control = %q, want %q", got.Control, tc.want.Control)
			}
			if string(got.Payload) != string(tc.want.Payload) {
				t.Errorf("Payload = %q, want %q", got.Payload, tc.want.Payload)
			}
			if string(got.Raw) != string(tc.want.Raw) {
				t.Errorf("Raw = %q, want %q", got.Raw, tc.want.Raw)
			}
		})
	}
}

// The two probes that failed under cm are exactly the two naming a file, and that has to be visible
// from a parsed command without re-reading the control string.
func TestNeedsFileIdentifiesTheFailingMedia(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"direct carries its data", probeDirect, false},
		{"tempfile names a file", probeTempFile, true},
		{"sharedmem names an object", probeSharedMem, true},
		{"transmit inline", realTransmit, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, ok := Parse([]byte(tc.in))
			if !ok {
				t.Fatal("Parse() ok = false")
			}
			if got := cmd.Medium.NeedsFile(); got != tc.want {
				t.Errorf("Medium(%q).NeedsFile() = %v, want %v", cmd.Medium, got, tc.want)
			}
		})
	}
}

// A command split across chunks must be reported as incomplete rather than malformed, because a pty
// read is capped at 1022 bytes on darwin whatever buffer is passed, so any real image transmission is
// guaranteed to arrive in pieces.
func TestParseIncompleteIsHeldNotDropped(t *testing.T) {
	full := probeDirect
	for i := 1; i < len(full); i++ {
		cmd, n, ok := Parse([]byte(full[:i]))
		if ok {
			t.Fatalf("Parse(%q) reported a complete command from a prefix", full[:i])
		}
		if n == 0 {
			t.Errorf("Parse(%q) consumed 0 bytes, so a caller would treat the fragment as "+
				"ordinary output and fail to recognize the rest", full[:i])
		}
		if cmd.Raw != nil {
			t.Errorf("Parse(%q) returned Raw on an incomplete command", full[:i])
		}
	}
}

// Bytes that are not a graphics command at all must be reported as such, so the scan can skip them
// cheaply rather than buffering ordinary output forever.
func TestParseRejectsNonGraphics(t *testing.T) {
	for _, in := range []string{
		"plain text",
		"\x1b[1;2H",             // a CSI
		"\x1b]133;A\x07",        // an OSC
		"\x1bP+q544e\x1b\\",     // a DCS
		"\x1b_Xsomething\x1b\\", // APC, but not graphics
	} {
		_, n, ok := Parse([]byte(in))
		if ok {
			t.Errorf("Parse(%q) reported a graphics command", in)
		}
		if n != 0 {
			t.Errorf("Parse(%q) consumed %d bytes, want 0 so the caller can skip it", in, n)
		}
	}
}

// A trailing fragment of the introducer has to be held, since the bytes completing it are in the next
// chunk.
func TestParseHoldsATrailingIntroducerFragment(t *testing.T) {
	for _, in := range []string{"\x1b", "\x1b_"} {
		_, n, ok := Parse([]byte(in))
		if ok {
			t.Errorf("Parse(%q) reported a complete command", in)
		}
		if n != len(in) {
			t.Errorf("Parse(%q) consumed %d, want %d so the fragment is buffered rather than emitted",
				in, n, len(in))
		}
	}
}

// BEL terminates an APC as well as ST, and programs use both.
func TestParseAcceptsBothTerminators(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"st", "\x1b_Ga=q,i=1;MTIz\x1b\\"},
		{"bel", "\x1b_Ga=q,i=1;MTIz\x07"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, n, ok := Parse([]byte(tc.in))
			if !ok {
				t.Fatalf("Parse() ok = false for a %s-terminated command", tc.name)
			}
			if n != len(tc.in) {
				t.Errorf("consumed %d bytes, want %d", n, len(tc.in))
			}
			if string(cmd.Payload) != "MTIz" {
				t.Errorf("Payload = %q, want %q", cmd.Payload, "MTIz")
			}
		})
	}
}

// Absent keys have to mean the protocol's defaults, not Go's zero values. a= defaults to T and t= to
// direct, so a bare transmission is a display and carries its own data.
func TestParseAppliesProtocolDefaults(t *testing.T) {
	cmd, _, ok := Parse([]byte("\x1b_Gi=9;MTIz\x1b\\"))
	if !ok {
		t.Fatal("Parse() ok = false")
	}
	if cmd.Action != ActionTransmitAndDisplay {
		t.Errorf("Action = %q, want %q for an absent a= key", cmd.Action, ActionTransmitAndDisplay)
	}
	if cmd.Medium != MediumDirect {
		t.Errorf("Medium = %q, want %q for an absent t= key", cmd.Medium, MediumDirect)
	}
	if cmd.Medium.NeedsFile() {
		t.Error("a command with no medium must not be treated as naming a file")
	}
}

// An image is addressable by id or by number, and a store keyed on only one would miss half the
// traffic. A command with neither cannot be recalled and must say so.
func TestKeyPrefersIDThenNumber(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        string
		wantID    uint32
		wantByNum bool
		wantOK    bool
	}{
		{"id", "\x1b_Ga=t,i=7;MQ==\x1b\\", 7, false, true},
		{"number", "\x1b_Ga=t,I=4;MQ==\x1b\\", 4, true, true},
		{"id wins over number", "\x1b_Ga=t,i=7,I=4;MQ==\x1b\\", 7, false, true},
		{"neither", "\x1b_Ga=t;MQ==\x1b\\", 0, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, ok := Parse([]byte(tc.in))
			if !ok {
				t.Fatal("Parse() ok = false")
			}
			id, byNumber, keyOK := cmd.Key()
			if id != tc.wantID || byNumber != tc.wantByNum || keyOK != tc.wantOK {
				t.Errorf("Key() = (%d, %v, %v), want (%d, %v, %v)",
					id, byNumber, keyOK, tc.wantID, tc.wantByNum, tc.wantOK)
			}
		})
	}
}

// Re-emission has to suppress responses, or a restored image puts answers on the input path for
// questions cm never asked, which is the failure interception exists to remove.
func TestWithQuietForcesSuppression(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"absent is appended", "a=T,f=100", "a=T,f=100,q=2"},
		{"existing is replaced", "a=T,q=0,f=100", "a=T,q=2,f=100"},
		{"already suppressed", "a=T,q=2", "a=T,q=2"},
		{"replaced not duplicated", "q=1,a=T", "q=2,a=T"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := WithQuiet(tc.in, 2)
			if got != tc.want {
				t.Errorf("WithQuiet(%q, 2) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Count(got, "q=") != 1 {
				t.Errorf("WithQuiet(%q, 2) = %q has %d q= keys, want exactly 1: a duplicate key is "+
					"undefined and a terminal may honor either", tc.in, got, strings.Count(got, "q="))
			}
		})
	}
}

// Encode and Parse have to round-trip, since re-emission builds a command that a real terminal then
// parses.
func TestEncodeRoundTrips(t *testing.T) {
	payload := []byte("iVBORw0KGgo")
	raw := Encode("a=T,q=2,f=100,s=4,v=3,i=1", payload)

	cmd, n, ok := Parse(raw)
	if !ok {
		t.Fatalf("Parse(Encode(...)) ok = false, raw = %q", raw)
	}
	if n != len(raw) {
		t.Errorf("consumed %d of %d bytes", n, len(raw))
	}
	if cmd.Action != ActionTransmitAndDisplay || cmd.ImageID != 1 || cmd.Quiet != 2 {
		t.Errorf("round trip lost fields: %+v", cmd)
	}
	if string(cmd.Payload) != string(payload) {
		t.Errorf("Payload = %q, want %q", cmd.Payload, payload)
	}
}

// A command with no payload must encode without a stray separator, which a delete or a put needs.
func TestEncodeOmitsEmptyPayload(t *testing.T) {
	raw := Encode("a=d,d=i,i=1", nil)
	if want := "\x1b_Ga=d,d=i,i=1\x1b\\"; string(raw) != want {
		t.Errorf("Encode() = %q, want %q", raw, want)
	}
	if _, _, ok := Parse(raw); !ok {
		t.Error("Parse() rejected a payload-free command")
	}
}
