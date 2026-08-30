package graphics

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// transferCommand builds a command naming a path, the way icat's probes do.
//
// size is the S= key, which icat always sends and which matters for shared memory: an shm object is
// rounded up to a page, so without it a 3 byte payload is read back as 4096 bytes of mostly padding.
// Zero omits the key, for the cases that are about the path rather than the length.
// The geometry is deliberately absent rather than a fixed s=1,v=1: an inlined payload is now bounded by
// what the geometry implies, so a hardcoded 1x1 would truncate every test's data to three bytes. Two of
// them failed exactly that way when the bound landed, which is the check working rather than a nuisance.
// Tests that care about geometry state it themselves.
func transferCommand(t *testing.T, medium Medium, path string, size int) Command {
	t.Helper()
	control := "a=q,t=" + string(medium) + ",i=1"
	if size > 0 {
		control += ",S=" + strconv.Itoa(size)
	}
	raw := "\x1b_G" + control + ";" + kittyEncode([]byte(path)) + "\x1b\\"
	return mustParse(t, raw)
}

// inlined is the command ReadTransfer must produce for a resolved `a=T,t=t,i=1` transfer of data.
//
// Spelled once so the whole returned value can be asserted rather than a field at a time: a field-by-field
// check passes while the rest of the struct is wrong, and what matters here is that the t= key is gone and
// the payload carries the bytes.
func inlined(data []byte) Command {
	payload := []byte(base64.StdEncoding.EncodeToString(data))
	return Command{
		Action:  ActionTransmitAndDisplay,
		Medium:  MediumDirect,
		ImageID: 1,
		Control: "a=T,i=1",
		Payload: payload,
		Raw:     Encode("a=T,i=1", payload),
	}
}

// kittyEncode encodes a payload the way kitty's own clients do: base64 with no padding.
//
// Every helper here used to pad, which is why the whole package passed while `kitten icat` was refused in a
// real session. A test that encodes the way the code under test decodes cannot fail, and this one did not
// for as long as it disagreed with kitty. See decodePayload.
func kittyEncode(b []byte) string { return base64.RawStdEncoding.EncodeToString(b) }

