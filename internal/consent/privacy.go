package consent

import (
	"context"
	"encoding/hex"
	"time"

	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/privacy"
)

// ErasureJob is the audit actor of the withdrawals written on erasure.
const ErasureJob = "kvkk.erasure"

// retainedReason is the legal basis of the rows Erase keeps (02 §4.3:
// consent_proof, 6563 burden of proof; swept by BE-057; terms_acceptance,
// the proof of the service contract, BE-118).
const retainedReason = "consent proof (6563, KVKK): kept until 3 years after the last withdrawal or the account erasure (consent_proof); " +
	"terms of use acceptances (contract proof): kept until 3 years after the account erasure (terms_acceptance)"

// privacyProvider is the KVKK provider of consent (01 §3.3). The ledger is
// proof the platform must keep: it is exported in full and never deleted
// on erasure (02 §4.3; the retention sweeper is BE-057).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "consent" }

// Export implements privacy.Provider: every consent row of the user, oldest
// first, with the proof the platform keeps about them (IP, channel, text
// version and digest), then every terms of use acceptance (BE-118).
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	rows, err := p.s.d.Store.AllOf(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	terms, err := p.s.d.Store.TermsAcceptancesOf(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "consent", Items: make([]map[string]any, 0, len(rows)+len(terms))}
	for _, c := range rows {
		item := map[string]any{
			"consent_id": c.ID.String(), "purpose_code": string(c.PurposeCode), "action": string(c.Action),
			"channel": string(c.Channel), "occurred_at": c.OccurredAt.Format(time.RFC3339),
		}
		if c.NoticeVersionID != nil {
			item["notice_version_id"] = c.NoticeVersionID.String()
			item["text_sha256"] = hex.EncodeToString(c.TextSHA256)
		}
		if c.ScopeID != nil {
			item["scope_id"] = c.ScopeID.String()
		}
		if c.IP != "" {
			item["ip"] = c.IP
		}
		if c.UserAgent != "" {
			item["user_agent"] = c.UserAgent
		}
		out.Items = append(out.Items, item)
	}
	for _, t := range terms {
		item := map[string]any{
			"terms_acceptance_id": t.ID.String(), "notice_version_id": t.NoticeVersionID.String(),
			"text_sha256": hex.EncodeToString(t.TextSHA256), "channel": string(t.Channel),
			"accepted_at": t.AcceptedAt.Format(time.RFC3339),
		}
		if t.RequestID != "" {
			item["request_id"] = t.RequestID
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Erase implements privacy.Provider. Nothing is deleted (proof, 02 §4.3);
// every consent still granted is withdrawn by appending a row (channel
// admin), so that İYS learns of the withdrawal of commercial consents and
// the consent_proof period starts. The terms of use acceptances stay too:
// the account's erasure starts their terms_acceptance period (BE-118). It
// is idempotent: a second run finds nothing granted.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	cur, err := p.s.d.Store.CurrentOf(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	by := authz.SystemActor(ErasureJob)
	err = p.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		for _, c := range cur {
			if c.Action != Granted {
				continue
			}
			_, err := p.s.record(ctx, RecordInput{
				UserID: userID, Purpose: c.PurposeCode, Action: Withdrawn, ScopeID: c.ScopeID, Channel: ChannelAdmin,
			}, by)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return privacy.EraseResult{}, err
	}
	all, err := p.s.d.Store.AllOf(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	terms, err := p.s.d.Store.TermsAcceptancesOf(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	res := privacy.EraseResult{Module: "consent", Retained: len(all) + len(terms)}
	if res.Retained > 0 {
		res.RetainedReason = retainedReason
	}
	return res, nil
}
