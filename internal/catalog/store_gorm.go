package catalog

import (
	"context"
	"fmt"
	"strings"
	"time"
	"uuid"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"koro/internal/authz"
	"koro/internal/platform/db"
)

// constraints maps the named constraints of 000098–000100 onto module
// errors (05 §6.6). The four-eyes CHECK answers like authz (03 §6.1: the
// database is the second layer of the same rule).
var constraints = db.Constraints{
	"services_code_uq":                       ErrServiceCodeTaken,
	"offerings_approver_chk":                 authz.ErrFourEyes,
	"offerings_default_payee_id_org_id_fkey": ErrParentMismatch,
	"offerings_event_id_org_id_fkey":         ErrParentMismatch,
	"commerce_policies_no_overlap_uq":        ErrPolicyOverlap,
}

type gormStore struct{ orm *gorm.DB }

// NewGormStore returns the PostgreSQL Store over the app_rw pool.
func NewGormStore(orm *gorm.DB) Store { return gormStore{orm: orm} }

func lockIf(q *gorm.DB, lock bool) *gorm.DB {
	if lock {
		return q.Clauses(clause.Locking{Strength: clause.LockingStrengthUpdate})
	}
	return q
}

func take[T any](op string, q *gorm.DB) (T, error) {
	var v T
	err := q.Take(&v).Error
	switch {
	case db.IsNotFound(err):
		return v, ErrNotFound
	case err != nil:
		return v, fmt.Errorf("catalog: %s: %w", op, err)
	}
	return v, nil
}

// ---- services

// InsertService implements Store.
//
// cross-org: the Koro catalogue has no org.
func (s gormStore) InsertService(ctx context.Context, v ServiceItem) (ServiceItem, error) {
	if err := db.Conn(ctx, s.orm).Create(&v).Error; err != nil {
		return ServiceItem{}, fmt.Errorf("catalog: insert service: %w", constraints.Translate(err))
	}
	return s.GetService(ctx, v.ID, false)
}

