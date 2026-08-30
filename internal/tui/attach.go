package tui

import (
	"context"
	"io"

	tea "charm.land/bubbletea/v2"
)

// attachCommand hands the terminal to a session while the picker stands aside.
//
// A tea.ExecCommand rather than a plain tea.Cmd, and that distinction is the whole mechanism.
// bubbletea runs an ordinary Cmd in a goroutine while it keeps rendering and keeps reading input,
// which for an attachment would mean two programs writing to one terminal and both reading the
// keyboard. Exec instead pauses the program: it stops the input reader, stops the renderer, restores
// the terminal to how it was found, runs this, and only then resumes. That is what makes the
// one-writer-per-stream rule hold across the handoff. See the package comment.
//
// The result is read off the struct rather than returned through the ExecCallback, which only carries
// an error. A detach is not an error and neither is a shell exiting, so the outcome has to travel
// some other way.
type attachCommand struct {
	// ctx is the picker's context, so cancelling it ends an attachment too rather than leaving one
	// holding a terminal the program has stopped rendering to.
	ctx    context.Context
	attach AttachFunc
	// ref is the session to attach to, or empty to create one the server names.
	ref string

	// result and err are what Run produced, read by the callback that turns them into a message.
	result Attachment
	err    error
}

// SetStdin, SetStdout and SetStderr are deliberately empty.
//
// bubbletea offers the program's streams as io.Reader and io.Writer, and an attachment needs neither:
// it needs the *os.File this process was started with, because putting a terminal into raw mode and
// asking it its size are ioctls on a descriptor, not writes to a stream. Those are the same files
// bubbletea was using, since the picker was handed the real terminal or it would not be running, so
// there is nothing here to record. Ignoring them is safe only because Exec has already released the
// terminal by the time Run is called.
func (*attachCommand) SetStdin(io.Reader)  {}
func (*attachCommand) SetStdout(io.Writer) {}
func (*attachCommand) SetStderr(io.Writer) {}

// Run attaches, and blocks until the attachment ends.
func (a *attachCommand) Run() error {
	a.result, a.err = a.attach(a.ctx, a.ref)
	// Returned as well as recorded, because bubbletea logs a failing Exec. The message built by the
	// callback carries the same error, and that is the copy the user sees.
	return a.err
}

// attachedMsg reports an ended attachment back to the model.
type attachedMsg struct {
	// ref is what was asked for, so an error can name it. The Attachment names the session the server
	// resolved it to, which for a created session is a name this process had never seen.
	ref        string
	attachment Attachment
	err        error
}

// handOff builds the command that attaches to ref, or creates a session when ref is empty.
//
// The command is also recorded on the returned model. The callback needs it either way, to read the
// outcome off it, so keeping it on the model rather than only in that closure costs nothing and makes
// the decision inspectable: tea.Exec seals its command inside a message this package cannot open, and
// which reference the picker chose is exactly the thing worth being able to check, since attaching by
// name rather than by ID would follow a binding that may have moved.
func (m model) handOff(ref string) (model, tea.Cmd) {
	cmd := &attachCommand{ctx: m.ctx, attach: m.attach, ref: ref}
	m.handoff = cmd
	return m, tea.Exec(cmd, func(error) tea.Msg {
		return attachedMsg{ref: ref, attachment: cmd.result, err: cmd.err}
	})
}
