package parties

import (
	"context"
	"fmt"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/files"
	"koro/internal/platform/db"
	"koro/internal/platform/textx"
)

// NewGormStore returns the PostgreSQL store over legal_parties and
// legal_party_documents (02 §17.1–17.2); it reads payees (02 §17.3) for
// the org scope.
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

type gormStore struct{ orm *gorm.DB }

// constraints maps the named constraints of 000094/000095 onto module
// errors (05 §6.6).
var constraints = db.Constraints{
	"legal_parties_taxno_uq":           ErrTaxIDTaken,
	"legal_parties_tckn_uq":            ErrTaxIDTaken,
	"legal_parties_platform_uq":        ErrPlatformExists,
	"legal_party_documents_file_id_uq": files.ErrAlreadyAttached,
}

// payeeOfOrgSQL and orgPartiesSelect relate a party (alias lp) to the org
// @org_id: its own (legal_parties.org_id) or the legal side of one of the
// org's payees that is not closed (03 k21; R-003 §4.7: the seller org is a
// payees row, never the party's org_id). The payee relation wins when both
// hold. The org-scoped WHERE clause is written in each method (05 §5.5).
const (
	payeeOfOrgSQL    = `EXISTS (SELECT 1 FROM payees py WHERE py.legal_party_id = lp.id AND py.org_id = @org_id AND py.status <> 'closed')`
	orgPartiesSelect = `SELECT lp.*, CASE WHEN ` + payeeOfOrgSQL + ` THEN 'payee' ELSE 'org' END AS scope FROM legal_parties lp`
)

// orgPartyRow is a party with its relation to the org of the query.
type orgPartyRow struct {
	LegalParty `gorm:"embedded"`
	Scope      Scope `gorm:"column:scope"`
}

func takeParty(op string, q *gorm.DB) (LegalParty, error) {
	var p LegalParty
	err := q.Take(&p).Error
	switch {
	case db.IsNotFound(err):
		return LegalParty{}, ErrNotFound
	case err != nil:
		return LegalParty{}, fmt.Errorf("parties: %s: %w", op, err)
	}
	return p, nil
}

// InsertParty implements Store.
//
// cross-org: a new row carries its own org_id (the company of a buyer
// party, derived from the path by the service; NULL for the others).
func (s gormStore) InsertParty(ctx context.Context, p LegalParty) (LegalParty, error) {
	if err := db.Conn(ctx, s.orm).Create(&p).Error; err != nil {
		return LegalParty{}, fmt.Errorf("parties: insert party: %w", constraints.Translate(err))
	}
	return s.GetParty(ctx, p.ID)
}

// InsertPlatformIfAbsent implements Store: ON CONFLICT DO NOTHING on
// legal_parties_platform_uq keeps the transaction usable when the row
// exists.
//
// cross-org: the platform party has no org.
func (s gormStore) InsertPlatformIfAbsent(ctx context.Context, p LegalParty) (LegalParty, bool, error) {
	if p.Kind != KindPlatform {
		return LegalParty{}, false, fmt.Errorf("parties: insert platform: kind %q", p.Kind)
	}
	res := db.Conn(ctx, s.orm).Clauses(clause.OnConflict{
		Columns:     []clause.Column{{Name: "kind"}},
		TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Expr{SQL: "kind = 'platform'"}}},
		DoNothing:   true,
	}).Create(&p)
	if res.Error != nil {
		return LegalParty{}, false, fmt.Errorf("parties: insert platform: %w", constraints.Translate(res.Error))
	}
	stored, err := s.GetPlatform(ctx)
	if err != nil {
		return LegalParty{}, false, err
	}
	return stored, res.RowsAffected == 1, nil
}

// GetParty implements Store.
//
// cross-org: addressed by ID (cross-module reads, the admin surface, a
// row the service has resolved within its org first).
func (s gormStore) GetParty(ctx context.Context, id uuid.UUID) (LegalParty, error) {
	return takeParty("get party", db.Conn(ctx, s.orm).Where("id = ?", id))
}

// LockParty implements Store.
//
// cross-org: addressed by ID from the admin surface.
func (s gormStore) LockParty(ctx context.Context, id uuid.UUID) (LegalParty, error) {
	return takeParty("lock party", db.Conn(ctx, s.orm).Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate}).
		Where("id = ?", id))
}

