package billing

import (
	"context"
	"fmt"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/platform/db"
)

// NewGormStore returns the PostgreSQL store of billing over invoices,
// invoice_lines and integration_credentials (02 §22).
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

var _ Store = gormStore{}

// InsertInvoice implements Store: ON CONFLICT (idempotency_key) DO NOTHING
// makes a repeated event a no-op, and a concurrent duplicate waits for the
// first transaction instead of failing the caller's.
//
// cross-org: a planned document between two parties; its scope columns
// come from the order or the payout (06 H-38), never from a request.
func (s gormStore) InsertInvoice(ctx context.Context, inv Invoice, lines []Line) (Invoice, bool, error) {
	conn := db.Conn(ctx, s.orm)
	res := conn.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "idempotency_key"}}, DoNothing: true}).Create(&inv)
	if res.Error != nil {
		return Invoice{}, false, fmt.Errorf("billing: insert invoice: %w", translate(res.Error))
	}
	if res.RowsAffected == 0 {
		var cur Invoice
		if err := conn.Where("idempotency_key = ?", inv.IdempotencyKey).Take(&cur).Error; err != nil {
			return Invoice{}, false, fmt.Errorf("billing: invoice by key: %w", err)
		}
		return cur, false, nil
	}
	if len(lines) > 0 {
		if err := conn.Create(&lines).Error; err != nil {
			return Invoice{}, false, fmt.Errorf("billing: insert invoice lines: %w", translate(err))
		}
	}
	stored, err := s.GetInvoice(ctx, inv.ID, false)
	return stored, true, err
}

// GetInvoice implements Store.
//
// cross-org: addressed by ID; the service authorises the loaded invoice.
func (s gormStore) GetInvoice(ctx context.Context, id uuid.UUID, lock bool) (Invoice, error) {
	q := db.Conn(ctx, s.orm)
	if lock {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var inv Invoice
	err := q.Where("id = ?", id).Take(&inv).Error
	switch {
	case db.IsNotFound(err):
		return Invoice{}, ErrNotFound
	case err != nil:
		return Invoice{}, fmt.Errorf("billing: get invoice: %w", err)
	}
	return inv, nil
}

// InvoiceLines implements Store.
//
// cross-org: the lines of an invoice the caller resolved.
func (s gormStore) InvoiceLines(ctx context.Context, invoiceID uuid.UUID) ([]Line, error) {
	var out []Line
	if err := db.Conn(ctx, s.orm).Where("invoice_id = ?", invoiceID).Order("position").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("billing: invoice lines: %w", err)
	}
	return out, nil
}

// UpdateInvoice implements Store with a targeted UPDATE of the running
// columns (06 H-56; invoices_frozen keeps an issued document as it is).
//
// cross-org: addressed by ID after the service's authorisation.
func (s gormStore) UpdateInvoice(ctx context.Context, next Invoice, from Status) (Invoice, error) {
	var rows []Invoice
	err := db.Conn(ctx, s.orm).Raw(`UPDATE invoices SET
		status = @status, document_type = @document_type, invoice_no = @invoice_no, ettn = @ettn, issue_date = @issue_date,
		pdf_file_id = @pdf_file_id, pdf_sha256 = @pdf_sha256, uploaded_by = @uploaded_by, verified_by = @verified_by,
		verified_at = @verified_at, issued_at = @issued_at, last_error = @last_error
		WHERE id = @id AND status = @from
		RETURNING *`, map[string]any{
		"id": next.ID, "from": from, "status": next.Status, "document_type": next.DocumentType, "invoice_no": next.InvoiceNo,
		"ettn": next.ETTN, "issue_date": next.IssueDate, "pdf_file_id": next.PDFFileID, "pdf_sha256": next.PDFSHA256,
		"uploaded_by": next.UploadedBy, "verified_by": next.VerifiedBy, "verified_at": next.VerifiedAt,
		"issued_at": next.IssuedAt, "last_error": next.LastError,
	}).Scan(&rows).Error
	if err != nil {
		return Invoice{}, fmt.Errorf("billing: update invoice: %w", translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetInvoice(ctx, next.ID, false); err != nil {
			return Invoice{}, err
		}
		return Invoice{}, ErrInvalidState
	}
	return rows[0], nil
}

// ListInvoices implements Store.
//
// cross-org: f.OrgID scopes the org surface (the issuer or the recipient
// org is the org of the path); the admin list spans every org
// (billing.invoice.list_admin).
func (s gormStore) ListInvoices(ctx context.Context, f Filter, after *Cursor, limit int) ([]Invoice, error) {
	q := db.Conn(ctx, s.orm).Model(&Invoice{})
	if f.OrgID != nil {
		switch f.Direction {
		case DirectionIssued:
			q = q.Where("issuer_org_id = ?", *f.OrgID)
		case DirectionReceived:
			q = q.Where("recipient_org_id = ?", *f.OrgID)
		default:
			q = q.Where("( issuer_org_id = ? OR recipient_org_id = ? )", *f.OrgID, *f.OrgID)
		}
	}
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.OrderID != nil {
		q = q.Where("order_id = ?", *f.OrderID)
	}
	if f.PayoutID != nil {
		q = q.Where("payout_id = ?", *f.PayoutID)
	}
	if after != nil {
		q = q.Where("(created_at, id) < ( ?, ?)", after.CreatedAt, after.ID)
	}
	var out []Invoice
	if err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("billing: list invoices: %w", err)
	}
	return out, nil
}