// GetService implements Store.
//
// cross-org: the Koro catalogue has no org.
func (s gormStore) GetService(ctx context.Context, id uuid.UUID, lock bool) (ServiceItem, error) {
	return take[ServiceItem]("get service", lockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// UpdateService implements Store: a targeted UPDATE of the mutable columns.
//
// cross-org: the Koro catalogue has no org.
func (s gormStore) UpdateService(ctx context.Context, v ServiceItem) (ServiceItem, error) {
	var rows []ServiceItem
	err := db.Conn(ctx, s.orm).Raw(`UPDATE services SET
		name_tr = @name_tr, description_tr = @description_tr, category = @category,
		entitlement_template = @entitlement_template, default_vat_rate_bp = @default_vat_rate_bp, vat_class = @vat_class,
		vat_exemption_code = @vat_exemption_code, vat_withholding_code = @vat_withholding_code,
		vat_withholding_num = @vat_withholding_num, vat_withholding_den = @vat_withholding_den,
		invoice_description_tr = @invoice_description_tr, accounting_reviewed_at = @accounting_reviewed_at,
		accounting_reviewed_ref = @accounting_reviewed_ref, active = @active
		WHERE id = @id
		RETURNING *`, map[string]any{
		"id": v.ID, "name_tr": v.NameTR, "description_tr": v.DescriptionTR, "category": v.Category,
		"entitlement_template": v.EntitlementTemplate, "default_vat_rate_bp": v.DefaultVATRateBP, "vat_class": v.VATClass,
		"vat_exemption_code": v.VATExemptionCode, "vat_withholding_code": v.VATWithholdingCode,
		"vat_withholding_num": v.VATWithholdingNum, "vat_withholding_den": v.VATWithholdingDen,
		"invoice_description_tr": v.InvoiceDescriptionTR, "accounting_reviewed_at": v.AccountingReviewedAt,
		"accounting_reviewed_ref": v.AccountingReviewedRef, "active": v.Active,
	}).Scan(&rows).Error
	if err != nil {
		return ServiceItem{}, fmt.Errorf("catalog: update service: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		return ServiceItem{}, ErrNotFound
	}
	return rows[0], nil
}

// ListServices implements Store.
//
// cross-org: the Koro catalogue has no org.
func (s gormStore) ListServices(ctx context.Context, f ServiceFilter) ([]ServiceItem, error) {
	q := db.Conn(ctx, s.orm).Order("name_tr, id")
	if f.Category != "" {
		q = q.Where("category = ?", f.Category)
	}
	if f.SellableOnly {
		q = q.Where("active AND accounting_reviewed_at IS NOT NULL")
	}
	out := []ServiceItem{}
	if err := q.Find(&out).Error; err != nil {
		return nil, fmt.Errorf("catalog: list services: %w", err)
	}
	return out, nil
}

// ServicesByID implements Store.
//
// cross-org: the Koro catalogue has no org.
func (s gormStore) ServicesByID(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ServiceItem, error) {
	out := map[uuid.UUID]ServiceItem{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []ServiceItem
	if err := db.Conn(ctx, s.orm).Where("id = ANY( ?)", db.UUIDArray(ids)).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("catalog: services by id: %w", err)
	}
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

// ---- offerings

// offeringSelect are the columns of an Offering (search_vector is
// generated and never read).
const offeringSelect = `id, service_id, seller_kind, org_id, event_id, default_payee_id, title, description_json, deliverables,
	unit_price_amount, currency, vat_rate_bp, compensation_type, quantity_total, quantity_sold, per_buyer_limit,
	sales_start_at, sales_end_at, fulfil_by, invoice_trigger, requires_university_approval, university_approval_status,
	university_approved_by, university_approved_at, blocked_sponsor_flags, status, submitted_at, approved_by, approved_at,
	rejection_reason, cancelled_at, cancel_reason, terms_version, created_by, created_at, updated_at`

// offerings starts a query on offerings with the model's columns.
//
// cross-org: a query builder; every caller scopes it (ScopeOrg or its own
// cross-org reason).
func (s gormStore) offerings(ctx context.Context) *gorm.DB {
	return db.Conn(ctx, s.orm).Model(&Offering{}).Select(offeringSelect)
}

// InsertOffering implements Store.
//
// cross-org: a new row carries its own org_id (the seller org the service
// resolved).
func (s gormStore) InsertOffering(ctx context.Context, o Offering) (Offering, error) {
	if o.Deliverables == nil {
		o.Deliverables = Deliverables{}
	}
	if o.BlockedSponsorFlags == nil {
		o.BlockedSponsorFlags = []string{}
	}
	if err := db.Conn(ctx, s.orm).Create(&o).Error; err != nil {
		return Offering{}, fmt.Errorf("catalog: insert offering: %w", constraints.Translate(err))
	}
	return s.GetOffering(ctx, o.ID, false)
}

// GetOffering implements Store.
//
// cross-org: addressed by ID (the resource surface, cross-module reads);
// the service authorises the row's own org.
func (s gormStore) GetOffering(ctx context.Context, id uuid.UUID, lock bool) (Offering, error) {
	return take[Offering]("get offering", lockIf(s.offerings(ctx), lock).Where("id = ?", id))
}

// UpdateOffering implements Store: a targeted UPDATE of the mutable columns
// guarded by the expected status (06 H-56: no Save; seller_kind, org_id,
// currency, compensation_type, quantity_sold, created_by and created_at
// are never written here).
//
// cross-org: addressed by ID; the service has locked and authorised the
// row in the same transaction.
func (s gormStore) UpdateOffering(ctx context.Context, o Offering, from OfferingStatus) (Offering, error) {
	if o.Deliverables == nil {
		o.Deliverables = Deliverables{}
	}
	if o.BlockedSponsorFlags == nil {
		o.BlockedSponsorFlags = []string{}
	}
	var rows []Offering
	err := db.Conn(ctx, s.orm).Raw(`UPDATE offerings SET
		service_id = @service_id, event_id = @event_id, default_payee_id = @default_payee_id, title = @title,
		description_json = @description_json, deliverables = @deliverables, unit_price_amount = @unit_price_amount,
		vat_rate_bp = @vat_rate_bp, quantity_total = @quantity_total, per_buyer_limit = @per_buyer_limit,
		sales_start_at = @sales_start_at, sales_end_at = @sales_end_at, fulfil_by = @fulfil_by,
		invoice_trigger = @invoice_trigger, requires_university_approval = @requires_university_approval,
		university_approval_status = @university_approval_status, university_approved_by = @university_approved_by,
		university_approved_at = @university_approved_at, blocked_sponsor_flags = @blocked_sponsor_flags,
		status = @status, submitted_at = @submitted_at, approved_by = @approved_by, approved_at = @approved_at,
		rejection_reason = @rejection_reason, cancelled_at = @cancelled_at, cancel_reason = @cancel_reason,
		terms_version = @terms_version
		WHERE id = @id AND status = @from
		RETURNING `+offeringSelect, map[string]any{
		"id": o.ID, "from": from, "service_id": o.ServiceID, "event_id": o.EventID, "default_payee_id": o.DefaultPayeeID,
		"title": o.Title, "description_json": o.DescriptionJSON, "deliverables": o.Deliverables,
		"unit_price_amount": o.UnitPriceAmount, "vat_rate_bp": o.VATRateBP, "quantity_total": o.QuantityTotal,
		"per_buyer_limit": o.PerBuyerLimit, "sales_start_at": o.SalesStartAt, "sales_end_at": o.SalesEndAt,
		"fulfil_by": o.FulfilBy, "invoice_trigger": o.InvoiceTrigger,
		"requires_university_approval": o.RequiresUniversityApproval, "university_approval_status": o.UniversityApprovalStatus,
		"university_approved_by": o.UniversityApprovedBy, "university_approved_at": o.UniversityApprovedAt,
		"blocked_sponsor_flags": o.BlockedSponsorFlags, "status": o.Status, "submitted_at": o.SubmittedAt,
		"approved_by": o.ApprovedBy, "approved_at": o.ApprovedAt, "rejection_reason": o.RejectionReason,
		"cancelled_at": o.CancelledAt, "cancel_reason": o.CancelReason, "terms_version": o.TermsVersion,
	}).Scan(&rows).Error
	if err != nil {
		return Offering{}, fmt.Errorf("catalog: update offering: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetOffering(ctx, o.ID, false); err != nil {
			return Offering{}, err
		}
		return Offering{}, ErrInvalidState
	}
	return rows[0], nil
}

// ListOrgOfferings implements Store.
func (s gormStore) ListOrgOfferings(ctx context.Context, orgID uuid.UUID, f OrgOfferingFilter, after *TimeKey, limit int) ([]Offering, error) {
	q := s.offerings(ctx).Scopes(db.ScopeOrg(orgID)).Order("created_at DESC, id DESC").Limit(limit)
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if after != nil {
		q = q.Where("(created_at, id) < ( ?, ?)", after.At, after.ID)
	}
	out := []Offering{}
	if err := q.Find(&out).Error; err != nil {
		return nil, fmt.Errorf("catalog: org offerings: %w", err)
	}
	return out, nil
}

// BrowseOfferings implements Store.
//
// cross-org: the marketplace lists every seller's active offerings (03
// §5.14 browse: every signed-in user).
func (s gormStore) BrowseOfferings(ctx context.Context, f BrowseFilter, after *BrowseKey, limit int) ([]Offering, error) {
	q := s.offerings(ctx).Where("status = 'active' AND (sales_end_at IS NULL OR sales_end_at > ?)", f.Now).Limit(limit)
	if f.Q != "" {
		q = q.Where("search_vector @@ websearch_to_tsquery('public.koro_tr'::regconfig, ?)", f.Q)
	}
	if f.OrgID != nil {
		q = q.Where("org_id = ?", *f.OrgID)
	}
	if f.OrgIDs != nil {
		q = q.Where("org_id = ANY( ?)", db.UUIDArray(f.OrgIDs))
	}
	if f.EventID != nil {
		q = q.Where("event_id = ?", *f.EventID)
	}
	if f.PriceMax != nil {
		q = q.Where("unit_price_amount <= ?", *f.PriceMax)
	}
	if f.Category != "" {
		q = q.Where("service_id IN (SELECT id FROM services WHERE category = ?)", f.Category)
	}
	const end = "coalesce(sales_end_at, '9999-12-31T00:00:00Z'::timestamptz)"
	switch f.Sort {
	case SortPrice:
		q = q.Order("unit_price_amount, id")
		if after != nil {
			q = q.Where("(unit_price_amount, id) > ( ?, ?)", after.Price, after.ID)
		}
	case SortSalesEnd:
		q = q.Order(end + ", id")
		if after != nil {
			q = q.Where("("+end+", id) > ( ?, ?)", after.At, after.ID)
		}
	default:
		q = q.Order("created_at DESC, id DESC")
		if after != nil {
			q = q.Where("(created_at, id) < ( ?, ?)", after.At, after.ID)
		}
	}
	out := []Offering{}
	if err := q.Find(&out).Error; err != nil {
		return nil, fmt.Errorf("catalog: browse offerings: %w", err)
	}
	return out, nil
}

// queue returns a page of a review queue, oldest submission first.
//
// cross-org: the review queues span every seller (see ReviewQueue,
// UniversityQueue).
func (s gormStore) queue(ctx context.Context, cond string, after *TimeKey, limit int) ([]Offering, error) {
	q := s.offerings(ctx).Where(cond).Order("submitted_at, id").Limit(limit)
	if after != nil {
		q = q.Where("(submitted_at, id) > ( ?, ?)", after.At, after.ID)
	}
	out := []Offering{}
	if err := q.Find(&out).Error; err != nil {
		return nil, fmt.Errorf("catalog: review queue: %w", err)
	}
	return out, nil
}

// ReviewQueue implements Store.
//
// cross-org: the platform's review queue spans every seller.
func (s gormStore) ReviewQueue(ctx context.Context, after *TimeKey, limit int) ([]Offering, error) {
	return s.queue(ctx, "status = 'pending_approval' AND approved_by IS NULL", after, limit)
}

// UniversityQueue implements Store.
//
// cross-org: every university's pending decisions; the service keeps the
// clubs of one university (directory has no university-by-org lookup).
func (s gormStore) UniversityQueue(ctx context.Context, after *TimeKey, limit int) ([]Offering, error) {
	return s.queue(ctx, "status = 'pending_approval' AND requires_university_approval AND university_approval_status = 'pending'",
		after, limit)
}

// LiveEventOfferings implements Store.
func (s gormStore) LiveEventOfferings(ctx context.Context, orgID, eventID uuid.UUID) ([]Offering, error) {
	out := []Offering{}
	err := lockIf(s.offerings(ctx), true).Scopes(db.ScopeOrg(orgID)).
		Where("event_id = ? AND status <> 'cancelled'", eventID).Order("id").Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("catalog: event offerings: %w", err)
	}
	return out, nil
}

// Reserve implements Store: the conditional UPDATE of 02 §18.2 plus the
// sales window at now (offerings_stock_chk / _sold_out_chk are the second
// layer).
//
// cross-org: addressed by ID (orders reserves the offering of its line).
func (s gormStore) Reserve(ctx context.Context, id uuid.UUID, qty int, now time.Time) (Offering, bool, error) {
	var rows []Offering
	err := db.Conn(ctx, s.orm).Raw(`UPDATE offerings SET quantity_sold = quantity_sold + @qty,
		status = CASE WHEN quantity_total IS NOT NULL AND quantity_sold + @qty = quantity_total THEN 'sold_out' ELSE status END
		WHERE id = @id AND status = 'active'
		  AND (quantity_total IS NULL OR quantity_sold + @qty <= quantity_total)
		  AND (sales_start_at IS NULL OR sales_start_at <= @now) AND (sales_end_at IS NULL OR sales_end_at > @now)
		RETURNING `+offeringSelect, map[string]any{"id": id, "qty": qty, "now": now}).Scan(&rows).Error
	if err != nil {
		return Offering{}, false, fmt.Errorf("catalog: reserve: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		o, err := s.GetOffering(ctx, id, false)
		return o, false, err
	}
	return rows[0], true, nil
}

// Release implements Store.
//
// cross-org: addressed by ID (orders releases the offering of its line).
func (s gormStore) Release(ctx context.Context, id uuid.UUID, qty int) (Offering, bool, error) {
	var rows []Offering
	err := db.Conn(ctx, s.orm).Raw(`UPDATE offerings SET quantity_sold = quantity_sold - @qty,
		status = CASE WHEN status = 'sold_out' THEN 'active' ELSE status END
		WHERE id = @id AND quantity_sold >= @qty
		RETURNING `+offeringSelect, map[string]any{"id": id, "qty": qty}).Scan(&rows).Error
	if err != nil {
		return Offering{}, false, fmt.Errorf("catalog: release: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		o, err := s.GetOffering(ctx, id, false)
		return o, false, err
	}
	return rows[0], true, nil
}

// ---- commerce policies

// InsertPolicy implements Store.
//
// cross-org: platform configuration (scope_id may name an org).
func (s gormStore) InsertPolicy(ctx context.Context, p CommercePolicy) (CommercePolicy, error) {
	if err := db.Conn(ctx, s.orm).Create(&p).Error; err != nil {
		return CommercePolicy{}, fmt.Errorf("catalog: insert policy: %w", constraints.Translate(err))
	}
	return s.GetPolicy(ctx, p.ID, false)
}

// GetPolicy implements Store.
//
// cross-org: platform configuration.
func (s gormStore) GetPolicy(ctx context.Context, id uuid.UUID, lock bool) (CommercePolicy, error) {
	return take[CommercePolicy]("get policy", lockIf(db.Conn(ctx, s.orm), lock).Where("id = ?", id))
}

// ClosePolicy implements Store: the only UPDATE koro_policy_close_only lets
// through (valid_to NULL → value, nothing else).
//
// cross-org: platform configuration.
func (s gormStore) ClosePolicy(ctx context.Context, id uuid.UUID, validTo time.Time) (CommercePolicy, error) {
	var rows []CommercePolicy
	err := db.Conn(ctx, s.orm).Raw(`UPDATE commerce_policies SET valid_to = @valid_to
		WHERE id = @id AND valid_to IS NULL RETURNING *`, map[string]any{"id": id, "valid_to": validTo}).Scan(&rows).Error
	if err != nil {
		return CommercePolicy{}, fmt.Errorf("catalog: close policy: %w", constraints.Translate(err))
	}
	if len(rows) == 0 {
		if _, err := s.GetPolicy(ctx, id, false); err != nil {
			return CommercePolicy{}, err
		}
		return CommercePolicy{}, ErrInvalidState
	}
	return rows[0], nil
}

// ListPolicies implements Store.
//
// cross-org: platform configuration.
func (s gormStore) ListPolicies(ctx context.Context, f PolicyFilter) ([]CommercePolicy, error) {
	q := db.Conn(ctx, s.orm).Order("valid_from DESC, id DESC")
	if f.Scope != "" {
		q = q.Where("scope = ?", f.Scope)
	}
	if f.At != nil {
		q = q.Where("valid_from <= ? AND (valid_to IS NULL OR valid_to > ?)", *f.At, *f.At)
	}
	out := []CommercePolicy{}
	if err := q.Find(&out).Error; err != nil {
		return nil, fmt.Errorf("catalog: list policies: %w", err)
	}
	return out, nil
}

// PolicyAt implements Store: one query over the four scopes, ranked
// offering > service > seller_org > default (commerce_policies_lookup_idx).
//
// cross-org: platform configuration.
func (s gormStore) PolicyAt(ctx context.Context, offeringID, serviceID, sellerOrgID uuid.UUID, t time.Time) (CommercePolicy, bool, error) {
	var rows []CommercePolicy
	err := db.Conn(ctx, s.orm).Raw(strings.TrimSpace(`
		SELECT * FROM commerce_policies
		 WHERE ((scope = 'offering' AND scope_id = @offering) OR (scope = 'service' AND scope_id = @service)
		        OR (scope = 'seller_org' AND scope_id = @org) OR scope = 'default')
		   AND valid_from <= @at AND (valid_to IS NULL OR valid_to > @at)
		 ORDER BY CASE scope WHEN 'offering' THEN 0 WHEN 'service' THEN 1 WHEN 'seller_org' THEN 2 ELSE 3 END
		 LIMIT 1`), map[string]any{"offering": offeringID, "service": serviceID, "org": sellerOrgID, "at": t}).Scan(&rows).Error
	if err != nil {
		return CommercePolicy{}, false, fmt.Errorf("catalog: policy at: %w", err)
	}
	if len(rows) == 0 {
		return CommercePolicy{}, false, nil
	}
	return rows[0], true, nil
}
