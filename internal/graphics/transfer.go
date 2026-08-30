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
	decoded, err := decodePayload(cmd.Payload)
	if err != nil {
		return cmd, fmt.Errorf("%w: undecodable path: %w", ErrTransferRefused, err)
	}
	path := string(decoded)

	// Shared memory is read through its own opener, because it is not a filesystem path on darwin: the
	// name lives in a namespace reached only by shm_open. Declining it instead was measured to be a real
	// downgrade rather than a safe fallback, which is why this exists: bare kitty negotiates `memory`
	// with icat, and a cm that refused it got `files`, so cm was making a working setup worse.
	var f *os.File
	if cmd.Medium == MediumSharedMemory {
		var err error
		if f, err = openShm(path); err != nil {
			return cmd, err
		}
	} else {
		if err := allowTransferPath(path); err != nil {
			return cmd, err
		}
		var err error
		if f, err = os.Open(path); err != nil {
			return cmd, fmt.Errorf("%w: %w", ErrTransferRefused, err)
		}
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return cmd, fmt.Errorf("%w: %w", ErrTransferRefused, err)
	}
	// The regular-file check applies to filesystem transfers only. A fifo would block a read forever,
	// which on the output pump stalls the whole session rather than one image, and a directory or device
	// is not an image at all. Kitty opens with O_NONBLOCK for the fifo case; refusing is simpler and cm
	// has a fallback the terminal does not.
	//
	// Skipped for shared memory because the object is not a regular file on every platform, and it cannot
	// be a fifo or a directory: the name reached shm_open, so whatever it named is shared memory.
	if cmd.Medium != MediumSharedMemory && !info.Mode().IsRegular() {
		return cmd, fmt.Errorf("%w: %s is not a regular file", ErrTransferRefused, path)
	}
	// How much of the container is actually the image. Getting this wrong is not a rounding error: the
	// terminal rejects a payload larger than the geometry implies, with "EFBIG: Too much data".
	//
	// Three sources, narrowest first, and the order matters. The geometry is authoritative for a raw
	// pixel format because that is what the terminal itself computes from, s*v*(f/8) in kitty. S= is next,
	// but it describes the *container* rather than the image, which is why it cannot be trusted alone: a
	// shared memory object is rounded up to a page, so a 3 byte image sits in 4096 bytes and an S= naming
	// the object's length hands over 4093 bytes of padding. Measured as a payload of [1 2 3 0 0 0 0 0 0 0 0]
	// against 3 bytes expected. The container's own size is the fallback, for PNG, where nothing else
	// knows the answer.
	size := info.Size()
	if cmd.DataSize > 0 && int64(cmd.DataSize) < size {
		size = int64(cmd.DataSize)
	}
	if want := cmd.ExpectedBytes(); want > 0 && int64(want) < size {
		size = int64(want)
	}
	if size > MaxTransferBytes {
		return cmd, fmt.Errorf("%w: %s is %d bytes, over the %d limit",
			ErrTransferRefused, path, size, MaxTransferBytes)
	}

	// Shared memory is read through its own path, because a descriptor from shm_open on darwin does not
	// support read(2): it fails with ENXIO, surfacing as "device not configured", which reads like a
	// missing device rather than the wrong syscall. Linux exposes these as files and reads them normally.
	var data []byte
	if cmd.Medium == MediumSharedMemory {
		if size == 0 {
			return cmd, fmt.Errorf("%w: %s is empty", ErrTransferRefused, path)
		}
		if data, err = readShm(f, int(size)); err != nil {
			return cmd, err
		}
	} else {
		data = make([]byte, size)
		if _, err := readFull(f, data); err != nil {
			return cmd, fmt.Errorf("%w: reading %s: %w", ErrTransferRefused, path, err)
		}
	}

	// Consumed, so cm removes it, matching what the terminal would have done. Leaving one behind
	// accumulates an object per image, since the program hands the name over and never looks at it again.
	//
	// Neither failure is fatal: the image has been read by this point, so the transmission succeeds and a
	// leftover object is untidy rather than broken.
	//
	// A t=f transfer is deliberately not removed. It can name a file the user cares about, and only t=t
	// and t=s promise the terminal may destroy what they name; kitty draws the same line, and additionally
	// requires its own naming convention before deleting a temp file, which is what isTempTransferPath
	// matches.
	switch {
	case cmd.Medium == MediumTempFile && isTempTransferPath(path):
		_ = os.Remove(path)
	case cmd.Medium == MediumSharedMemory:
		_ = unlinkShm(path)
	}

	// Rebuilt as a direct transmission carrying the bytes, since the file is now gone or at least
	// already read. The t= key is dropped rather than rewritten to t=d, because direct is the default
	// and leaving a stale medium would have the terminal look for a file again.
	//
	// Raw is chunked rather than one command, and holds several when the image is large. That is the one
	// place a Command's Raw is not a single command, and it is required: a file names its data in a few
	// dozen bytes and carries none, so inlining turns a small command into a payload the size of the
	// image, and a single command that size is one a terminal may discard. See EncodeChunks.
	out := cmd
	out.Medium = MediumDirect
	out.Control = withoutKeys(cmd.Control, "t")
	out.Payload = []byte(base64.StdEncoding.EncodeToString(data))
	out.Raw = EncodeChunks(out.Control, out.Payload)
	return out, nil
}