// InvoicesOfOrderLine implements Store.
//
// cross-org: the invoices of one order line (the refund of the line).
func (s gormStore) InvoicesOfOrderLine(ctx context.Context, orderLineID uuid.UUID) ([]Invoice, error) {
	var out []Invoice
	err := db.Conn(ctx, s.orm).Where(`EXISTS (SELECT 1 FROM invoice_lines l WHERE l.invoice_id = invoices.id AND l.order_line_id = ?)`,
		orderLineID).Order("created_at, id").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("billing: invoices of order line: %w", err)
	}
	return out, nil
}

// InvoicesBy implements Store.
//
// cross-org: a data subject's own records (KVKK export).
func (s gormStore) InvoicesBy(ctx context.Context, userID uuid.UUID) ([]Invoice, error) {
	var out []Invoice
	if err := db.Conn(ctx, s.orm).Where("uploaded_by = ? OR verified_by = ?", userID, userID).
		Order("created_at, id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("billing: invoices by a user: %w", err)
	}
	return out, nil
}

// Credentials implements Store.
//
// cross-org: platform configuration.
func (s gormStore) Credentials(ctx context.Context) ([]Credential, error) {
	var out []Credential
	if err := db.Conn(ctx, s.orm).Order("provider").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("billing: integration credentials: %w", err)
	}
	return out, nil
}

// PutCredential implements Store (upsert on the provider).
//
// cross-org: platform configuration.
func (s gormStore) PutCredential(ctx context.Context, c Credential) (Credential, error) {
	var rows []Credential
	err := db.Conn(ctx, s.orm).Raw(`INSERT INTO integration_credentials
		(provider, external_account, access_token_enc, refresh_token_enc, expires_at, rotated_at)
		VALUES ( @provider, @external_account, @access_token_enc, @refresh_token_enc, @expires_at, @rotated_at)
		ON CONFLICT (provider) DO UPDATE SET external_account = EXCLUDED.external_account,
			access_token_enc = EXCLUDED.access_token_enc, refresh_token_enc = EXCLUDED.refresh_token_enc,
			expires_at = EXCLUDED.expires_at, rotated_at = EXCLUDED.rotated_at
		RETURNING *`, map[string]any{
		"provider": c.Provider, "external_account": c.ExternalAccount, "access_token_enc": c.AccessTokenEnc,
		"refresh_token_enc": c.RefreshTokenEnc, "expires_at": c.ExpiresAt, "rotated_at": c.RotatedAt,
	}).Scan(&rows).Error
	if err != nil {
		return Credential{}, fmt.Errorf("billing: put integration credential: %w", translate(err))
	}
	if len(rows) != 1 {
		return Credential{}, fmt.Errorf("billing: put integration credential: %d rows", len(rows))
	}
	return rows[0], nil
}
