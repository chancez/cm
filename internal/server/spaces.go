package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/chancez/cm/internal/spaces"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// SpaceSummary is a space with how many sessions are in it.
type SpaceSummary struct {
	store.Space
	Sessions int
	Running  int
}

// Spaces returns every space with its session counts, by name.
func (m *Manager) Spaces(ctx context.Context) ([]SpaceSummary, error) {
	all, err := m.store.Spaces(ctx)
	if err != nil {
		return nil, err
	}
	recs, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	index := make(map[string]int, len(all))
	out := make([]SpaceSummary, len(all))
	for i, sp := range all {
		out[i] = SpaceSummary{Space: sp}
		index[sp.Name] = i
	}
	for _, rec := range recs {
		i, ok := index[rec.Space]
		if !ok {
			// The store creates a space with the session that names it, so this is a row written outside
			// it. Counted under nothing rather than inventing a space the store does not hold.
			continue
		}
		out[i].Sessions++
		if rec.State == store.StateRunning {
			out[i].Running++
		}
	}
	return out, nil
}

// SpaceOf returns the space a session is in.
func (m *Manager) SpaceOf(ctx context.Context, id string) (string, error) {
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return rec.Space, nil
}

// Move puts a session in another space, returning the one it was in.
//
// Against the store, like SetTags, so an ended session can be moved too: a shelved session filed in the
// wrong space is exactly the one that is hard to find.
func (m *Manager) Move(ctx context.Context, id, space string) (string, error) {
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if err := m.store.Apply(ctx, id, store.Update{Space: &space}); err != nil {
		return "", err
	}
	return rec.Space, nil
}

// parentSpace is the space of the session a client says it runs inside, for a session it creates to
// inherit. Empty when it is inside none, or one this server cannot find, which then falls back to
// default rather than failing the attach.
func (s *Service) parentSpace(ctx context.Context, ref string) string {
	if ref == "" {
		return ""
	}
	id, err := s.mgr.Resolve(ctx, ref)
	if err != nil {
		return ""
	}
	space, err := s.mgr.SpaceOf(ctx, id)
	if err != nil {
		return ""
	}
	return space
}

// Spaces lists every space.
func (s *Service) Spaces(ctx context.Context, _ *serverv1.SpacesRequest) (*serverv1.SpacesResponse, error) {
	all, err := s.mgr.Spaces(ctx)
	if err != nil {
		return nil, err
	}
	resp := &serverv1.SpacesResponse{}
	for _, sp := range all {
		resp.Spaces = append(resp.Spaces, &serverv1.Space{
			Name:          sp.Name,
			CreatedAtUnix: sp.CreatedAt.Unix(),
			Sessions:      uint32(sp.Sessions),
			Running:       uint32(sp.Running),
		})
	}
	return resp, nil
}

// CreateSpace creates a space, so one can exist before anything is put in it.
func (s *Service) CreateSpace(ctx context.Context, req *serverv1.CreateSpaceRequest) (*serverv1.CreateSpaceResponse, error) {
	// Validated here as well as in the CLI, because this is the trust boundary: a space name is printed
	// to the terminal of whoever lists spaces.
	if err := spaces.ValidateName(req.Name); err != nil {
		return nil, err
	}
	if err := s.mgr.store.CreateSpace(ctx, req.Name); err != nil {
		return nil, err
	}
	return &serverv1.CreateSpaceResponse{}, nil
}

// RemoveSpace removes an empty space.
func (s *Service) RemoveSpace(ctx context.Context, req *serverv1.RemoveSpaceRequest) (*serverv1.RemoveSpaceResponse, error) {
	if err := spaces.ValidateName(req.Name); err != nil {
		return nil, err
	}
	if err := s.mgr.store.DeleteSpace(ctx, req.Name); err != nil {
		return nil, err
	}
	return &serverv1.RemoveSpaceResponse{}, nil
}

