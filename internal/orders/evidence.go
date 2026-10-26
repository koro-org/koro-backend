package orders

import (
	"context"
	"errors"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/problem"
)

// Delivery evidence (BE-093, T-292; user decision 2026-10-23): the seller
// may attach a photo or a PDF when it marks a line delivered. The file is a
// delivery_evidence upload of the line's seller org (createUpload with the
// seller's org_id, 03 k30); orders attaches it to the line in the marking
// transaction and it never changes (order_lines_evidence_frozen). The
// buyer's admins confirm or dispute with it in view, the platform and
// finance admins resolve a dispute with it, the seller's markers see what
// they attached. It is part of the line's financial record (10 years).

// evidenceTTL is the lifetime of an evidence download URL (04 §1.14: ≤ 5
// minutes).
const evidenceTTL = 5 * time.Minute

// evidenceModule is the files owner module of an evidence file.
const evidenceModule = "orders"

// evidenceOwner is the files owner of the evidence of a line.
func evidenceOwner(lineID uuid.UUID) files.Owner {
	return files.Owner{Module: evidenceModule, EntityID: lineID, Purpose: files.PurposeDeliveryEvidence}
}

// checkEvidence checks the evidence file of a mark before the transaction:
// a completed delivery_evidence upload of the line's seller org (06 H-38:
// the org comes from the line, never from the request). An unknown file,
// another purpose and another org's file get the same 422 (no probing of
// other orgs' files); a file rejected after its upload or found infected
// is 409. Attach repeats the purpose, status and attachment checks under
// the file's lock.
func (s *service) checkEvidence(ctx context.Context, l Line, fileID uuid.UUID) error {
	const loc = "body.evidence_file_id"
	if s.d.Files == nil {
		return ErrUnavailable
	}
	f, err := s.d.Files.Get(ctx, fileID)
	if err == nil {
		err = f.CheckOrgUpload(l.SellerOrgID, evidenceOwner(l.ID))
	}
	switch {
	case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrNotOrgUpload):
		return problem.Field(loc, "must be a delivery_evidence upload of the selling organization")
	case errors.Is(err, files.ErrUploadNotActive):
		return problem.Field(loc, "must be a completed upload")
	}
	return err
}

// evidenceAttachErr is the answer to a failed Attach of the evidence: a
// file gone since checkEvidence (the orphan cleanup purged it) is the 422 of
// an unknown file; the files errors (409) and the rest pass through.
func evidenceAttachErr(err error) error {
	if errors.Is(err, files.ErrNotFound) {
		return problem.Field("body.evidence_file_id", "must be a delivery_evidence upload of the selling organization")
	}
	return err
}

// LineEvidence implements Service (getOrderLineEvidence). Order: the line
// (404) → authz on it (404 for whoever does not see the line, 403 without
// the cell) → the evidence (404 when the line has none) → a 5-minute URL of
// a clean file (409 files.not_clean while the scan has not cleared it) →
// audit (every download, before the URL is handed out).
func (s *service) LineEvidence(ctx context.Context, a authz.Actor, lineID uuid.UUID) (files.Download, error) {
	l, err := s.d.Store.GetLine(ctx, lineID, false)
	if err != nil {
		return files.Download{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersLineReadEvidence, lineResource(l)); err != nil {
		return files.Download{}, err
	}
	if l.FulfillmentEvidenceFileID == nil {
		return files.Download{}, ErrNotFound
	}
	if s.d.Files == nil {
		return files.Download{}, ErrUnavailable
	}
	file := *l.FulfillmentEvidenceFileID
	dl, err := s.d.Files.PresignGet(ctx, file, evidenceTTL)
	if err != nil {
		return files.Download{}, err
	}
	meta := map[string]any{"operation": "download", "evidence_file_id": file.String(), "ttl_seconds": int(evidenceTTL.Seconds())}
	if err := s.d.Audit.Log(ctx, lineEvent(a, authz.OrdersLineReadEvidence, l, "", meta)); err != nil {
		return files.Download{}, err
	}
	return dl, nil
}
