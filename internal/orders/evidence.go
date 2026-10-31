package orders

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// Delivery evidence (BE-093, T-292; user decision 2026-10-23): the seller
// may attach a photo or a PDF when it marks a line delivered. The file is a
// delivery_evidence upload of the line's seller org (createUpload with the
// seller's org_id, 03 k30); orders attaches it to the line in the marking
// transaction and it never changes (order_lines_evidence_frozen) unless the
// scan rejects it after the upload (BE-154, T-393): the seller's markers
// then attach a new file in its place (ReplaceLineEvidence). The
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

// evidenceStates returns the states of the delivery evidence files of
// lines (nil without one, or without the files module).
func (s *service) evidenceStates(ctx context.Context, lines ...Line) (map[uuid.UUID]files.State, error) {
	ids := make([]*uuid.UUID, len(lines))
	for i := range lines {
		ids[i] = lines[i].FulfillmentEvidenceFileID
	}
	set := files.IDs(ids...)
	if len(set) == 0 || s.d.Files == nil {
		return nil, nil
	}
	return s.d.Files.States(ctx, set)
}

// ReplaceLineEvidence implements Service (replaceOrderLineEvidence; BE-154,
// T-393): the people who mark the line delivered (orders.line.mark_fulfilled
// on it: 404/403) → the line's evidence (409 orders.invalid_state when it
// has none) → the new file a delivery_evidence upload of the line's seller
// org (422; 409 when it was rejected too) → in one transaction (the order,
// then the line locked): the line still holds that evidence, rejected after
// its upload (409 files.not_rejected otherwise), the new file attached
// (files.Attach) and written in its place → audit (both files; the old row
// stays attached to the line). The delivery itself does not change.
func (s *service) ReplaceLineEvidence(ctx context.Context, a authz.Actor, lineID, fileID uuid.UUID) (LineView, error) {
	l, err := s.d.Store.GetLine(ctx, lineID, false)
	if err != nil {
		return LineView{}, err
	}
	if err := authz.Can(ctx, a, authz.OrdersLineMarkFulfilled, lineResource(l)); err != nil {
		return LineView{}, err
	}
	if l.FulfillmentEvidenceFileID == nil {
		return LineView{}, ErrInvalidState
	}
	if err := s.checkEvidence(ctx, l, fileID); err != nil {
		return LineView{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op       string
		Line     uuid.UUID
		Evidence uuid.UUID
	}{"replace_order_line_evidence", lineID, fileID})
	if err != nil {
		return LineView{}, err
	}
	seller := !fullReader(a, l.OrgID)
	return s.lineChange(ctx, hash, lineID, seller, func(ctx context.Context, _ Order, cur Line, _ time.Time) (Line, error) {
		if cur.FulfillmentEvidenceFileID == nil {
			return Line{}, ErrInvalidState
		}
		old := *cur.FulfillmentEvidenceFileID
		meta, err := files.Replace(ctx, s.d.Files, a, old, fileID, evidenceOwner(cur.ID))
		if err != nil {
			return Line{}, evidenceAttachErr(err)
		}
		stored, err := s.d.Store.ReplaceLineEvidence(ctx, cur.ID, old, fileID)
		if err != nil {
			return Line{}, err
		}
		return stored, s.d.Audit.Log(ctx, lineEvent(a, authz.OrdersLineMarkFulfilled, stored, "", meta))
	})
}

// notifyRejectedEvidence is the file.rejected subscription of the delivery
// evidence (BE-154, T-393): the line keeps the reference (a financial
// record, BE-151) and the seller's markers (club admins and university
// representatives of the selling org: they replace it,
// replaceOrderLineEvidence) are told in-app; while the line is disputed the
// finance admins who resolve the dispute with the evidence in view are told
// too. A line that holds another evidence by now (replaced) or is gone is a
// no-op; the dedupe keys (the file and the recipient) make a repeated
// delivery one notice.
func (s *service) notifyRejectedEvidence(ctx context.Context, r files.Rejection) error {
	l, err := s.d.Store.GetLine(ctx, r.EntityID, false)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case l.FulfillmentEvidenceFileID == nil || *l.FulfillmentEvidenceFileID != r.FileID:
		return nil
	}
	o, _, ok, err := s.noticeOrder(ctx, l.OrderID)
	if err != nil || !ok {
		return err
	}
	key := r.FileID.String()
	admins, err := s.membersWith(ctx, l.SellerOrgID, authz.MemberClubAdmin, authz.MemberUniversityRep)
	if err != nil {
		return err
	}
	errs := []error{s.notifyInApp(ctx, o, l.SellerOrgID, tmplEvidenceRejected, key, admins, notify.OrgSalesLink(l.SellerOrgID))}
	if l.FulfillmentStatus == FulfillDisputed {
		if s.d.Staff == nil {
			return errors.New("orders: the finance admin lookup is not wired")
		}
		fins, err := s.d.Staff.FinanceAdmins(ctx)
		if err != nil {
			return fmt.Errorf("orders: finance admins: %w", err)
		}
		errs = append(errs, s.notifyInApp(ctx, o, l.SellerOrgID, tmplEvidenceRejected, key, fins, notify.AdminOrderLink(o.OrderNo)))
	}
	return errors.Join(errs...)
}
