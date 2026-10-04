package parties

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
)

// retainedReason explains why Erase keeps what it finds (02 §0.6, §17.1:
// "mali kayıtlara bağlıyken silinmez").
const retainedReason = "legal parties and their documents are financial records kept for 10 years " +
	"(financial_records; tax and commercial law): they are neither deleted nor anonymised on account " +
	"erasure; created_by/uploaded_by stay as references to the anonymised account and the rows are " +
	"destroyed when the retention period ends"

// privacyProvider is the KVKK provider of parties (01 §3.3). A legal party
// is personal data when it is a natural person (individual, sole
// proprietorship; 02 §17.1) and a document may be. They are financial
// records (10 years), so Erase retains everything (a restriction, recorded
// in the DSR answer as a legal obligation). Export returns what the user
// entered: the parties they created and the documents they uploaded. No
// export item ever carries a national ID or the document file itself
// (the user has the file; the export lists it).
//
// Limit (BE-036): the parties of a natural person are linked to the
// account that created them, not to the person (who may not be a Koro
// user); a natural person's own access request is answered by finance
// from the party (T-31 follow-up, 06 H-54 keeps them closed in the MVP).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "parties" }

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	ps, err := p.s.d.Store.PartiesCreatedBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	ds, err := p.s.d.Store.DocumentsUploadedBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "parties", Items: make([]map[string]any, 0, len(ps)+len(ds))}
	for _, lp := range ps {
		item := map[string]any{
			"record": "legal_party_created", "party_id": lp.ID.String(), "kind": string(lp.Kind),
			"legal_type": string(lp.LegalType), "title": lp.Title, "status": string(lp.Status),
			"created_at": lp.CreatedAt.UTC().Format(time.RFC3339),
		}
		if lp.OrgID != nil {
			item["org_id"] = lp.OrgID.String()
		}
		out.Items = append(out.Items, item)
	}
	for _, d := range ds {
		item := map[string]any{
			"record": "legal_party_document_uploaded", "document_id": d.ID.String(), "party_id": d.LegalPartyID.String(),
			"document_kind": string(d.Kind), "file_id": d.FileID.String(), "uploaded_at": d.CreatedAt.UTC().Format(time.RFC3339),
		}
		if d.VerifiedAt != nil {
			item["verified_at"] = d.VerifiedAt.UTC().Format(time.RFC3339)
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// Erase implements privacy.Provider: nothing is deleted or anonymised (see
// retainedReason). It is idempotent.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	ps, err := p.s.d.Store.PartiesCreatedBy(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	ds, err := p.s.d.Store.DocumentsUploadedBy(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	res := privacy.EraseResult{Module: "parties", Retained: len(ps) + len(ds)}
	if res.Retained > 0 {
		res.RetainedReason = retainedReason
	}
	return res, nil
}
