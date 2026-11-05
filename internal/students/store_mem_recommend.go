package students

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"uuid"
)

// PopularClubs implements recommendStore.
func (s *MemStore) PopularClubs(_ context.Context, dim string, minimum, limit int) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	for _, p := range s.profiles {
		if !p.AudienceEligible || !slices.Contains(p.AudienceKeys, dim) {
			continue
		}
		for _, k := range p.AudienceKeys {
			if strings.HasPrefix(k, keyClub) {
				counts[k]++
			}
		}
	}
	keys := make([]string, 0, len(counts))
	for k, n := range counts {
		if n >= minimum {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(cmp.Compare(counts[b], counts[a]), strings.Compare(a, b))
	})
	out := []uuid.UUID{}
	for _, k := range keys[:min(limit, len(keys))] {
		if id, err := uuid.Parse(strings.TrimPrefix(k, keyClub)); err == nil {
			out = append(out, id)
		}
	}
	return out, nil
}