// GetPlatform implements Store.
//
// cross-org: the platform party has no org.
func (s gormStore) GetPlatform(ctx context.Context) (LegalParty, error) {
	return takeParty("get platform", db.Conn(ctx, s.orm).Where("kind = 'platform'"))
}

// UpdateParty implements Store: a targeted UPDATE of the mutable columns
// (kind, org_id, created_by and created_at never change; updated_at is the
// trigger's), RETURNING the stored row.
//
// cross-org: addressed by ID; the service has resolved and locked the
// row within its org (or on the admin surface) in the same transaction.
func (s gormStore) UpdateParty(ctx context.Context, p LegalParty) (LegalParty, error) {
	var rows []LegalParty
	err := db.Conn(ctx, s.orm).Raw(`UPDATE legal_parties SET
		legal_type = @legal_type, title = @title, tax_office = @tax_office, tax_no = @tax_no,
		tckn_enc = @tckn_enc, tckn_bidx = @tckn_bidx, mersis_no = @mersis_no, address = @address,
		email = @email, phone = @phone, vat_status = @vat_status, e_invoice_alias = @e_invoice_alias,
		e_invoice_checked_at = @e_invoice_checked_at, status = @status, verified_at = @verified_at,
		verified_by = @verified_by
		WHERE id = @id
		RETURNING *`, map[string]any{
		"id": p.ID, "legal_type": p.LegalType, "title": p.Title, "tax_office": p.TaxOffice, "tax_no": p.TaxNo,
		"tckn_enc": p.TCKNEnc, "tckn_bidx": p.TCKNBidx, "mersis_no": p.MersisNo, "address": p.Address,
		"email": p.Email, "phone": p.Phone, "vat_status": p.VATStatus, "e_invoice_alias": p.EInvoiceAlias,
		"e_invoice_checked_at": p.EInvoiceCheckedAt, "status": p.Status, "verified_at": p.VerifiedAt,
		"verified_by": p.VerifiedBy,
	}).Scan(&rows).Error
	if err != nil {
		return LegalParty{}, fmt.Errorf("parties: update party: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		return LegalParty{}, ErrNotFound
	}
	return rows[0], nil
}

func orgPartiesOf(rows []orgPartyRow) []OrgParty {
	out := make([]OrgParty, len(rows))
	for i, r := range rows {
		out[i] = OrgParty{Party: r.LegalParty, Scope: r.Scope}
	}
	return out
}

// OrgParties implements Store.
func (s gormStore) OrgParties(ctx context.Context, orgID uuid.UUID) ([]OrgParty, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	q := orgPartiesSelect + ` WHERE lp.kind <> 'platform' AND (lp.org_id = @org_id OR ` + payeeOfOrgSQL + `) ORDER BY lp.id`
	var rows []orgPartyRow
	if err := db.Conn(ctx, s.orm).Raw(q, map[string]any{"org_id": orgID}).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("parties: org parties: %w", err)
	}
	return orgPartiesOf(rows), nil
}

// OrgParty implements Store.
func (s gormStore) OrgParty(ctx context.Context, orgID, id uuid.UUID, lock bool) (OrgParty, error) {
	if orgID == uuid.Nil() {
		return OrgParty{}, db.ErrMissingOrgScope
	}
	q := orgPartiesSelect + ` WHERE lp.id = @id AND lp.kind <> 'platform' AND (lp.org_id = @org_id OR ` + payeeOfOrgSQL + `)`
	if lock {
		q += ` FOR UPDATE OF lp`
	}
	var rows []orgPartyRow
	if err := db.Conn(ctx, s.orm).Raw(q, map[string]any{"org_id": orgID, "id": id}).Scan(&rows).Error; err != nil {
		return OrgParty{}, fmt.Errorf("parties: org party: %w", err)
	}
	if len(rows) == 0 {
		return OrgParty{}, ErrNotFound
	}
	return orgPartiesOf(rows)[0], nil
}

