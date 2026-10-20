package talent

import (
	"context"
	"uuid"

	"koro/internal/platform/privacy"
)

// providerName is the KVKK provider name (the module).
const providerName = "talent"

// privacyProvider is the KVKK provider of talent (01 §3.3): the ledger,
// the daily counters, the neutralisations, the cohort and the score.
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

func (privacyProvider) Name() string { return providerName }

// Export implements privacy.Provider for access and export requests. It
// states whether profiling data is held and nothing more: the content (the
// signals, the band) is disclosed through a profiling_access request,
// which a second approver reviews before it reaches the subject (03 §5.12;
// a plain access request has no such review).
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	var has bool
	err := p.s.inTx(ctx, func(ctx context.Context) error {
		var err error
		has, err = p.s.d.Store.HasData(ctx, userID)
		return err
	})
	if err != nil {
		return privacy.ExportSection{}, err
	}
	return privacy.ExportSection{Module: providerName, Items: []map[string]any{{
		"profiling_data_held": has,
		"disclosure":          "profiling_access",
		"note":                "Talent (aktivite temelli sıralama) verisinin içeriği yalnız profilleme erişim başvurusuyla (profiling_access) verilir.",
	}}}, nil
}

// Erase implements privacy.Provider: every talent row of the user goes
// (02 §15: hard delete on account erasure), the append-only ledger through
// talent.point_events_erase_user, which requires the completed erasure
// request kvkk runs this for. Idempotent: a second run deletes nothing.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	var n int
	err := p.s.inTx(ctx, func(ctx context.Context) error {
		var err error
		n, err = p.s.d.Store.EraseUser(ctx, userID)
		return err
	})
	if err != nil {
		return privacy.EraseResult{}, err
	}
	return privacy.EraseResult{Module: providerName, Deleted: n}, nil
}
