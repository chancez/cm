package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
)

// idAlphabet is what a session ID is spelled with.
//
// Lowercase, no vowels, and none of the glyph pairs that get misread: no 0 against o, no 1 against l
// or i. An ID is read off a session listing and typed back, and it also becomes a filename, so the
// alphabet is chosen for being unambiguous on a screen rather than for density. Dropping the vowels
// means an ID cannot spell a word, which matters for something printed in a listing.
//
// 'i' being absent is load-bearing beyond legibility. The migration that introduced IDs backfills the
// sessions that predate them with "mig" plus a counter, and no generated ID can begin that way, so the
// two can never collide without any check at migration time.
const idAlphabet = "23456789abcdefghjkmnpqrstvwxyz"

// idLen is how many characters an ID has.
//
// 30^8 is 6.6e11, so at ten thousand sessions over a machine's lifetime the chance that any two
// collide is about 1 in 13000, and a collision is caught by the primary key rather than accepted, so
// it is not a correctness risk at all.
//
// Random rather than a counter, which is the part worth keeping. A counter would live in this database,
// so losing or replacing the state directory restarts it, and an ID recorded outside cm would then
// resolve to an unrelated session. Two servers with separate state directories would hand out the same
// low numbers as well. That silent wrong-session resolution is the one failure an identity has to rule
// out, and it is the same reason names are never reused. A random ID that outlives its database fails
// to resolve instead, which is the correct outcome.
const idLen = 8

// NewID returns an ID no session in this database is using.
func (s *Store) NewID(ctx context.Context) (string, error) {
	source := s.IDSource
	if source == nil {
		source = generateID
	}
	// Bounded rather than looping until it works. With a working generator the first attempt succeeds
	// essentially always, so an unbounded loop would turn a broken IDSource into a hang instead of an
	// error naming what happened.
	for range 8 {
		id, err := source()
		if err != nil {
			return "", err
		}
		var one int
		err = s.db.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return id, nil
		}
		if err != nil {
			return "", fmt.Errorf("checking whether session ID %s is free: %w", id, err)
		}
	}
	return "", errors.New("could not allocate a free session ID in 8 attempts")
}

// generateID draws one ID from the alphabet.
func generateID() (string, error) {
	// The largest multiple of the alphabet size that fits in a byte. Values above it are rejected
	// rather than taken modulo, which would make the first 16 characters of a 30-character alphabet
	// slightly likelier than the rest. The bias would be harmless here, and rejecting costs nothing.
	//
	// Compared as an int, not a byte: an alphabet whose size divides 256 puts the limit at 256, and
	// byte(256) is 0, which would reject every draw and spin forever.
	limit := 256 - (256 % len(idAlphabet))

	out := make([]byte, 0, idLen)
	buf := make([]byte, idLen)
	for len(out) < idLen {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generating a session ID: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, idAlphabet[int(b)%len(idAlphabet)])
			if len(out) == idLen {
				break
			}
		}
	}
	return string(out), nil
}
