package graphics

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// transferCommand builds a command naming a path, the way icat's probes do.
func transferCommand(t *testing.T, medium Medium, path string) Command {
	t.Helper()
	raw := "\x1b_Ga=q,f=24,t=" + string(medium) + ",s=1,v=1,i=1;" +
		base64.StdEncoding.EncodeToString([]byte(path)) + "\x1b\\"
	return mustParse(t, raw)
}

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

	got, err := ReadTransfer(transferCommand(t, MediumTempFile, path))
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

	if _, err := ReadTransfer(transferCommand(t, MediumTempFile, path)); err != nil {
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

	if _, err := ReadTransfer(transferCommand(t, MediumFile, path)); err != nil {
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

// cm refuses paths that do not look like a graphics transfer, so it cannot be used as a confused reader
// turning an arbitrary file into base64 on someone's screen.
func TestReadTransferRefusesPathsOutsideTemp(t *testing.T) {
	for _, tc := range []struct{ name, path string }{
		{"absolute elsewhere", "/etc/passwd"},
		{"home", filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa")},
		{"relative", "some/relative/path"},
		{"traversal out of temp", filepath.Join(os.TempDir(), "..", "..", "etc", "passwd")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadTransfer(transferCommand(t, MediumFile, tc.path))
			if !errors.Is(err, ErrTransferRefused) {
				t.Errorf("ReadTransfer(%q) error = %v, want ErrTransferRefused", tc.path, err)
			}
		})
	}
}

// Shared memory is declined rather than half-implemented: reading it needs shm_open, and guessing a
// filesystem path would read an unrelated file. icat falls back when a medium is declined, which is what
// makes refusing safe.
func TestReadTransferDeclinesSharedMemory(t *testing.T) {
	_, err := ReadTransfer(mustParse(t, probeSharedMem))
	if !errors.Is(err, ErrTransferRefused) {
		t.Errorf("ReadTransfer() error = %v, want ErrTransferRefused for shared memory", err)
	}
}

// A missing file is refused rather than crashing, which is also the state icat's own probe files are in
// by the time anything else looks: icat deletes them when detection finishes.
func TestReadTransferRefusesAMissingFile(t *testing.T) {
	path := filepath.Join(os.TempDir(), "kitty-tty-graphics-protocol-does-not-exist")
	_, err := ReadTransfer(transferCommand(t, MediumTempFile, path))
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
	_, err := ReadTransfer(transferCommand(t, MediumFile, dir))
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

	_, err = ReadTransfer(transferCommand(t, MediumTempFile, f.Name()))
	if !errors.Is(err, ErrTransferRefused) {
		t.Errorf("ReadTransfer() error = %v, want ErrTransferRefused for an oversized file", err)
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
