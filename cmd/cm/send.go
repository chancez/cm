package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/input"
	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newSendCommand(g *globals) *cobra.Command {
	var (
		newline  bool
		until    string
		timeout  time.Duration
		asJSON   bool
		follow   bool
		raw      bool
		keys     []string
		match    string
		matchRaw bool
	)
	cmd := &cobra.Command{
		Use:   "send <session> [text]...",
		Short: "Send input to a session without attaching",
		Long: `Send input to a session without attaching.

Text is written to the pty exactly as typing it would be, so the session's own
echo and prompt appear in its output.

--key sends a keystroke instead of characters, which text cannot express:

  cm send build --key ctrl-c              # interrupt what is running
  cm send agent --key escape             # leave a program's insert mode
  cm send agent 'yes' --key enter        # type, then press enter
  cm send menu --key down --key down --key enter

Accepts ctrl-c (or c-c, or ^C), alt-x, named keys like enter, tab, escape,
backspace, delete, up, down, left, right, home, end, pageup, pagedown, and f1
through f12. Repeat it to send several in order; keys are sent after any text.

An unknown key name is an error rather than being sent as text. That matters
because the failure is otherwise silent: 'cm send build ctrl-c' types the
characters "ctrl-c" onto the command line and the build keeps running, which reads
as cm having ignored the request.

--enter's carriage return is written separately from the text, after a short pause,
rather than appended to it. A pty read returns at most 1022 bytes, so a long line
reaches the program as several reads, and a full-screen program treats that burst as
a paste: a carriage return inside it is pasted content rather than the key that
submits. Sending it on its own is what makes a long prompt to an agent submit
instead of sitting in its input box.

--key goes through the pty, so it reaches whatever has the terminal in the state a
keypress would. Use 'cm signal' instead when the target is the process rather than
the keyboard: a program that reads ctrl-c as a byte rather than as an interrupt
will not stop for --key, and a session with no foreground job has nothing to
interrupt.

--wait blocks until the session reaches a state after the input lands, which is
how a script or an agent runs something and then reads the result:

  cm send build 'make' --enter --wait idle && cm read build

That is one request, not a send followed by 'cm wait', and the difference is
correctness rather than efficiency. The command starts as soon as the input
arrives, so a fast one finishes before a separate wait could be issued, and that
wait would then block until its timeout having missed what it was waiting for.
The server arms the wait before writing the input.

--follow streams the session's output while the command runs and returns when it
finishes, which is what watching a build looks like without attaching:

  cm send build 'make' --enter --follow

It implies --wait idle, since it has to know when to stop. Escape sequences are
stripped, so the output is plain text: a colour code in a redirected build log is
noise, and this is usually what replaces a send followed by a read where you had to
guess how much to read. Use --raw to keep the sequences.

What you see is everything the session printed, which includes the shell echoing the
line it was sent and the prompt that follows. Those are the session's own output, not
something cm adds, and suppressing them would mean not writing the input through the
pty at all. 'cm read' afterwards renders the screen instead, if the command's output
alone is what you want.

The stream is opened before the input is sent, so nothing the command prints at
the start is missed. Doing it the other way round loses whatever appears before the
follower connects, which for a fast command can be all of it.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return errors.New("a session name is required")
			}
			// Text is optional only because --key can supply the input instead. Without either there is
			// nothing to send, and silently sending an empty string would look like it worked.
			if len(args) == 1 && len(keys) == 0 {
				return errors.New("nothing to send; give text or use --key")
			}
			return nil
		},
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if err := paths.ValidateSessionRef(name); err != nil {
				return err
			}
			data := strings.Join(args[1:], " ")

			// Keys after any text, so `--key ctrl-c` interrupts what is running and
			// `cm send s 'make' --key enter` types then presses. A caller wanting the other order can
			// make two calls; guessing an interleaving from flag position would be worse than a fixed
			// rule, since cobra does not preserve it.
			if len(keys) > 0 {
				encoded, err := input.ParseKeys(keys)
				if err != nil {
					return err
				}
				data += string(encoded)
			}
			// Carriage return, not newline: a shell at its prompt has the pty in raw mode, where CR is
			// what accept-line is bound to.
			//
			// Held separately rather than appended, because concatenating it into the same pty write is
			// what made `--enter` fail to submit against a full-screen reader. A pty read returns at most
			// 1022 bytes, measured, so one large write arrives as several reads: 1201 bytes came back as
			// [1022, 179], with the CR in that 179-byte tail. A program doing paste detection sees a
			// multi-read burst, treats it as pasted content, and consumes the trailing CR as part of the
			// paste rather than as the keypress that submits it.
			//
			// Reported driving a Claude Code session with cm send: the prompt appeared in its input box
			// as "[Pasted text #4]" and sat there unsubmitted, and a second `cm send --key enter`
			// submitted it. Measured against a real one, holding everything but length constant: 42 bytes
			// submitted, 121 and 281 bytes landed without submitting, and 842 bytes did not appear at all
			// until a separate enter arrived. Two writes submitted at every size.
			enter := ""
			if newline {
				enter = "\r"
			}

			if match != "" && until != "" {
				return errors.New("--match and --wait cannot be combined; " +
					"--match waits on output and --wait waits on a state")
			}
			if matchRaw && match == "" {
				return errors.New("--match-raw only applies with --match")
			}
			if match != "" && follow {
				// Refused rather than resolved. --follow stops when its wait resolves, and a match
				// resolving mid-command would cut the stream off partway through output the caller was
				// watching, which reads as truncation rather than as the flag working.
				return errors.New("--match and --follow cannot be combined; " +
					"follow streams until the command ends, which is a different stopping point")
			}

			// --follow implies waiting for idle, since streaming until "whenever" is not a thing: the
			// command has to end for this to return. An explicit --wait still wins, so
			// `--follow --wait exited` watches until the session itself finishes rather than until the
			// command does.
			if follow && until == "" {
				until = "idle"
			}

			var state serverv1.WaitState
			if until != "" {
				var ok bool
				state, ok = waitStates[until]
				if !ok {
					return fmt.Errorf("unknown state %q, want one of idle, busy, blocked, exited", until)
				}
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			if follow {
				if raw {
					warnIfTerminal(os.Stdout)
				}
				cfg, cerr := g.config()
				if cerr != nil {
					return cerr
				}
				logger, closeLog := newClientLogger(dirs, cfg)
				if closeLog != nil {
					defer closeLog.Close()
				}
				return sendAndFollow(cmd.Context(), dirs, name, data, enter, state, timeout, raw, logger)
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				// The same wait `cm wait` issues, through the same server, so it is exposed the same way: a
				// --wait blocked or a --match against a server predating either runs the whole timeout and
				// reports nothing about why. One rule, in waitTarget.needsCapability, rather than a second
				// copy here that would drift from it.
				target := waitTarget{match: match, state: state, until: until}
				note, cerr := checkWaitCapability(ctx, cl, target)
				if cerr != nil {
					return cerr
				}
				target.note = note

				resp, err := cl.Send(ctx, &serverv1.SendRequest{
					Session:       name,
					Data:          []byte(data),
					Enter:         []byte(enter),
					WaitUntil:     state,
					Match:         match,
					MatchRaw:      matchRaw,
					WaitTimeoutMs: uint64(timeout.Milliseconds()),
				})
				if err != nil {
					return err
				}
				if resp.GetWait() == nil {
					return nil
				}
				// Described by what was waited for, so a timeout message names the pattern rather than
				// an empty state.
				return reportWait(os.Stdout, os.Stderr, name, target, resp.GetWait(), asJSON)
			})
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&newline, "enter", "n", false,
		"append a carriage return so the shell runs the input")
	f.StringArrayVar(&keys, "key", nil,
		"send a key rather than text: ctrl-c, enter, up, f5, alt-x (repeatable, in order)")
	f.StringVar(&until, "wait", "",
		"after sending, wait until the session is idle, busy, blocked, or exited")
	f.StringVar(&match, "match", "",
		"after sending, wait until this text appears in the output")
	f.BoolVar(&matchRaw, "match-raw", false,
		"match the bytes the program emitted rather than the text they rendered to")
	addTimeoutFlag(f, &timeout)
	f.BoolVar(&asJSON, "json", false, "print the wait result as JSON")
	f.BoolVarP(&follow, "follow", "f", false,
		"stream the session's output until the command finishes (implies --wait idle)")
	f.BoolVar(&raw, "raw", false,
		"with --follow, keep escape sequences instead of stripping them")
	return cmd
}