// decodePayload decodes a command's payload, accepting it padded or not.
//
// Required rather than defensive, and the reason is the whole of a reported breakage. kitty's own clients
// encode payloads *unpadded*: `tools/tui/graphics/command.go` uses base64.RawStdEncoding, so a path whose
// length is not a multiple of three arrives without the trailing "=" that base64.StdEncoding demands.
// Decoding such a payload with StdEncoding fails, cm declines the transfer, the whole command is dropped,
// and no image ever reaches the terminal. `kitten icat` reports nothing and exits 0, so the only trace is
// cm's own "declined a graphics transfer" line.
//
// Measured from a real session's log against a real icat capture. The path
// /Users/chancez/screenshots/hsa_contribution.png is 47 bytes, its unpadded base64 is 63 characters, and
// StdEncoding rejects that at byte 60, which is exactly what the log said. A t=t temp path of 87 bytes
// encodes to 116 characters and decoded fine. That is what made this look intermittent rather than
// deterministic: only a path whose length is divisible by three survives, so a fixed filename fails every
// time while kitty's own temp names, which end in a random number of varying digit count, fail about two
// invocations in three.
//
// Padding is stripped rather than the two encodings tried in turn, so a program that does pad is still
// understood: RawStdEncoding is strict about a "=" it does not expect.
func decodePayload(payload []byte) ([]byte, error) {
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(string(payload), "="))
}

// allowTransferPath decides whether cm will read a path a program named.
//
// Only the path's shape is checked, and that is a reversal worth stating with its evidence. This used to
// require a temp directory, on the reasoning that cm should not be a confused reader turning an arbitrary
// path into base64 on someone's screen. The measurement that overturned it: `kitten icat
// ~/screenshots/hsa_contribution.png` names the user's own file directly with t=f, because for a local
// file icat has no reason to copy it first. cm refused it as "not under a temporary directory", dropped
// the command, and no image appeared. The allow-list was therefore not protecting against a hostile case,
// it was breaking the ordinary one, and it broke it for every file a person is likely to look at.
//
// The reasoning it rested on does not hold either, and the previous comment said so in its own first
// sentence: a program inside a session already runs as the user, so it can open the file itself and send
// the bytes inline. `icat --transfer-mode=stream` does exactly that. Refusing to *read* what a program
// could hand over anyway buys nothing, and kitty itself reads whatever path its client names.
//
// What is still enforced is elsewhere and unchanged: a non-regular file is refused, since a fifo would
// block the output pump forever, the read is bounded by MaxTransferBytes, and deletion stays limited to
// t=t paths carrying kitty's own naming convention plus shared memory. That last one is the property that
// actually matters, because it is the only one that could destroy something.
func allowTransferPath(path string) error {
	if !filepath.IsAbs(path) {
		// A relative path would resolve against the server's working directory, which is not the
		// program's, so it would name a different file than the program meant.
		return fmt.Errorf("%w: %s is not an absolute path", ErrTransferRefused, path)
	}
	return nil
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