// Move puts sessions in another space, creating it if needed.
func (s *Service) Move(ctx context.Context, req *serverv1.MoveRequest) (*serverv1.MoveResponse, error) {
	if len(req.Sessions) == 0 {
		return nil, errors.New("a session name is required")
	}
	if err := spaces.ValidateName(req.Space); err != nil {
		return nil, err
	}
	resp := &serverv1.MoveResponse{
		Moved:  map[string]string{},
		Errors: map[string]string{},
	}
	for _, ref := range req.Sessions {
		id, err := s.mgr.Resolve(ctx, ref)
		if err != nil {
			resp.Errors[ref] = err.Error()
			continue
		}
		from, err := s.mgr.Move(ctx, id, req.Space)
		if err != nil {
			resp.Errors[ref] = err.Error()
			continue
		}
		resp.Moved[ref] = from
	}
	return resp, nil
}

// validateMeta checks a metadata request's space and key, here because this is the trust boundary: both
// are printed to the terminal of whoever lists them.
func validateMeta(space, key string) error {
	if err := spaces.ValidateName(space); err != nil {
		return err
	}
	return spaces.ValidateMetaKey(key)
}

// ListSpaceMeta lists a space's metadata keys.
func (s *Service) ListSpaceMeta(ctx context.Context, req *serverv1.ListSpaceMetaRequest) (*serverv1.ListSpaceMetaResponse, error) {
	if err := spaces.ValidateName(req.Space); err != nil {
		return nil, err
	}
	all, err := s.mgr.store.SpaceMetas(ctx, req.Space, req.Prefix)
	if err != nil {
		return nil, err
	}
	resp := &serverv1.ListSpaceMetaResponse{}
	for _, m := range all {
		resp.Entries = append(resp.Entries, spaceMetaEntry(m))
	}
	return resp, nil
}

// GetSpaceMeta returns one key's value.
func (s *Service) GetSpaceMeta(ctx context.Context, req *serverv1.GetSpaceMetaRequest) (*serverv1.GetSpaceMetaResponse, error) {
	if err := validateMeta(req.Space, req.Key); err != nil {
		return nil, err
	}
	m, err := s.mgr.store.GetSpaceMeta(ctx, req.Space, req.Key)
	if errors.Is(err, store.ErrMetaNotFound) {
		return &serverv1.GetSpaceMetaResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &serverv1.GetSpaceMetaResponse{Found: true, Entry: spaceMetaEntry(m)}, nil
}

// SetSpaceMeta writes one key's value.
func (s *Service) SetSpaceMeta(ctx context.Context, req *serverv1.SetSpaceMetaRequest) (*serverv1.SetSpaceMetaResponse, error) {
	if err := validateMeta(req.Space, req.Key); err != nil {
		return nil, err
	}
	if len(req.Value) > spaces.MaxMetaValueLen {
		return nil, fmt.Errorf("value is %d bytes, limit is %d", len(req.Value), spaces.MaxMetaValueLen)
	}
	rev, err := s.mgr.store.SetSpaceMeta(ctx, req.Space, req.Key, req.Value, req.IfRevision)
	if mismatch, ok := errors.AsType[*store.RevisionMismatchError](err); ok {
		return &serverv1.SetSpaceMetaResponse{Conflict: true, CurrentRevision: mismatch.Current}, nil
	}
	if err != nil {
		return nil, err
	}
	return &serverv1.SetSpaceMetaResponse{Revision: rev}, nil
}

// RemoveSpaceMeta removes one key.
func (s *Service) RemoveSpaceMeta(ctx context.Context, req *serverv1.RemoveSpaceMetaRequest) (*serverv1.RemoveSpaceMetaResponse, error) {
	if err := validateMeta(req.Space, req.Key); err != nil {
		return nil, err
	}
	removed, err := s.mgr.store.DeleteSpaceMeta(ctx, req.Space, req.Key, req.IfRevision)
	if mismatch, ok := errors.AsType[*store.RevisionMismatchError](err); ok {
		return &serverv1.RemoveSpaceMetaResponse{Conflict: true, CurrentRevision: mismatch.Current}, nil
	}
	if err != nil {
		return nil, err
	}
	return &serverv1.RemoveSpaceMetaResponse{Removed: removed}, nil
}

func spaceMetaEntry(m store.SpaceMeta) *serverv1.SpaceMetaEntry {
	return &serverv1.SpaceMetaEntry{
		Key:           m.Key,
		Value:         m.Value,
		Size:          uint32(m.Size),
		Revision:      m.Revision,
		UpdatedAtUnix: m.UpdatedAt.Unix(),
	}
}