// writeTransferFile creates a file with kitty's own transfer naming, under the temp directory.
func writeTransferFile(t *testing.T, data []byte) string {
	t.Helper()
	f, err := os.CreateTemp("", "kitty-tty-graphics-protocol-*")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

// The whole point: a command naming a file comes back carrying the bytes, so the terminal never has to
// open a path that cm already consumed.
func TestReadTransferInlinesAFile(t *testing.T) {
	want := []byte{1, 2, 3, 4, 5}
	path := writeTransferFile(t, want)

	got, err := ReadTransfer(transferCommand(t, MediumTempFile, path, 0))
	if err != nil {
		t.Fatalf("ReadTransfer() error = %v", err)
	}

	if got.Medium != MediumDirect {
		t.Errorf("Medium = %q, want %q", got.Medium, MediumDirect)
	}
	// The t= key has to be gone rather than rewritten, or the terminal looks for a file again.
	if strings.Contains(got.Control, "t=") {
		t.Errorf("Control = %q still names a medium", got.Control)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(got.Payload))
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	if string(decoded) != string(want) {
		t.Errorf("payload decoded to %v, want %v", decoded, want)
	}
	// And the rebuilt command has to be parseable, since a terminal reads it next.
	if _, _, ok := Parse(got.Raw); !ok {
		t.Errorf("Raw = %q does not parse", got.Raw)
	}
}

// A temp transfer is cm's to remove once read, matching what the terminal would have done. Otherwise
// one file per image accumulates, since the program hands over the path and never looks again.
func TestReadTransferDeletesATempFile(t *testing.T) {
	path := writeTransferFile(t, []byte("data"))

	if _, err := ReadTransfer(transferCommand(t, MediumTempFile, path, 0)); err != nil {
		t.Fatalf("ReadTransfer() error = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the temp transfer file still exists after being read (stat err = %v)", err)
	}
}

// A t=f transfer may name a file the user cares about, and only t=t promises the terminal may remove it.
func TestReadTransferKeepsANamedFile(t *testing.T) {
	dir := t.TempDir()
	// Named without kitty's temp marker, but inside a temp directory so it is readable.
	path := filepath.Join(dir, "picture.png")
	if err := os.WriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if _, err := ReadTransfer(transferCommand(t, MediumFile, path, 0)); err != nil {
		t.Fatalf("ReadTransfer() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a t=f file was deleted; only t=t allows that (stat err = %v)", err)
	}
}

// A command carrying its own data is untouched, which is the common case and must not be slowed or
// rewritten.
func TestReadTransferLeavesDirectAlone(t *testing.T) {
	in := mustParse(t, probeDirect)
	got, err := ReadTransfer(in)
	if err != nil {
		t.Fatalf("ReadTransfer() error = %v", err)
	}
	if string(got.Raw) != string(in.Raw) {
		t.Errorf("Raw = %q, want it unchanged", got.Raw)
	}
}

// A relative path is refused, since it would resolve against the server's working directory rather than
// the program's and so name a different file than the program meant.
//
// Everything absolute is now read, which is a reversal: this used to require a temp directory. `kitten
// icat ~/screenshots/hsa_contribution.png` names the user's own file with t=f, because for a local file
// icat has no reason to copy it first, and cm refused it as "not under a temporary directory" and dropped
// the image. The restriction was breaking the ordinary case rather than preventing a hostile one, and a
// program inside a session can hand cm the same bytes inline anyway. See allowTransferPath.
func TestReadTransferRefusesRelativePaths(t *testing.T) {
	_, err := ReadTransfer(transferCommand(t, MediumFile, "some/relative/path", 0))
	if !errors.Is(err, ErrTransferRefused) {
		t.Errorf("ReadTransfer(relative) error = %v, want ErrTransferRefused", err)
	}
}

// A file the user named outside a temp directory is read, which is the reported case.
func TestReadTransferReadsAFileTheUserNamed(t *testing.T) {
	// Under the test's own directory, which is not a temp transfer path and not under os.TempDir on a
	// machine where those differ. What matters is that it is somewhere a person keeps files.
	dir := t.TempDir()
	path := filepath.Join(dir, "holiday.png")
	want := []byte{9, 9, 9, 1}
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := ReadTransfer(mustParse(t, "\x1b_Ga=T,t=f,i=1;"+kittyEncode([]byte(path))+"\x1b\\"))
	if err != nil {
		t.Fatalf("ReadTransfer() error = %v, want the file read", err)
	}
	decoded, err := base64Decode(string(got.Payload))
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	if string(decoded) != string(want) {
		t.Errorf("payload = %v, want the file's bytes %v", decoded, want)
	}
	// And it must still be there afterwards. This is the property the old allow-list was really
	// protecting, and it is enforced where it belongs: only t=t with kitty's naming, or shm, is removed.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a t=f file the user named was deleted (stat err = %v)", err)
	}
}

// Shared memory round-trips, which is the medium kitty prefers.
//
// Declining it was measured to be a downgrade rather than a safe fallback: bare kitty negotiates `memory`
// with icat, and a cm that refused it got `files`, so cm made a working setup worse. That is why this is
// implemented rather than refused, and it is the case a path-based reader gets wrong on darwin, where the
// name lives in a namespace reachable only through shm_open.
func TestReadTransferReadsSharedMemory(t *testing.T) {
	want := []byte{9, 8, 7}
	name := writeShmObject(t, want)

	got, err := ReadTransfer(transferCommand(t, MediumSharedMemory, name, len(want)))
	if err != nil {
		t.Fatalf("ReadTransfer() error = %v", err)
	}
	if got.Medium != MediumDirect {
		t.Errorf("Medium = %q, want %q", got.Medium, MediumDirect)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(got.Payload))
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	if string(decoded) != string(want) {
		t.Errorf("payload decoded to %v, want %v", decoded, want)
	}

	// Consumed, so cm unlinks it: leaving one behind accumulates an object per image, since the program
	// hands the name over and never looks again.
	if _, err := ReadTransfer(transferCommand(t, MediumSharedMemory, name, len(want))); err == nil {
		t.Error("the shared memory object survived being read, so it was not unlinked")
	}
}

// A shared memory name is refused when nothing has that name, rather than crashing or reading something
// else.
func TestReadTransferRefusesAMissingShmObject(t *testing.T) {
	_, err := ReadTransfer(transferCommand(t, MediumSharedMemory, "cm-test-does-not-exist", 0))
	if !errors.Is(err, ErrTransferRefused) {
		t.Errorf("ReadTransfer() error = %v, want ErrTransferRefused", err)
	}
}

// A missing file is refused rather than crashing, which is also the state icat's own probe files are in
// by the time anything else looks: icat deletes them when detection finishes.
func TestReadTransferRefusesAMissingFile(t *testing.T) {
	path := filepath.Join(os.TempDir(), "kitty-tty-graphics-protocol-does-not-exist")
	_, err := ReadTransfer(transferCommand(t, MediumTempFile, path, 0))
	if !errors.Is(err, ErrTransferRefused) {
		t.Errorf("ReadTransfer() error = %v, want ErrTransferRefused", err)
	}
}

