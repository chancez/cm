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
// terminal and internal/client holds nothing. While an attachment is live, bubbletea has released
// the terminal, stopped reading input, and is not rendering, and internal/client owns every byte.
// tea.Exec is what makes the handoff strict: it pauses the program, runs the attachment to
// completion, and resumes. See attachCommand.
//
// The consequence for anything added here: an attachment must not print. A message written to
// stderr as the attachment ends lands on a screen bubbletea is about to repaint from its own model,
// so it either flickers away or corrupts the frame. Anything worth saying goes in the status line,
// which is part of the frame.
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
}

// AttachFunc attaches this terminal to a session and returns when the attachment ends.
//
// ref is a session reference, or empty to create a session the server names, which is what `cm
// attach` with no argument does.
//
// Called while bubbletea has released the terminal, so the implementation is free to put it in raw
// mode and paint a session. It must not print anything: see the package comment.
//
// A function rather than internal/client.Attach directly, for two reasons. It keeps this package
// clear of terminal ownership, so its tests need no pty. And it keeps the dependency pointing one
// way: the command layer knows how to build a client's options from flags and config, and this
// package should not learn that.
type AttachFunc func(ctx context.Context, ref string) (Attachment, error)

// Attachment is how an attachment ended, in the terms the picker has something to say about.
//
// Its own type rather than internal/client.Result, which carries an upgrade request and a resume
// position that mean nothing here. See Options.Attach for what the picker does with an upgrade.
type Attachment struct {
	// Session is what to call the session in a message: the name it was attached by, or its ID
	// reference when it has no name.
	Session string
	// Detached is true when the user detached rather than the session ending.
	Detached bool
	// Exited is true when the session's shell exited.
	Exited bool
	// ExitCode is the shell's status, and is meaningless unless Exited.
	ExitCode int
	// Stale is true when the server asked this client to come back on a newer build.
	//
	// `cm attach` answers that by replacing its own process, which keeps the window on the session
	// with the screen untouched. The picker cannot: exec would replace the picker too, and the list
	// this attachment came back to would be gone. So it is reported instead, and quitting and
	// reopening is what picks up the new build. See docs/tui.md.
	Stale bool
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
