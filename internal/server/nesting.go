package server

import "context"

// hostingParent returns the session a nested client is running inside, when this server has it.
//
// insideRef is what the client sent as Open.inside_session, and ownID the session it is attaching to.
// Reporting (nil, false) means nothing is nested as far as this server is concerned, which is the
// ordinary case for every attach from a real terminal.
//
// A *reference* rather than an ID, and that is the bug this function exists to have one place for. The
// client sends the value of CM_SESSION, which is the ID with its sigil, so a lookup by ID found nothing
// and the whole nesting mechanism was dead in a shipped binary while every test passed: the unit tests
// call beginHosting directly, and the one covering the client's side used a name-shaped value. `cm list`
// never showed "(hosting ...)" and a parent went on recording its child's directory as its own. Only an
// end-to-end run with a real nested attach showed it, which is TestDetachKeyLeavesTheInnermostSession.
//
// Resolve takes either spelling, which also covers a session created by an older server: those exported
// CM_SESSION as a name, and a name is what Resolve looks up in the bindings.
//
// A failure to resolve is not an error worth failing an attach over. The parent may have exited, or
// belong to a different server, or be named by a binding this one has never had. Nothing is frozen in
// that case, which is correct, because there is no parent here whose bookkeeping could be wrong.
func (s *Service) hostingParent(ctx context.Context, insideRef, ownID string) (*Session, bool) {
	if insideRef == "" {
		return nil, false
	}
	parentID, err := s.mgr.Resolve(ctx, insideRef)
	if err != nil {
		return nil, false
	}
	// Attaching to the session you are already inside is not nesting. Compared after resolving, since
	// one side is a reference and the other an ID: comparing the two spellings never matched, so this
	// guard did nothing.
	if parentID == ownID {
		return nil, false
	}
	parent, live := s.mgr.Get(parentID)
	if !live {
		return nil, false
	}
	return parent, true
}
