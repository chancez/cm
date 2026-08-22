package server

import (
	"context"
	"errors"

	"github.com/chancez/cm/internal/tags"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Tag sets or removes a session's tags.
//
// Works against the store rather than the live registry, which is what distinguishes this from
// Report. A report describes a running program and is held in memory, so reporting on a session that
// is not running is meaningless. A tag describes the session itself, so an exited-but-recorded
// session can still be tagged: labelling a finished run to keep track of it is a reasonable thing to
// want, and refusing would be a rule with no purpose behind it.
func (s *Service) Tag(ctx context.Context, req *serverv1.TagRequest) (*serverv1.TagResponse, error) {
	if req.Session == "" {
		return nil, errors.New("a session name is required")
	}
	// Nothing to do is an error rather than a silent success. A caller that meant to change something
	// and passed no tags has a bug, and reporting the current set back as if the call worked would
	// hide it.
	if len(req.Set) == 0 && len(req.Remove) == 0 && !req.Replace {
		return nil, errors.New("no tags to set or remove")
	}

	// Validated server-side because this is the trust boundary. A tag reaching the store unchecked
	// would be printed to the terminal of whoever runs `cm list`, which is what the character set
	// exists to prevent.
	if err := tags.Validate(req.Set); err != nil {
		return nil, err
	}
	for _, key := range req.Remove {
		if err := tags.ValidateKey(key); err != nil {
			return nil, err
		}
	}

	id, err := s.mgr.Resolve(ctx, req.Session)
	if err != nil {
		return nil, err
	}
	result, err := s.mgr.SetTags(ctx, id, req.Set, req.Remove, req.Replace)
	if err != nil {
		return nil, err
	}
	return &serverv1.TagResponse{Tags: result}, nil
}