// Anything that is not a regular file is refused. A fifo would block a read forever, which on the output
// pump would stall the whole session.
func TestReadTransferRefusesNonRegularFiles(t *testing.T) {
	dir := t.TempDir()
	// A directory is the portable stand-in; a fifo needs syscall support the test does not need to
	// assume, and both take the same branch.
	_, err := ReadTransfer(transferCommand(t, MediumFile, dir, 0))
	if !errors.Is(err, ErrTransferRefused) {
		t.Errorf("ReadTransfer(directory) error = %v, want ErrTransferRefused", err)
	}
}

// An oversized file is refused, since the payload names a path and so its size is whatever is on disk
// rather than something the stream already spent.
func TestReadTransferRefusesAnOversizedFile(t *testing.T) {
	f, err := os.CreateTemp("", "kitty-tty-graphics-protocol-*")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	defer os.Remove(f.Name())
	// Sparse, so the test does not write 100 MiB.
	if err := f.Truncate(MaxTransferBytes + 1); err != nil {
		t.Fatalf("Truncate() error = %v", err)
	}
	f.Close()

	_, err = ReadTransfer(transferCommand(t, MediumTempFile, f.Name(), 0))
	if !errors.Is(err, ErrTransferRefused) {
		t.Errorf("ReadTransfer() error = %v, want ErrTransferRefused for an oversized file", err)
	}
}

// A path arrives unpadded, because that is how kitty's own clients encode it, and every path length must
// resolve rather than only the ones divisible by three.
//
// The reported breakage. cm decoded with base64.StdEncoding, which demands padding, so a path of 47 bytes
// (unpadded base64 is 63 characters) was refused at byte 60 and the image never reached the terminal.
// icat exits 0 with nothing printed, so the only symptom was a missing image. A path length divisible by
// three encodes to a multiple of four and needed no padding, which is why the same command worked
// occasionally and made this look like a race: kitty's temp names end in a random number, so their length
// varies, while the user's own filename failed every single time.
//
// The three residues are the whole test. Only len%3 == 0 passed before the fix, so a single fixture would
// have had a two in three chance of catching it.
func TestReadTransferAcceptsAnUnpaddedPath(t *testing.T) {
	for _, residue := range []int{0, 1, 2} {
		t.Run("path length %3 == "+strconv.Itoa(residue), func(t *testing.T) {
			want := []byte{1, 2, 3, 4, 5}
			path := writeTransferFile(t, want)
			// Lengthened by renaming rather than by padding the name blindly, so the file the command
			// names is the file on disk.
			for len(path)%3 != residue {
				next := path + "x"
				if err := os.Rename(path, next); err != nil {
					t.Fatalf("Rename() error = %v", err)
				}
				path = next
			}
			t.Cleanup(func() { os.Remove(path) })

			cmd := mustParse(t, "\x1b_Ga=T,t=t,i=1;"+kittyEncode([]byte(path))+"\x1b\\")
			got, err := ReadTransfer(cmd)
			if err != nil {
				t.Fatalf("ReadTransfer() error = %v, for a %d byte path encoding to %d characters",
					err, len(path), len(kittyEncode([]byte(path))))
			}
			if wantCmd := inlined(want); !reflect.DeepEqual(got, wantCmd) {
				t.Errorf("ReadTransfer() = %+v, want %+v", got, wantCmd)
			}
		})
	}
}

// A program that does pad is still understood, so accepting the unpadded form is an addition rather than a
// swap: RawStdEncoding on its own rejects a "=" it did not expect.
func TestReadTransferAcceptsAPaddedPath(t *testing.T) {
	want := []byte{1, 2, 3}
	path := writeTransferFile(t, want)

	cmd := mustParse(t, "\x1b_Ga=T,t=t,i=1;"+
		base64.StdEncoding.EncodeToString([]byte(path))+"\x1b\\")
	got, err := ReadTransfer(cmd)
	if err != nil {
		t.Fatalf("ReadTransfer() error = %v", err)
	}
	if wantCmd := inlined(want); !reflect.DeepEqual(got, wantCmd) {
		t.Errorf("ReadTransfer() = %+v, want %+v", got, wantCmd)
	}
}

