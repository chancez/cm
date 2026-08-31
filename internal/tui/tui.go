// Package tui is the session picker: a list of sessions to attach to, which an attachment returns
// to when it detaches.
//
// What it is for is a window that has no session yet. `cm ls` says what exists and `cm attach` goes
// somewhere, and between them is the thing a person actually does, which is look at what is running
// and decide. A picker that the attachment comes back to makes that one place rather than two
// commands and a name typed from memory.
//
// # Why this package owns the terminal, and how that stays legal
//
// AGENTS.md's rule is exactly one writer per shared byte stream, and the stream here is the user's
// terminal. There are now two things with something to say to it: this package, through bubbletea,
// and internal/client, which paints a session. Both write escape sequences, and a byte from one
// landing inside a sequence from the other is the rendering bug that
// cmd/cm.TestCommandLayerWritesNoEscapeSequences exists to prevent.
//
// They are kept apart in time rather than merged. While the list is up, this package owns the
// terminal and no client holds it. While an attachment is live, bubbletea has released the terminal,
// stopped reading input, and is not rendering, and the client owns every byte. tea.Exec is what makes
// the handoff strict: it pauses the program, runs the attachment to completion, and resumes. See
// attachCommand.
//
// The attachment is a `cm attach` child process rather than a call into internal/client, and that is
// not a matter of taste. internal/client.readInput leaves its reader blocked in the kernel on purpose,
// because a blocked read cannot be cancelled; `cm attach` exits immediately afterwards so nothing
// notices. In-process, that leftover reader stays on the terminal and steals exactly one keystroke
// from the picker per attachment, measured: attach, detach, then a single "/" does nothing while every
// key after it works. Closing the descriptor does not help, since Go defers the real close until the
// outstanding read finishes. A child process takes its leftovers with it when it exits.
//
// The consequence for anything added here: nothing in this process may print. A message written to
// stderr as the attachment ends lands on a screen bubbletea is about to repaint from its own model, so
// it either flickers away or corrupts the frame. The child's own output is captured and shown in the
// status line, which is part of the frame. See Attachment.Note.
package tui

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// refreshInterval is how often the list re-reads the server.
//
// A poll rather than a subscription because the server has no watch RPC: List is the only way to ask
// what exists. One second is chosen against what the list is for, which is deciding where to go: the
// state column changes when a command starts or finishes, and a person scanning rows notices a stale
// "running(nvim)" within about a second. It costs one request on an already-open connection, which is
// nothing like the 23ms a fresh `cm` invocation costs, since neither a process nor a connection is
// created.
const refreshInterval = time.Second

// Sessions is the part of the server's API the picker uses.
//
// An interface rather than the generated client so a test can drive the model without a server, a
// socket, or a session: everything below the keypress is then an ordinary function call. serverv1.
// ServerClient satisfies it as it stands, so the real caller passes the client straight in and there
// is no wrapper to keep in step.
//
// Deliberately the smallest set that covers the actions. Adding an RPC here is the signal that the
// picker is growing another verb, which is worth noticing rather than doing by reflex.
type Sessions interface {
	List(context.Context, *serverv1.ListRequest) (*serverv1.ListResponse, error)
	Kill(context.Context, *serverv1.KillRequest) (*serverv1.KillResponse, error)
	Bind(context.Context, *serverv1.BindRequest) (*serverv1.BindResponse, error)
	Unbind(context.Context, *serverv1.UnbindRequest) (*serverv1.UnbindResponse, error)
	// Read is the preview pane's source: the plain form renders the session's cells to text, so no
	// escape sequence from the program reaches the frame. The raw form would, which is why it is not
	// used here. See previewLines.
	Read(context.Context, *serverv1.ReadRequest) (*serverv1.ReadResponse, error)
}

// AttachFunc gives this terminal to a session and returns when the attachment ends.
//
// ref is a session reference, or empty to create a session the server names, which is what `cm
// attach` with no argument does.
//
// Called while bubbletea has released the terminal, so the implementation is free to run something
// that takes it over completely. See the package comment for why that is a child process.
//
// A function rather than the command built here, so this package needs no view on how a client is
// configured: which flags were set and what the config file says belong to the command layer, and a
// test needs neither.
type AttachFunc func(ctx context.Context, ref string) (Attachment, error)

// Attachment is what an attachment left behind.
//
// One field, because the picker has one thing to do with the outcome: say it. The attachment is a
// child process, so what it has to say arrives as the text it printed rather than as fields to
// re-render. `cm attach` already distinguishes a detach from a session that ended and from one that
// ended unexpectedly, and saying the same things again in this package's own wording is how the two
// drift apart.
type Attachment struct {
	// Note is what the attachment printed on the way out, shown verbatim in the status line, or empty
	// when it said nothing.
	Note string
}

// Options configures a picker.
type Options struct {
	// Sessions is the server to ask. Required.
	Sessions Sessions
	// Attach hands the terminal to a session. Required.
	Attach AttachFunc
	// Tags filters the list, in the form `cm ls --tag` takes. Applied on the server for every
	// refresh, so a session that gains a matching tag appears without a restart.
	Tags []string
	// Preview starts the picker with the output pane open.
	Preview bool
	// Refresh is how often the list re-reads the server. Nil means refreshInterval.
	//
	// A pointer so that zero can mean something: no polling at all, refreshing only after an action.
	// That is what a test wants, since a timer in a unit test is a second of waiting or a race, and it
	// is a coherent setting in its own right for someone who would rather the list held still.
	Refresh *time.Duration
	// Notice is shown in the status line at startup, for something true about this invocation rather
	// than about a session. The picker running inside a session is the case it exists for: attaching
	// from there nests, and the detach key belongs to the outermost client, so detaching lands
	// somewhere the user did not expect and looks like a bug in cm.
	Notice string
}

// Run shows the picker until the user quits.
//
// ctx cancels the program, which is how a signal ends it: bubbletea restores the terminal on the way
// out, so a cancelled context leaves a usable shell rather than one in raw mode.
func Run(ctx context.Context, opts Options) error {
	if opts.Sessions == nil || opts.Attach == nil {
		return errors.New("a picker needs a server and a way to attach")
	}
	_, err := tea.NewProgram(newModel(ctx, opts), tea.WithContext(ctx)).Run()
	// A cancelled context is how the picker is asked to stop, so it is not a failure to report. Left
	// unwrapped otherwise: bubbletea's errors name the terminal operation that failed, which is what
	// the user needs to see.
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
