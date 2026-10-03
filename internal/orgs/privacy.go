package orgs

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
)

// retainedReason explains why Erase keeps the rows it finds (BE-017).
const retainedReason = "organizations and companies hold no personal data (02 §7.1-7.2: created_by/verified_by " +
	"are references to the account, which identity anonymises); membership rows are ended with account_erased " +
	"by the membership lifecycle (BE-019) and purged by its retention sweeper"

// privacyProvider is the KVKK provider of orgs (01 §3.3). The org and
// company rows are not personal data (02 §7.1-7.2; the company's tax
// number and MERSİS live in parties, the authorised person's details are
// the user's own account in identity). What orgs knows about a person is
// their memberships (02 §7.3) and the orgs they registered: Export returns
// both to the data subject; Erase deletes nothing in this ticket (see
// retainedReason).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "orgs" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	ms, err := p.s.d.Store.MembershipsOfUser(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	created, err := p.s.d.Store.OrgsCreatedBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "orgs", Items: make([]map[string]any, 0, len(ms)+len(created))}
	for _, m := range ms {
		item := map[string]any{
			"record": "membership", "membership_id": m.ID.String(), "org_id": m.OrgID.String(),
			"org_kind": string(m.OrgKind), "role": string(m.Role), "status": string(m.Status), "source": string(m.Source),
		}
		putTime(item, "requested_at", m.RequestedAt)
		putTime(item, "joined_at", m.JoinedAt)
		putTime(item, "ended_at", m.EndedAt)
		if m.EndReason != nil {
			item["end_reason"] = string(*m.EndReason)
		}
		out.Items = append(out.Items, item)
	}
	for _, o := range created {
		out.Items = append(out.Items, map[string]any{
			"record": "registered_org", "org_id": o.ID.String(), "org_kind": string(o.Kind), "name": o.Name,
			"created_at": o.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func putTime(item map[string]any, key string, t *time.Time) {
	if t != nil {
		item[key] = t.UTC().Format(time.RFC3339)
	}
}

// Erase implements privacy.Provider: nothing is deleted here (see
// retainedReason). It is idempotent.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	ms, err := p.s.d.Store.MembershipsOfUser(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	res := privacy.EraseResult{Module: "orgs", Retained: len(ms)}
	if res.Retained > 0 {
		res.RetainedReason = retainedReason
	}
	return res, nil
}
