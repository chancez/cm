package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chancez/cm/internal/vt"
)

const (
	// historyPageRows is a page when the client asks for none: about 120 KB of text at 200 columns, and
	// about 0.5ms holding the session's model.
	historyPageRows = 1000
	// historyPageMaxRows caps a page, so one request cannot hold the model the way a whole render does.
	historyPageMaxRows = 10000
	// historyCursorIdle is how long a cursor nobody pages lives. A walk pages back to back, so this only
	// matters for a client that died mid-walk: its cursor holds a position in a live session, or for a
	// finished session a whole replayed model.
	historyCursorIdle = 30 * time.Second
	// maxHistoryCursorsPerSession bounds open walks of one session. Each live one is a tracked reference
	// libghostty updates as the page list changes, so they are cheap but not free.
	maxHistoryCursorsPerSession = 8
)

// errUnknownCursor reports a cursor this server is not holding: never issued, finished, or expired.
var errUnknownCursor = errors.New("unknown history cursor: it finished, expired after 30 seconds unused, " +
	"or belongs to a server that has since restarted; start again from the top")

// historyCursor is one walk in progress.
type historyCursor struct {
	session string
	timer   *time.Timer

	mu     sync.Mutex
	reader vt.HistoryReader
	// owned is the replayed model a finished session's walk reads, closed with the cursor. Nil for a live
	// session, whose model belongs to the session.
	owned  Terminal
	closed bool
}

func (c *historyCursor) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.reader != nil {
		c.reader.Close()
	}
	if c.owned != nil {
		c.owned.Close()
	}
}

// historyCursors is the set of open walks, usable as a zero value.
type historyCursors struct {
	mu      sync.Mutex
	byToken map[string]*historyCursor
}

func (h *historyCursors) add(c *historyCursor) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	open := 0
	for _, other := range h.byToken {
		if other.session == c.session {
			open++
		}
	}
	if open >= maxHistoryCursorsPerSession {
		return "", fmt.Errorf("session %s already has %d history reads open; each expires after %s unused",
			c.session, open, historyCursorIdle)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	if h.byToken == nil {
		h.byToken = make(map[string]*historyCursor)
	}
	h.byToken[token] = c
	c.timer = time.AfterFunc(historyCursorIdle, func() { h.drop(token) })
	return token, nil
}

func (h *historyCursors) get(token string) *historyCursor {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.byToken[token]
	if c != nil {
		c.timer.Reset(historyCursorIdle)
	}
	return c
}

func (h *historyCursors) drop(token string) {
	h.mu.Lock()
	c := h.byToken[token]
	delete(h.byToken, token)
	h.mu.Unlock()
	if c != nil {
		c.timer.Stop()
		c.close()
	}
}

func (h *historyCursors) closeAll() {
	h.mu.Lock()
	all := h.byToken
	h.byToken = nil
	h.mu.Unlock()
	for _, c := range all {
		c.timer.Stop()
		c.close()
	}
}

// HistoryPage is one page of a session's contents, and how to ask for the next.
type HistoryPage struct {
	Data      []byte
	Cursor    string
	Done      bool
	Discarded bool
}

// HistoryPage returns the next page of a walk of a session's contents, starting one when cursor is empty.
//
// The walk's position is held here rather than handed to the client, because the only position that
// survives output, pruning, and a resize is a libghostty tracked reference. See vt.SessionTerminal.OpenHistory.
// sess is the session when it is live; nil reads a finished session's persisted output, replayed once into a
// model the cursor keeps for the rest of the walk instead of once a page.
func (m *Manager) HistoryPage(ctx context.Context, id string, sess *Session, cursor string, rows int) (HistoryPage, error) {
	if rows <= 0 {
		rows = historyPageRows
	}
	rows = min(rows, historyPageMaxRows)

	if cursor == "" {
		c := &historyCursor{session: id}
		if sess != nil {
			r, err := sess.OpenHistory()
			if err != nil {
				return HistoryPage{}, err
			}
			c.reader = r
		} else {
			term, err := m.replayTerminal(ctx, id)
			if err != nil {
				return HistoryPage{}, err
			}
			if term != nil {
				r, err := term.OpenHistory()
				if err != nil {
					term.Close()
					return HistoryPage{}, err
				}
				c.reader, c.owned = r, term
			}
		}
		if c.reader == nil {
			// No model, or an empty log: nothing to walk, which is a complete answer.
			c.close()
			return HistoryPage{Done: true}, nil
		}
		token, err := m.cursors.add(c)
		if err != nil {
			c.close()
			return HistoryPage{}, err
		}
		cursor = token
	}

	c := m.cursors.get(cursor)
	if c == nil || c.session != id {
		return HistoryPage{}, errUnknownCursor
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return HistoryPage{}, errUnknownCursor
	}
	data, done, err := c.reader.Next(rows)
	c.mu.Unlock()
	switch {
	case errors.Is(err, vt.ErrHistoryDiscarded):
		m.cursors.drop(cursor)
		return HistoryPage{Discarded: true}, nil
	case err != nil:
		m.cursors.drop(cursor)
		return HistoryPage{}, err
	case done:
		m.cursors.drop(cursor)
		return HistoryPage{Data: data, Done: true}, nil
	}
	return HistoryPage{Data: data, Cursor: cursor}, nil
}
