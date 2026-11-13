package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/chancez/cm/internal/paths"
)

// launchdLabelPrefix starts the label of each job that starts a server.
const launchdLabelPrefix = "com.github.chancez.cm.server"

// launchctl is named by path rather than looked up, since a client's PATH is whatever launched it, and one
// started by a terminal's own integration carries only a handful of entries.
const launchctl = "/bin/launchctl"

// startServer starts a server through a launchd job, so it does not join the coalition of the app that
// started this client, and returns what to call once the server answers.
//
// macOS puts every process into the coalition of the process that spawned it, and Setsid does not leave it.
// LaunchServices treats every process in an app's coalition as belonging to that app: once the app quits with
// them still running, its Dock icon says "Running in Background", and Force Quit or "Stop Running in
// Background" on that icon kills every one of them. A server started from a terminal's shell was in that
// terminal's coalition, every shim and shell it spawned joined it, and so did each later server started by a
// command run inside one of those sessions. That is how 27 sessions died at once: a server had been started
// from Terminal.app at 09:56, two upgrades later its successor and every session were still in Terminal's
// coalition at 16:14, and the Dock's force quit of Terminal's leftover icon logged "killing coalition pid"
// for each of them. Nothing in cm could see it coming, and the shims' logs said only that they were
// signalled.
//
// launchd gives each job a coalition of its own that no app owns, so nothing in the Dock can reach it. The job
// runs `cm server spawn`, which starts the server in a session of its own and exits, and the job is removed
// once the server answers: the server outlives the job, measured by removing a job while its first process was
// still running. A job per start rather than one per installation, so concurrent starts cannot remove each
// other's.
//
// Falls back to starting the server directly when launchd will not take the job, for instance when this user
// has no gui domain to put it in. A server in the caller's coalition is still a server, and refusing to start
// one would take every window with it.
//
// Costs about 65ms on a start: `cm list` starting a server took 62 to 66ms directly and 115 to 194ms this
// way, median 131ms, of which bootstrap is about 10ms and bootout about 8ms. A start happens once per boot or
// upgrade, and stays under serverStartQuiet.
func startServer(exe string, dirs paths.Dirs) (func(), error) {
	if done, err := startServerUnderLaunchd(exe, dirs); err == nil {
		return done, nil
	}
	return func() {}, spawnServer(exe, dirs, nil)
}

// startServerUnderLaunchd submits a one-shot launchd job that starts a server, and returns what removes it.
func startServerUnderLaunchd(exe string, dirs paths.Dirs) (func(), error) {
	label := fmt.Sprintf("%s.%d.%d", launchdLabelPrefix, os.Getpid(), time.Now().UnixNano())
	reqPath := filepath.Join(dirs.Runtime, label+".req")
	plistPath := filepath.Join(dirs.Runtime, label+".plist")

	argv := append([]string{exe}, serverArgs(dirs, "server", "spawn", reqPath)...)
	plist, err := serverJobPlist(label, argv, dirs.ServerStartErr())
	if err != nil {
		return nil, err
	}
	if err := writeSpawnRequest(reqPath, newSpawnRequest()); err != nil {
		return nil, err
	}
	// Truncated here because the job's own stderr is appended to it, and a failure to read the request would
	// otherwise be reported alongside whatever an earlier start said.
	if f, err := os.Create(dirs.ServerStartErr()); err == nil {
		f.Close()
	}
	if err := os.WriteFile(plistPath, plist, 0o600); err != nil {
		_ = os.Remove(reqPath)
		return nil, err
	}
	// launchd reads the plist when the job is bootstrapped and never again.
	defer os.Remove(plistPath)

	domain := "gui/" + strconv.Itoa(os.Getuid())
	if out, err := exec.Command(launchctl, "bootstrap", domain, plistPath).CombinedOutput(); err != nil {
		_ = os.Remove(reqPath)
		return nil, fmt.Errorf("launchctl bootstrap %s: %w: %s", domain, err, bytes.TrimSpace(out))
	}
	return func() {
		_ = exec.Command(launchctl, "bootout", domain+"/"+label).Run()
		// Normally gone already, since spawn removes it once read. This covers a job that never got that far.
		_ = os.Remove(reqPath)
	}, nil
}

// serverJobPlist returns the launchd job that runs argv once.
//
// ProcessType is Interactive because every shell a user types into descends from this job, and its policy is
// inherited by every descendant, including after the job is gone. Measured on a setsid grandchild of a removed
// job: scheduling priority 31 with Interactive, the same as a process under a terminal, against 20 with
// launchd's default for an agent and 4 with Background.
func serverJobPlist(label string, argv []string, stderrPath string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	buf.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	buf.WriteString(`<plist version="1.0"><dict>` + "\n")

	str := func(key, value string) error {
		if !plistSafe(value) {
			return fmt.Errorf("%s %q cannot be written in a launchd plist", key, value)
		}
		buf.WriteString("<key>" + key + "</key><string>")
		_ = xml.EscapeText(&buf, []byte(value))
		buf.WriteString("</string>\n")
		return nil
	}
	if err := str("Label", label); err != nil {
		return nil, err
	}
	buf.WriteString("<key>ProgramArguments</key><array>")
	for _, arg := range argv {
		if !plistSafe(arg) {
			return nil, fmt.Errorf("argument %q cannot be written in a launchd plist", arg)
		}
		buf.WriteString("<string>")
		_ = xml.EscapeText(&buf, []byte(arg))
		buf.WriteString("</string>")
	}
	buf.WriteString("</array>\n")
	if err := str("StandardErrorPath", stderrPath); err != nil {
		return nil, err
	}
	if err := str("ProcessType", "Interactive"); err != nil {
		return nil, err
	}
	buf.WriteString("<key>RunAtLoad</key><true/>\n")
	buf.WriteString("<key>KeepAlive</key><false/>\n")
	buf.WriteString("</dict></plist>\n")
	return buf.Bytes(), nil
}

// plistSafe reports whether s can be written as a plist string without being altered.
//
// xml.EscapeText replaces what XML 1.0 cannot carry rather than failing, so a path holding a control character
// would reach launchd as a different path.
func plistSafe(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
		case r >= 0x20 && r <= 0xD7FF:
		case r >= 0xE000 && r <= 0xFFFD:
		case r >= 0x10000 && r <= 0x10FFFF:
		default:
			return false
		}
	}
	return true
}
