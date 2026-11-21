package client

import (
	"context"
	"errors"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// newSessionOpen is the Open the overlay's new-session key sends: a session the server names, in dir.
//
// Built from this client's options, so the environment and the client's description are the same as any
// attach, and then stripped of everything that describes *this* attachment rather than a session in
// general. The command, the tags, persist, on_restore and if_running were all given for the session this
// window was opened on, and a new session inheriting them would be that window's `-- nvim` running again
// somewhere nobody asked. The outstanding queries belong to this attachment's program and must not be
// handed to a session that never asked them, and Open *consumes* them, so the pending set is detached from
// the copy first: otherwise building this Open would lose the questions this attachment re-offers on its
// next reconnect.
//
// dir is the current session's directory, which is what "a new session here" means, or empty for the
// directory this client was started in. Explicit when given, so the server does not treat it as a guess.
// space is the current session's space, for the same reason, or empty to let the server decide.
func newSessionOpen(opts Options, dir, space string, rows, cols uint16) *serverv1.Open {
	opts.pending = nil
	open := opts.Open("")
	open.Command = nil
	open.Tags = nil
	// The current session's rather than this window's --space, which named the session the window was opened
	// on: after a switch or a move, "here" is somewhere else.
	open.Space = space
	open.Persist = false
	open.OnRestore = ""
	open.IfRunning = serverv1.IfRunning_IF_RUNNING_UNSPECIFIED
	open.TerminalKittyGraphics = false
	if dir != "" {
		open.Cwd, open.CwdExplicit = dir, true
	}
	open.Rows, open.Cols = uint32(rows), uint32(cols)
	// Created and left, as `cm attach --no-attach` does: the switch that follows is the attachment.
	open.NoAttach = true
	open.NoRestore = true
	return open
}

// sessionDir is the directory a new session made from currentID should start in, or empty for the
// client's own.
//
// Only a local one. A session that has ssh'd elsewhere reports a path on the other machine, which would be
// a directory this machine either does not have or has by coincidence. See pickDir.
func sessionDir(resp *serverv1.ListResponse, currentID string) string {
	for _, s := range resp.GetSessions() {
		if s.GetId() == currentID && s.GetCwdIsLocal() {
			return s.GetCwd()
		}
	}
	return ""
}

// sessionSpace is the space of the session with currentID, or empty when the list does not hold it, which
// leaves the choice to the server.
func sessionSpace(resp *serverv1.ListResponse, currentID string) string {
	for _, s := range resp.GetSessions() {
		if s.GetId() == currentID {
			return s.GetSpace()
		}
	}
	return ""
}

// createSession sends open on a stream of its own and returns the new session as an ID reference.
//
// A second stream on the connection this client already holds, which ttrpc multiplexes, so the attachment
// on screen is not disturbed while the session is made.
func createSession(ctx context.Context, cl serverv1.ServerClient, open *serverv1.Open) (string, error) {
	stream, err := cl.Attach(ctx)
	if err != nil {
		return "", err
	}
	if err := stream.Send(&serverv1.AttachRequest{Event: &serverv1.AttachRequest_Open{Open: open}}); err != nil {
		return "", err
	}
	resp, err := stream.Recv()
	if err != nil {
		return "", err
	}
	opened := resp.GetOpened()
	if opened == nil || opened.GetSessionId() == "" {
		return "", errors.New("the server did not open a session")
	}
	_ = stream.Send(&serverv1.AttachRequest{
		Event: &serverv1.AttachRequest_Detach{Detach: &serverv1.Detach{NoAck: true}},
	})
	DrainAfterDetach(stream)
	return paths.FormatSessionID(opened.GetSessionId()), nil
}
