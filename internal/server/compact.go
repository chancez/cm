package server

import "time"

// compactInterval is how often a session offers its terminal model a chance to compress its scrollback.
//
// The model decides whether there is anything to do, and checking costs a look at each page without
// decompressing any, so this only bounds how soon after output stops the memory comes back.
const compactInterval = time.Second

// runCompactor compresses the session's scrollback once its output has been quiet, for as long as the
// session lasts.
//
// Uncompressed, scrollback costs its full width in cells whatever the lines hold: 100000 lines measured
// 235 MB resident at 200 columns, and 32 MB compressed.
func (s *Session) runCompactor() {
	t := time.NewTicker(compactInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		if !s.compact() {
			return
		}
	}
}

// compact runs compression steps until the model has none left, reporting false once compression should
// stop for good.
//
// One step at a time, each taking the model's lock on its own, so output arriving mid-pass waits for one
// step of about 100us rather than for the whole pass.
func (s *Session) compact() bool {
	for {
		s.mu.Lock()
		term := s.term
		s.mu.Unlock()
		if term == nil {
			return true
		}
		more, err := term.Compact()
		if err != nil {
			// Stopped rather than retried: compression changes only how the model stores scrollback, so the
			// cost of giving up is memory, and a failure every second would be a log line every second.
			s.log.Warn("compressing scrollback failed, leaving it uncompressed", "err", err)
			return false
		}
		if !more {
			return true
		}
		select {
		case <-s.done:
			return false
		default:
		}
	}
}
