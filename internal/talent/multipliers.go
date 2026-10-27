package talent

import (
	"context"
	"errors"
	"fmt"
)

// multNeutralBP is the multiplier of the neutral band (×1.00; 10 000 = ×1).
const multNeutralBP = 10000

// BandMultipliers are the ranking multipliers of the bands of a config
// version (bands[].mult_bp in basis points, 10 000 = ×1.00; BE-099, 00
// T-328): BandMultipliers[i] belongs to band i+1. Discovery multiplies the
// relevance of a result with the multiplier of the student's band.
//
// They are platform configuration, the same for everyone, not a value of a
// user, and they leave talent only through Ranking.BandMultipliers, whose
// version validated them: each in 10001..11500 and growing with the band
// (ADR-0015: only upwards, at most ×1.15). The zero value is ×1.00 for
// every band.
type BandMultipliers []int

// Of returns the multiplier of band b: ×1.00 for the neutral band. A band
// above the version's highest (a score the nightly run computed under an
// earlier version with more bands, until the next run) gets the highest
// band's multiplier, so a higher band never ranks below a lower one.
func (m BandMultipliers) Of(b Band) int {
	if b == BandNeutral || len(m) == 0 {
		return multNeutralBP
	}
	return m[min(int(b), len(m))-1]
}

// BandMultipliers implements Ranking: the bands[].mult_bp of the config
// version in force now. With collection off every band is neutral, so
// nothing is read; a version that does not validate is an error (the
// nightly run refuses it too; talent guesses no product value).
func (s *service) BandMultipliers(ctx context.Context) (BandMultipliers, bool, error) {
	if !s.d.CollectionEnabled {
		return nil, false, nil
	}
	var cfg ConfigVersion
	err := s.inTx(ctx, func(ctx context.Context) error {
		var err error
		cfg, err = s.configs().CurrentConfig(ctx, s.now())
		return err
	})
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	p, err := parseParams(cfg.Params)
	if err != nil {
		return nil, false, fmt.Errorf("talent: band multipliers of config version %s: %w", cfg.ID, err)
	}
	m := make(BandMultipliers, len(p.Bands))
	for i, b := range p.Bands {
		m[i] = b.MultBP
	}
	return m, true, nil
}
