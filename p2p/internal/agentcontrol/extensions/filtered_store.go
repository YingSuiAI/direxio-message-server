package extensions

import (
	"context"
	"sort"
	"strings"
)

// FilteredStore extends Store with owner-scoped, source/state filtered
// pagination.  The token is the last installation_id returned by the prior
// page, so ordering remains stable while callers advance through the list.
type FilteredStore interface {
	ListFiltered(context.Context, string, int, string, string, string) ([]Installation, string, error)
}

// ValidateListFilter rejects values that are not part of the persisted wire
// contract. Empty source/state values mean no filter.
func ValidateListFilter(source, state string) error {
	switch strings.TrimSpace(source) {
	case "", "official_registry", "smithery", "glama", "github":
	default:
		return ErrInvalid
	}
	switch strings.TrimSpace(state) {
	case "", "draft", "installing", "installed", "updating", "uninstalling", "removed", "failed":
	default:
		return ErrInvalid
	}
	return nil
}

// ListFiltered implements FilteredStore for the in-memory lifecycle store.
// It deliberately filters after taking the same lock used by List/Get, and
// returns cloned installations so callers cannot mutate store state.
func (s *MemoryStore) ListFiltered(_ context.Context, owner string, limit int, token, source, state string) ([]Installation, string, error) {
	if limit <= 0 || limit > 100 || strings.TrimSpace(owner) == "" {
		return nil, "", ErrInvalid
	}
	if err := ValidateListFilter(source, state); err != nil {
		return nil, "", err
	}
	token, source, state = strings.TrimSpace(token), strings.TrimSpace(source), strings.TrimSpace(state)
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := owner + "\x00"
	out := make([]Installation, 0)
	for key, item := range s.items {
		if !strings.HasPrefix(key, prefix) || item.ID <= token || (source != "" && item.Candidate.Source != source) || (state != "" && item.State != state) {
			continue
		}
		out = append(out, cloneInstallation(item))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		next := out[limit-1].ID
		return out[:limit], next, nil
	}
	return out, "", nil
}

var _ FilteredStore = (*MemoryStore)(nil)
