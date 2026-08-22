package graphics

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxTransferBytes bounds a file-based transfer cm will read.
//
// 100 MiB, matching the ceiling zellij applies to a decoded image. The bound exists because the payload
// names a path rather than carrying data, so its size is whatever is on disk rather than something the
// stream already spent: without a bound a program could name a huge file and have cm read all of it.
const MaxTransferBytes = 100 << 20

// ErrTransferRefused reports a transfer cm declined to read.
var ErrTransferRefused = errors.New("graphics transfer refused")

// ReadTransfer resolves a command that names a file into one that carries its data inline.
//
// This is the part that makes cm the terminal for graphics rather than a pipe, and the reason it must
// exist rather than forwarding: a transfer file is consumed once. Kitty opens the path, reads it, and
// unlinks it (`kitty/graphics.c` deletes any path containing "tty-graphics-protocol" after a successful
// read, and shm_unlinks a shared memory name), so two readers cannot both succeed. Under cm the program
// and the real terminal were both trying, which is the reported
// "EBADF ... Failed to open file for graphics transmission with error: [2] No such file or directory"
// for exactly the two of icat's three probes that name something on the filesystem.
//
// Reading it here and re-emitting the bytes inline means one reader, and the terminal receives a
// transmission it can satisfy without touching cm's filesystem.
//
// Returns the command unchanged when it names no file. Wraps ErrTransferRefused when the path is not one
// cm will read, so a caller can answer the program rather than hang.
func ReadTransfer(cmd Command) (Command, error) {
	if !cmd.Medium.NeedsFile() {
		return cmd, nil
	}

	// The payload is the path, base64 encoded like any other payload.
	decoded, err := base64.StdEncoding.DecodeString(string(cmd.Payload))
	if err != nil {
		return cmd, fmt.Errorf("%w: undecodable path: %w", ErrTransferRefused, err)
	}
	path := string(decoded)

	if cmd.Medium == MediumSharedMemory {
		// POSIX shared memory is not a filesystem path on darwin, and reading it needs shm_open rather
		// than open. Refused rather than half-implemented: a wrong guess at the path would read an
		// unrelated file, and answering "unsupported" makes icat fall back to a medium that works, which
		// is what it does when a terminal declines any medium.
		return cmd, fmt.Errorf("%w: shared memory transfers are not read by cm", ErrTransferRefused)
	}

	if err := allowTransferPath(path); err != nil {
		return cmd, err
	}

	f, err := os.Open(path)
	if err != nil {
		return cmd, fmt.Errorf("%w: %w", ErrTransferRefused, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return cmd, fmt.Errorf("%w: %w", ErrTransferRefused, err)
	}
	if !info.Mode().IsRegular() {
		// A fifo would block, a directory cannot be read, and a device is not an image. Kitty opens with
		// O_NONBLOCK for the fifo case specifically; refusing is simpler and cm has a fallback the
		// terminal does not.
		return cmd, fmt.Errorf("%w: %s is not a regular file", ErrTransferRefused, path)
	}
	if info.Size() > MaxTransferBytes {
		return cmd, fmt.Errorf("%w: %s is %d bytes, over the %d limit",
			ErrTransferRefused, path, info.Size(), MaxTransferBytes)
	}

	data := make([]byte, info.Size())
	if _, err := readFull(f, data); err != nil {
		return cmd, fmt.Errorf("%w: reading %s: %w", ErrTransferRefused, path, err)
	}

	// A temp file is cm's to delete now that it has been consumed, matching what the terminal would have
	// done. Leaving it would accumulate one file per image in the user's temp directory, since the
	// program hands the path over and never looks at it again.
	//
	// Guarded by the same name check kitty uses rather than deleting anything named: a t=f transfer can
	// name a file the user cares about, and only t=t promises the terminal may remove it.
	if cmd.Medium == MediumTempFile && isTempTransferPath(path) {
		if err := os.Remove(path); err != nil {
			// Not fatal. The image was read, so the transmission succeeds; a leftover file is untidy
			// rather than broken.
			_ = err
		}
	}

	// Rebuilt as a direct transmission carrying the bytes, since the file is now gone or at least
	// already read. The t= key is dropped rather than rewritten to t=d, because direct is the default
	// and leaving a stale medium would have the terminal look for a file again.
	out := cmd
	out.Medium = MediumDirect
	out.Control = withoutKeys(cmd.Control, "t")
	out.Payload = []byte(base64.StdEncoding.EncodeToString(data))
	out.Raw = Encode(out.Control, out.Payload)
	return out, nil
}

// allowTransferPath decides whether cm will read a path a program named.
//
// A program inside a session is already running as the user, so this is not a privilege boundary: it
// cannot reach anything the program could not open itself. What it does prevent is cm being used as a
// confused reader, turning a path into base64 on a terminal's screen for a file the program chose. So
// the rule matches what a graphics transfer legitimately looks like: a temp file, or something under a
// temp directory.
//
// Kitty applies the same shape of check before deleting, and asks its own permission hook before reading
// (`is_ok_to_read_image_file`). cm has no such hook, so the allow-list is the whole of it.
func allowTransferPath(path string) error {
	if !filepath.IsAbs(path) {
		// A relative path would resolve against the server's working directory, which is not the
		// program's, so it would name a different file than the program meant.
		return fmt.Errorf("%w: %s is not an absolute path", ErrTransferRefused, path)
	}
	clean := filepath.Clean(path)
	if isTempTransferPath(clean) {
		return nil
	}
	for _, dir := range transferDirs() {
		if dir == "" {
			continue
		}
		if rel, err := filepath.Rel(dir, clean); err == nil && !strings.HasPrefix(rel, "..") {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not under a temporary directory", ErrTransferRefused, clean)
}

// isTempTransferPath reports whether a path is one a graphics client created for a transfer.
//
// The marker is kitty's own: it names transfer files "kitty-tty-graphics-protocol-*" and deletes any
// path containing "tty-graphics-protocol" after reading, so matching the same substring is matching the
// convention rather than inventing one.
func isTempTransferPath(path string) bool {
	return strings.Contains(path, "tty-graphics-protocol")
}

// transferDirs lists the directories a transfer file may live under.
func transferDirs() []string {
	dirs := []string{os.TempDir(), "/tmp"}
	// /dev/shm is where shared memory appears on Linux, and a t=f transfer may name one there.
	if _, err := os.Stat("/dev/shm"); err == nil {
		dirs = append(dirs, "/dev/shm")
	}
	return dirs
}

// readFull fills buf, treating a short read as an error.
func readFull(f *os.File, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := f.Read(buf[total:])
		total += n
		if err != nil {
			if total == len(buf) {
				return total, nil
			}
			return total, err
		}
		if n == 0 {
			return total, errors.New("short read")
		}
	}
	return total, nil
}

// withoutKeys removes named keys from a control section.
func withoutKeys(control string, keys ...string) string {
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	parts := strings.Split(control, ",")
	out := make([]string, 0, len(parts))
	for _, kv := range parts {
		if k, _, found := strings.Cut(kv, "="); found && drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return strings.Join(out, ",")
}
