//go:build !cgo

// Package vt wraps libghostty-vt.
//
// This file stands in when cgo is unavailable. The emulator cannot be built without it, so terminal
// state, screen restore on reattach, and history rendering are absent, while sessions, attach,
// detach, and persistence all still work.
//
// It exists because the claim that cgo is confined to this package should be verifiable rather than
// asserted. Without a stub, a CGO_ENABLED=0 build fails here and the confinement is untested.
package vt

import "errors"

// Available reports whether the terminal emulator was compiled in.
//
// Callers check this rather than calling and handling the error, because "no emulator" is a
// build-wide fact rather than a per-call failure, and the sensible response is to run without a
// terminal model instead of failing every session.
const Available = false

// ErrUnavailable reports that cm was built without the terminal emulator.
var ErrUnavailable = errors.New(
	"cm was built without cgo, so terminal state and screen restore are unavailable")

// SessionTerminal is the unavailable stand-in for the emulator-backed terminal model.
type SessionTerminal struct{}

// NewSessionTerminal always fails, so a caller falls back to running without a terminal model rather
// than failing to start a session.
func NewSessionTerminal(rows, cols uint16, scrollbackLines int) (*SessionTerminal, error) {
	return nil, ErrUnavailable
}

func (s *SessionTerminal) Write(p []byte) error           { return ErrUnavailable }
func (s *SessionTerminal) Restore() ([]byte, error)       { return nil, ErrUnavailable }
func (s *SessionTerminal) Resize(rows, cols uint16) error { return ErrUnavailable }
func (s *SessionTerminal) TakePending() [][]byte          { return nil }
func (s *SessionTerminal) Title() string                  { return "" }
func (s *SessionTerminal) Pwd() string                    { return "" }
func (s *SessionTerminal) FocusReporting() bool           { return false }
func (s *SessionTerminal) Plain() ([]byte, error)         { return nil, ErrUnavailable }
func (s *SessionTerminal) VT() ([]byte, error)            { return nil, ErrUnavailable }
func (s *SessionTerminal) HTML() ([]byte, error)          { return nil, ErrUnavailable }
func (s *SessionTerminal) Close() error                   { return nil }
