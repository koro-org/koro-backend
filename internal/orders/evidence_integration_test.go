//go:build integration

package orders_test

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/orders"
)

// pgEvidence inserts a completed delivery_evidence upload of org (app_rw;
// scan skipped, main bucket) and returns its id.
func (p *pgEnv) pgEvidence(t *testing.T, org uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	pgExec(t, p.d, `INSERT INTO files (id, purpose, org_id, uploaded_by, bucket, storage_key, declared_mime, detected_mime,
		size_bytes, sha256, status, scan_status)
		VALUES ($1, 'delivery_evidence', $2, $3, $4, $5, 'image/jpeg', 'image/jpeg', 2048, $6, 'active', 'skipped')`,
		id, org, pgUser(t, p.d), evidenceBuckets.Files, "delivery_evidence/2026/10/"+id.String(), make([]byte, 32))
	return id
}

// paidPGLine places one line of a fresh offering of the seller and pays it.
func (p *pgEnv) paidPGLine(t *testing.T) orders.Line {
	t.Helper()
	off := p.w.offering(t, p.seller)
	vs, err := p.create(p.buyerActor(t), line(off.id, 1))
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	o := vs[0].Order
	if eff, err := p.mod.Orders.ApplyPayment(t.Context(), o.ID, uuid.NewV7(), o.PayableAmount); err != nil || !eff.Paid {
		t.Fatalf("ApplyPayment = %+v, %v", eff, err)
	}
	return vs[0].Lines[0]
}

// evidenceOf reads the line's evidence column and the file's attachment.
func (p *pgEnv) evidenceOf(t *testing.T, lineID, fileID uuid.UUID) (evidence *uuid.UUID, module *string, entity *uuid.UUID) {
	t.Helper()
	if err := p.d.Pool.QueryRow(t.Context(), `SELECT fulfillment_evidence_file_id FROM order_lines WHERE id = $1`, lineID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if err := p.d.Pool.QueryRow(t.Context(), `SELECT attached_module, attached_entity_id FROM files WHERE id = $1`, fileID).Scan(&module, &entity); err != nil {
		t.Fatal(err)
	}
	return evidence, module, entity
}

// TestMarkWithEvidence_Postgres (BE-093, T-292): on PostgreSQL the mark
// writes the evidence column (000137: FK, order_lines_evidence_chk) and
// attaches the file to the line in the same transaction; the confirmation
// keeps it (the targeted UPDATE never clears it, order_lines_evidence_frozen
// would refuse); the buyer downloads it; another org's upload is 422 and
// changes nothing.
func TestMarkWithEvidence_Postgres(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t)
	l := p.paidPGLine(t)
	sa, sctx := p.sellerActor(t)
	foreign := p.pgEvidence(t, p.buyer.org)
	_, err := p.mod.Service.MarkLineFulfilled(sctx, sa, l.ID, orders.MarkInput{EvidenceFileID: &foreign})
	wantProblem(t, err, 422, "validation_failed")
	if ev, mod, _ := p.evidenceOf(t, l.ID, foreign); ev != nil || mod != nil {
		t.Fatalf("after the refusal: evidence %v, attached to %v", ev, mod)
	}
	file := p.pgEvidence(t, p.seller.org)
	v, err := p.mod.Service.MarkLineFulfilled(sctx, sa, l.ID, orders.MarkInput{Note: "Stant kuruldu", EvidenceFileID: &file})
	if err != nil || v.Line.FulfillmentEvidenceFileID == nil || *v.Line.FulfillmentEvidenceFileID != file {
		t.Fatalf("MarkLineFulfilled = %+v, %v", v.Line, err)
	}
	if ev, mod, ent := p.evidenceOf(t, l.ID, file); ev == nil || *ev != file || mod == nil || *mod != "orders" || ent == nil || *ent != l.ID {
		t.Fatalf("evidence %v, attached to %v/%v", ev, mod, ent)
	}
	ctx := p.buyerActor(t)
	ba, err := authz.ActorFrom(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := p.mod.Service.ConfirmLine(ctx, ba, l.ID); err != nil || c.Line.FulfillmentEvidenceFileID == nil || *c.Line.FulfillmentEvidenceFileID != file {
		t.Fatalf("ConfirmLine = %+v, %v", c.Line, err)
	}
	dl, err := p.mod.Service.LineEvidence(ctx, ba, l.ID)
	if err != nil || dl.URL == "" {
		t.Fatalf("LineEvidence = %+v, %v", dl, err)
	}
}

// failingMarkAudit fails the audit of a mark: the write after the
// evidence's Attach and the line's UPDATE in the marking transaction.
type failingMarkAudit struct{ auditLog }

var errAuditDown = errors.New("audit store down")

func (f *failingMarkAudit) Log(ctx context.Context, e audit.Event) error {
	if e.Action == string(authz.OrdersLineMarkFulfilled) {
		return errAuditDown
	}
	return f.auditLog.Log(ctx, e)
}

// TestMarkWithEvidence_Postgres_OneTransaction: when the mark fails after
// the attachment (its audit write), the rollback takes the attachment and
// the line's evidence with it: the file is an unattached upload again and
// the line is still pending (files.Attach joins the marking transaction).
func TestMarkWithEvidence_Postgres_OneTransaction(t *testing.T) {
	t.Parallel()
	p := newPGEnv(t, func(d *orders.Deps) { d.Audit = &failingMarkAudit{} })
	l := p.paidPGLine(t)
	sa, sctx := p.sellerActor(t)
	file := p.pgEvidence(t, p.seller.org)
	_, err := p.mod.Service.MarkLineFulfilled(sctx, sa, l.ID, orders.MarkInput{EvidenceFileID: &file})
	if !errors.Is(err, errAuditDown) {
		t.Fatalf("MarkLineFulfilled = %v, want the audit failure", err)
	}
	if ev, mod, ent := p.evidenceOf(t, l.ID, file); ev != nil || mod != nil || ent != nil {
		t.Fatalf("after the rollback: evidence %v, attached to %v/%v", ev, mod, ent)
	}
	var status string
	if err := p.d.Pool.QueryRow(t.Context(), `SELECT fulfillment_status FROM order_lines WHERE id = $1`, l.ID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("line status = %q, %v", status, err)
	}
}