// BuyerParties implements Store.
func (s gormStore) BuyerParties(ctx context.Context, orgID uuid.UUID) ([]LegalParty, error) {
	var out []LegalParty
	err := db.Conn(ctx, s.orm).Scopes(db.ScopeOrg(orgID)).Where("kind = 'buyer_company'").Order("id DESC").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("parties: buyer parties: %w", err)
	}
	return out, nil
}

// ListParties implements Store.
//
// cross-org: the admin list of every party (finance and platform admins).
func (s gormStore) ListParties(ctx context.Context, f ListFilter, after *uuid.UUID, limit int) ([]LegalParty, error) {
	q := db.Conn(ctx, s.orm).Model(&LegalParty{})
	if f.Kind != "" {
		q = q.Where("kind = ?", f.Kind)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	switch {
	case f.Q != "" && isVKNQuery(f.Q):
		q = q.Where("tax_no = ?", f.Q)
	case f.Q != "":
		q = q.Where("public.koro_norm(title) LIKE '%' || public.koro_norm( ?) || '%'", textx.EscapeLike(f.Q))
	}
	if after != nil {
		q = q.Where("id < ?", *after)
	}
	var out []LegalParty
	if err := q.Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("parties: list parties: %w", err)
	}
	return out, nil
}

// PartiesCreatedBy implements Store.
//
// cross-org: the KVKK export of one user.
func (s gormStore) PartiesCreatedBy(ctx context.Context, userID uuid.UUID) ([]LegalParty, error) {
	var out []LegalParty
	if err := db.Conn(ctx, s.orm).Where("created_by = ?", userID).Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("parties: parties created by: %w", err)
	}
	return out, nil
}

// InsertDocument implements Store.
//
// cross-org: documents carry no org_id; the service resolved the party
// within its org first (06 H-38).
func (s gormStore) InsertDocument(ctx context.Context, d Document) (Document, error) {
	if err := db.Conn(ctx, s.orm).Create(&d).Error; err != nil {
		return Document{}, fmt.Errorf("parties: insert document: %w", constraints.Translate(err))
	}
	return s.GetDocument(ctx, d.ID, false)
}

// GetDocument implements Store.
//
// cross-org: addressed by ID; the service compares its party with the
// party resolved within the org (or reads it on the admin surface).
func (s gormStore) GetDocument(ctx context.Context, id uuid.UUID, lock bool) (Document, error) {
	q := db.Conn(ctx, s.orm).Where("id = ?", id)
	if lock {
		q = q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	var d Document
	err := q.Take(&d).Error
	switch {
	case db.IsNotFound(err):
		return Document{}, ErrNotFound
	case err != nil:
		return Document{}, fmt.Errorf("parties: get document: %w", err)
	}
	return d, nil
}

// PartyDocuments implements Store.
//
// cross-org: the documents of parties the caller has already resolved.
func (s gormStore) PartyDocuments(ctx context.Context, partyIDs []uuid.UUID) ([]Document, error) {
	if len(partyIDs) == 0 {
		return nil, nil
	}
	var out []Document
	if err := db.Conn(ctx, s.orm).Where("legal_party_id IN ?", partyIDs).Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("parties: party documents: %w", err)
	}
	return out, nil
}

// SetDocumentVerified implements Store.
//
// cross-org: addressed by ID from the admin surface.
func (s gormStore) SetDocumentVerified(ctx context.Context, id, by uuid.UUID, at time.Time) (Document, error) {
	res := db.Conn(ctx, s.orm).Model(&Document{}).Where("id = ?", id).
		Updates(map[string]any{"verified_at": at, "verified_by": by})
	switch {
	case res.Error != nil:
		return Document{}, fmt.Errorf("parties: verify document: %w", constraints.Translate(res.Error))
	case res.RowsAffected == 0:
		return Document{}, ErrNotFound
	}
	return s.GetDocument(ctx, id, false)
}

// DocumentsUploadedBy implements Store.
//
// cross-org: the KVKK export of one user.
func (s gormStore) DocumentsUploadedBy(ctx context.Context, userID uuid.UUID) ([]Document, error) {
	var out []Document
	if err := db.Conn(ctx, s.orm).Where("uploaded_by = ?", userID).Order("id").Find(&out).Error; err != nil {
		return nil, fmt.Errorf("parties: documents uploaded by: %w", err)
	}
	return out, nil
}