// An undecodable path is refused rather than treated as a filename, since base64 is how every payload
// arrives.
func TestReadTransferRefusesAnUndecodablePath(t *testing.T) {
	cmd := mustParse(t, "\x1b_Ga=q,t=t,i=1;!!!not base64!!!\x1b\\")
	_, err := ReadTransfer(cmd)
	if !errors.Is(err, ErrTransferRefused) {
		t.Errorf("ReadTransfer() error = %v, want ErrTransferRefused", err)
	}
}

// withoutKeys must remove only what it is asked to, or a rebuilt command loses geometry and the terminal
// draws the image at the wrong size.
func TestWithoutKeysKeepsEverythingElse(t *testing.T) {
	got := withoutKeys("a=T,t=t,f=100,s=4,v=3,i=1", "t")
	want := "a=T,f=100,s=4,v=3,i=1"
	if got != want {
		t.Errorf("withoutKeys() = %q, want %q", got, want)
	}
}

// An inlined transfer must carry exactly the bytes the geometry implies, not the whole container.
//
// This is the bug the sandbox surfaced as "EFBIG: Too much data" from a real kitty. A terminal derives
// the expected byte count from the geometry rather than from S=: kitty computes s*v*(f/8) and allocates
// that plus ten bytes, so a payload beyond it is rejected outright. A shared memory object is rounded up
// to a page, so a 3 byte image arrives inside 4096, and trusting S= or the container's length hands over
// thousands of bytes of padding the program never sent. Measured before the fix as [1 2 3 0 0 0 0 0 0 0 0].
//
// zellij avoids this on its own output path by building a transmit control from scratch and emitting no
// S= at all, deriving everything from the geometry it holds. cm cannot do quite that, since it forwards a
// program's own command rather than re-originating it, so it bounds the read instead.
func TestReadTransferBoundsPayloadByGeometry(t *testing.T) {
	// Three bytes of image, in a container that is larger. The geometry says 1x1 RGB, so 3 bytes.
	image := []byte{1, 2, 3}
	padded := append(append([]byte{}, image...), make([]byte, 200)...)

	for _, tc := range []struct {
		name   string
		medium Medium
		// declared is the S= value, describing the container rather than the image, which is what a real
		// client sends for shared memory.
		declared int
	}{
		{"tempfile with a container-sized S", MediumTempFile, len(padded)},
		{"tempfile with no S", MediumTempFile, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTransferFile(t, padded)
			cmd := mustParse(t, "\x1b_Ga=T,f=24,s=1,v=1,t="+string(tc.medium)+
				",i=1"+optS(tc.declared)+";"+
				base64Encode([]byte(path))+"\x1b\\")

			got, err := ReadTransfer(cmd)
			if err != nil {
				t.Fatalf("ReadTransfer() error = %v", err)
			}
			decoded, err := base64Decode(string(got.Payload))
			if err != nil {
				t.Fatalf("payload is not base64: %v", err)
			}
			if string(decoded) != string(image) {
				t.Errorf("payload = %v, want %v: the geometry implies %d bytes, so anything longer is "+
					"container padding a terminal rejects with EFBIG",
					decoded, image, cmd.ExpectedBytes())
			}
		})
	}
}

// A PNG payload has no derivable size, so the container's length stands: a compressed image's decoded
// size is not a function of its geometry, and bounding it by s*v*(f/8) would truncate it.
func TestReadTransferDoesNotBoundPNGByGeometry(t *testing.T) {
	data := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...)
	path := writeTransferFile(t, data)

	cmd := mustParse(t, "\x1b_Ga=T,f=100,s=1712,v=1294,t=t,i=1;"+
		base64Encode([]byte(path))+"\x1b\\")
	if want := cmd.ExpectedBytes(); want != 0 {
		t.Fatalf("ExpectedBytes() = %d for PNG, want 0: its decoded size is not derivable", want)
	}

	got, err := ReadTransfer(cmd)
	if err != nil {
		t.Fatalf("ReadTransfer() error = %v", err)
	}
	decoded, err := base64Decode(string(got.Payload))
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	if len(decoded) != len(data) {
		t.Errorf("payload is %d bytes, want all %d: a PNG must not be bounded by geometry",
			len(decoded), len(data))
	}
}

func optS(n int) string {
	if n == 0 {
		return ""
	}
	return ",S=" + strconv.Itoa(n)
}

// base64Encode builds a payload as a program would, so unpadded like kitty. See kittyEncode.
func base64Encode(b []byte) string { return kittyEncode(b) }

// base64Decode reads a payload cm produced, which is padded, so this one is StdEncoding on purpose.
func base64Decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }
