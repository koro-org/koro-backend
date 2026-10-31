package entitlements

import (
	"context"
	"fmt"
	"time"
	"uuid"
)

// Statistics is the package usage read of a company's panel (stats,
// BE-163; SR §8 "paket kullanımı"): per feature, the units of the rights
// usable now and how many of them are spent. It is a system read without
// authorisation: the caller authorises its own read (stats.company.read).
type Statistics interface {
	// Usage returns every feature of orgID in Features() order, a feature
	// without a usable right with zero figures.
	Usage(ctx context.Context, orgID uuid.UUID) ([]FeatureUsage, error)
}

// FeatureUsage is one feature of an org's package usage over the rights
// usable now (Usable: active, in their validity window, units left for a
// quota feature). Quota features count units (Total, Used, Remaining);
// discovery_access has AccessUntil instead.
type FeatureUsage struct {
	Feature     Feature
	Total       int
	Used        int
	Remaining   int
	AccessUntil *time.Time
}

var _ Statistics = (*service)(nil)

// Usage implements Statistics over the store's Usable read.
func (s *service) Usage(ctx context.Context, orgID uuid.UUID) ([]FeatureUsage, error) {
	now := s.d.Clock.Now()
	out := make([]FeatureUsage, 0, len(Features()))
	for _, f := range Features() {
		rows, err := s.d.Store.Usable(ctx, orgID, f, now)
		if err != nil {
			return nil, fmt.Errorf("entitlements: usage: %w", err)
		}
		a := summarize(f, rows)
		u := FeatureUsage{Feature: f, Remaining: a.Remaining, AccessUntil: a.AccessUntil}
		if f.Kind() == KindQuota {
			for _, e := range rows {
				if e.QuantityTotal != nil {
					u.Total += *e.QuantityTotal
				}
				u.Used += e.QuantityUsed
			}
		}
		out = append(out, u)
	}
	return out, nil
}
