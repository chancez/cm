package client

import (
	"log/slog"
	"time"

	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/input"
)

// probeGraphicsWindow bounds how long the probe's answer is still expected.
//
// Ten seconds, which is `kitten icat`'s own --detection-timeout default, read from kittens/icat/detect.go
// rather than picked: that is the reference implementation of this exact question, and it errors out rather
// than proceeding when the wait expires. Nothing here waits, so this window costs no latency at all; it only
// decides how long a reply is still recognised as cm's rather than treated as typing.
//
// Generous on purpose. A bound that is too small is the mistake docs/architecture.md records against tmux's
// 500ms escape-time, and the first version of this code made it: 250ms, chosen for the attach path, which on
// a mobile ssh link is not a generous round trip. The window is safe to make large precisely because it no
// longer gates anything, and because what it holds is a reply nobody else can claim.
const probeGraphicsWindow = 10 * time.Second

// graphicsProbe asks a terminal whether it can draw images and recognises the answer whenever it arrives.
//
// cm re-transmits stored images to an attaching client, which makes cm a sender under the graphics protocol,
// and a sender asks first: that is why `kitten icat` over ssh to a plain terminal sends no payload at all. cm
// did not ask, so a terminal that cannot draw an image was sent one and printed the base64 as text.
//
// The question goes out before the first Open and nothing waits for it. That is the whole shape, and it is
// what a blocking read got wrong: the answer decides whether images are sent, but it does not have to decide
// it *now*, because the server can send them later. So the attach proceeds immediately, and a slow but
// capable terminal gets its images one round trip behind its text rather than never.
//
// The answer is matched rather than swallowed, using the same ReplyFramer the server uses on this stream one
// hop later. A client that ate anything reply-shaped would break a program inside the session that
// legitimately asks the terminal the same questions; what makes this safe is that the reply is claimed only
// while cm's own question is outstanding, and that during the pre-Open window there is no program stream for
// it to belong to.
type graphicsProbe struct {
	// asked is when the question went out, so the window can expire. Zero means never asked, which is a
	// resume or a client with no terminal.
	asked time.Time
	// framer holds a reply split across reads, which a pty guarantees: reads cap at 1022 bytes and nothing
	// aligns them to a sequence.
	framer input.ReplyFramer
	// settled records that the answer is decided, so it is reported once and not revised. Two replies arrive
	// for one question and either can decide it, which is where this went wrong: a terminal answers the
	// graphics query and then answers the device attributes query, and treating the second as the negative
	// answer turned every yes into a no. Nothing drew, and both replies looked correct in isolation.
	settled bool
	// gotDA records that the device attributes reply has been consumed. That reply is the last thing this
	// exchange expects, so it is also what ends it: after it, a reply belongs to whoever asked next.
	gotDA bool
	// log is where a discarded or unexpected reply is recorded, since an image that does not appear is
	// otherwise inexplicable.
	log *slog.Logger
}

// ask writes the question to the terminal.
//
// Through the screen because everything for a terminal goes through the one writer, and as an injection
// because that is what it is: bytes cm produced rather than the session's.
//
// Safe to write before knowing the answer, which is worth stating because it looks circular. A terminal that
// cannot parse an APC prints about forty bytes of this command, and the repaint that follows a fresh attach
// opens by clearing the screen, so those bytes are erased before anyone sees them. A client that gets no
// repaint must therefore not ask, which is why a resume does not.
func (p *graphicsProbe) ask(scr *screen, log *slog.Logger) {
	p.log = log
	if err := scr.inject(graphics.ProbeCommands()); err != nil {
		// A terminal that cannot be written to will not be shown images either, so this is not worth
		// failing an attach over.
		log.Debug("writing the graphics probe failed, assuming no image support", "error", err)
		return
	}
	p.asked = time.Now()
}

// take removes this probe's answer from a chunk of terminal input.
//
// Returns what remains, which the caller must forward: those are keystrokes. Also reports whether the answer
// just arrived and whether it says yes, so the caller can tell the server.
//
// Everything is passed through untouched once the exchange is over or the window has expired, which is the
// property that keeps a program's own replies working: cm claims a reply only while its own question is
// outstanding.
func (p *graphicsProbe) take(data []byte, now time.Time) (rest []byte, answered, draws bool) {
	if p.asked.IsZero() || p.gotDA || now.Sub(p.asked) > probeGraphicsWindow {
		return data, false, false
	}

	for _, part := range p.framer.Split(data, now, true) {
		if !part.Reply {
			rest = append(rest, part.Data...)
			continue
		}
		// A reply while cm's question is outstanding. Two of them arrive for one question and both are
		// consumed, because cm asked both: the graphics response, which says yes or no, and the device
		// attributes reply, which every terminal sends and which therefore says the terminal has finished
		// answering. Whichever comes first decides; the other is swallowed without revising the decision.
		// Revising it is the bug this shape exists to prevent, since a terminal that can draw images sends
		// the OK and then the DA reply back to back, in one read.
		if cmd, n, ok := graphics.Parse(part.Data); ok && n == len(part.Data) {
			if isAnswer, yes := graphics.IsProbeAnswer(cmd); isAnswer {
				if !p.settled {
					p.settled, answered, draws = true, true, yes
				}
				continue
			}
		}
		if graphics.DeviceAttributesEnd(part.Data) == len(part.Data) {
			// Consumed either way, and the end of the exchange. Arriving with nothing before it is the
			// negative answer, which is the reason the query is paired with primary DA at all: a terminal
			// with no graphics support says nothing to `a=q`, so waiting for its silence to elapse would
			// otherwise cost a timeout.
			p.gotDA = true
			if !p.settled {
				p.settled, answered, draws = true, true, false
			}
			continue
		}
		// A reply cm did not ask for. Forwarded rather than dropped: during this window a program may
		// already be running and asking its own questions, and swallowing one truncates its conversation.
		p.log.Debug("forwarding a reply that is not the graphics probe's answer", "bytes", len(part.Data))
		rest = append(rest, part.Data...)
	}
	return rest, answered, draws
}
