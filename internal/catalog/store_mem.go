package catalog

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// errCheck mirrors a CHECK violation of 000098–000100 (the GORM store gets
// 422 from db.Translate).
var errCheck = problem.ErrValidation.Wrap(errors.New("catalog: check constraint violated"))

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It follows the GORM store's contract, including the CHECK,
// unique and exclusion constraints of 02 §18 (000098–000100) a test could
// otherwise only meet in PostgreSQL. Every method is atomic under one
// mutex; it has no transactions (db.NopTx), so a "lock" is a plain read.
type MemStore struct {
	mu        sync.Mutex
	services  map[uuid.UUID]ServiceItem
	offerings map[uuid.UUID]Offering
	policies  map[uuid.UUID]CommercePolicy
	// now stamps created_at and updated_at like the defaults and the
	// *_touch_updated_at triggers.
	now func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory Store; now stamps the rows.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{services: map[uuid.UUID]ServiceItem{}, offerings: map[uuid.UUID]Offering{},
		policies: map[uuid.UUID]CommercePolicy{}, now: now}
}

func (s *MemStore) stamp(prev time.Time) time.Time {
	t := s.now().UTC().Truncate(time.Microsecond)
	if !t.After(prev) {
		t = prev.Add(time.Microsecond)
	}
	return t
}

// ---- services

func memServiceOK(v ServiceItem) bool {
	n := utf8.RuneCountInString(v.NameTR)
	return serviceCodeRe.MatchString(v.Code) && n >= 2 && n <= maxServiceName && checkService(v) == nil &&
		(v.EntitlementTemplate == nil || bytes.HasPrefix(bytes.TrimSpace(v.EntitlementTemplate), []byte("{")))
}

// InsertService implements Store.
func (s *MemStore) InsertService(_ context.Context, v ServiceItem) (ServiceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !memServiceOK(v) {
		return ServiceItem{}, errCheck
	}
	for _, o := range s.services {
		if o.Code == v.Code {
			return ServiceItem{}, ErrServiceCodeTaken
		}
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = s.stamp(time.Time{})
	}
	if v.UpdatedAt.IsZero() {
		v.UpdatedAt = v.CreatedAt
	}
	s.services[v.ID] = v
	return v, nil
}

// GetService implements Store.
func (s *MemStore) GetService(_ context.Context, id uuid.UUID, _ bool) (ServiceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.services[id]
	if !ok {
		return ServiceItem{}, ErrNotFound
	}
	return v, nil
}

// UpdateService implements Store.
func (s *MemStore) UpdateService(_ context.Context, v ServiceItem) (ServiceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.services[v.ID]
	if !ok {
		return ServiceItem{}, ErrNotFound
	}
	v.Code, v.CreatedAt = cur.Code, cur.CreatedAt
	if !memServiceOK(v) {
		return ServiceItem{}, errCheck
	}
	v.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.services[v.ID] = v
	return v, nil
}

// ListServices implements Store.
func (s *MemStore) ListServices(_ context.Context, f ServiceFilter) ([]ServiceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ServiceItem{}
	for _, v := range s.services {
		if f.Category != "" && v.Category != f.Category || f.SellableOnly && !v.Sellable() {
			continue
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b ServiceItem) int {
		return cmp.Or(cmp.Compare(textx.LowerTR(a.NameTR), textx.LowerTR(b.NameTR)), compareUUID(a.ID, b.ID))
	})
	return out, nil
}

// ServicesByID implements Store.
func (s *MemStore) ServicesByID(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]ServiceItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]ServiceItem{}
	for _, id := range ids {
		if v, ok := s.services[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// ---- offerings

// memOfferingErr mirrors the CHECKs of 000099 (the four-eyes CHECK answers
// like authz, as the GORM store's translation does).
func memOfferingErr(o Offering) error {
	n := utf8.RuneCountInString(o.Title)
	approved := o.ApprovedBy != nil
	uniDecided := o.UniversityApprovalStatus == UniApproved || o.UniversityApprovalStatus == UniRejected
	switch {
	case o.ApprovedBy != nil && *o.ApprovedBy == o.CreatedBy:
		return authz.ErrFourEyes // offerings_approver_chk
	case n < minTitle || n > maxTitle,
		o.UnitPriceAmount <= 0, o.Currency != "TRY", o.VATRateBP < 0 || o.VATRateBP > 10000,
		o.CompensationType != "cash" && o.CompensationType != "in_kind",
		o.QuantityTotal != nil && *o.QuantityTotal <= 0, o.QuantitySold < 0,
		o.PerBuyerLimit != nil && *o.PerBuyerLimit <= 0,
		!o.InvoiceTrigger.Valid(), !o.Status.Valid(),
		o.SellerKind != SellerOrg && o.SellerKind != SellerPlatform,
		o.QuantityTotal != nil && o.QuantitySold > *o.QuantityTotal,
		o.Status == OfferingSoldOut && (o.QuantityTotal == nil || o.QuantitySold != *o.QuantityTotal),
		o.SalesEndAt != nil && o.SalesStartAt != nil && !o.SalesEndAt.After(*o.SalesStartAt),
		o.SellerKind == SellerOrg && o.Status != OfferingDraft && o.Status != OfferingCancelled && o.DefaultPayeeID == nil,
		o.SellerKind == SellerPlatform && o.DefaultPayeeID != nil,
		o.Status == OfferingPendingApproval && o.SubmittedAt == nil,
		(o.ApprovedBy == nil) != (o.ApprovedAt == nil),
		(o.Status == OfferingActive || o.Status == OfferingSoldOut || o.Status == OfferingClosed) && !approved,
		o.RequiresUniversityApproval != (o.UniversityApprovalStatus != UniNotRequired),
		uniDecided != (o.UniversityApprovedBy != nil) || uniDecided != (o.UniversityApprovedAt != nil),
		o.RequiresUniversityApproval && (o.Status == OfferingActive || o.Status == OfferingSoldOut) &&
			o.UniversityApprovalStatus != UniApproved,
		(o.Status == OfferingCancelled) != (o.CancelledAt != nil),
		o.CompensationType != "cash" && o.Status != OfferingDraft && o.Status != OfferingCancelled,
		o.RejectionReason != nil && utf8.RuneCountInString(*o.RejectionReason) > maxReason,
		o.CancelReason != nil && utf8.RuneCountInString(*o.CancelReason) > maxReason:
		return errCheck
	}
	return nil
}

func cloneOffering(o Offering) Offering {
	o.Deliverables = slices.Clone(o.Deliverables)
	if o.Deliverables == nil {
		o.Deliverables = Deliverables{}
	}
	o.BlockedSponsorFlags = slices.Clone(o.BlockedSponsorFlags)
	if o.BlockedSponsorFlags == nil {
		o.BlockedSponsorFlags = []string{}
	}
	o.DescriptionJSON = bytes.Clone(o.DescriptionJSON)
	return o
}

// InsertOffering implements Store.
func (s *MemStore) InsertOffering(_ context.Context, o Offering) (Offering, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.services[o.ServiceID]; !ok {
		return Offering{}, problem.ErrConflict.Wrap(errors.New("catalog: offerings_service_id_fkey"))
	}
	if o.Currency == "" {
		o.Currency = "TRY"
	}
	if o.CompensationType == "" {
		o.CompensationType = "cash"
	}
	if err := memOfferingErr(o); err != nil {
		return Offering{}, err
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = s.stamp(time.Time{})
	}
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = o.CreatedAt
	}
	o = cloneOffering(o)
	s.offerings[o.ID] = o
	return cloneOffering(o), nil
}

// GetOffering implements Store.
func (s *MemStore) GetOffering(_ context.Context, id uuid.UUID, _ bool) (Offering, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.offerings[id]
	if !ok {
		return Offering{}, ErrNotFound
	}
	return cloneOffering(o), nil
}

// UpdateOffering implements Store (the immutable columns are kept).
func (s *MemStore) UpdateOffering(_ context.Context, o Offering, from OfferingStatus) (Offering, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.offerings[o.ID]
	if !ok {
		return Offering{}, ErrNotFound
	}
	if cur.Status != from {
		return Offering{}, ErrInvalidState
	}
	o.SellerKind, o.OrgID, o.CreatedBy, o.CreatedAt, o.QuantitySold = cur.SellerKind, cur.OrgID, cur.CreatedBy, cur.CreatedAt, cur.QuantitySold
	o.Currency, o.CompensationType = cur.Currency, cur.CompensationType
	if err := memOfferingErr(o); err != nil {
		return Offering{}, err
	}
	o.UpdatedAt = s.stamp(cur.UpdatedAt)
	o = cloneOffering(o)
	s.offerings[o.ID] = o
	return cloneOffering(o), nil
}

// sorted returns the offerings matching keep, sorted by cmpFn.
func (s *MemStore) sorted(keep func(Offering) bool, cmpFn func(a, b Offering) int) []Offering {
	var out []Offering
	for _, o := range s.offerings {
		if keep(o) {
			out = append(out, cloneOffering(o))
		}
	}
	slices.SortFunc(out, cmpFn)
	return out
}

func limitOf(rows []Offering, limit int) []Offering {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	if rows == nil {
		return []Offering{}
	}
	return rows
}

// newestFirst orders by (created_at, id) descending.
func newestFirst(a, b Offering) int {
	return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), compareUUID(b.ID, a.ID))
}

// submittedFirst orders by (submitted_at, id) ascending.
func submittedFirst(a, b Offering) int {
	return cmp.Or(submittedKey(a).At.Compare(submittedKey(b).At), compareUUID(a.ID, b.ID))
}

func afterTime(k *TimeKey, at time.Time, id uuid.UUID, desc bool) bool {
	if k == nil {
		return true
	}
	c := cmp.Or(at.Compare(k.At), compareUUID(id, k.ID))
	if desc {
		return c < 0
	}
	return c > 0
}

// ListOrgOfferings implements Store.
func (s *MemStore) ListOrgOfferings(_ context.Context, orgID uuid.UUID, f OrgOfferingFilter, after *TimeKey, limit int) ([]Offering, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.sorted(func(o Offering) bool {
		return o.OrgID == orgID && (f.Status == "" || o.Status == f.Status) && afterTime(after, o.CreatedAt, o.ID, true)
	}, newestFirst)
	return limitOf(rows, limit), nil
}

// BrowseOfferings implements Store.
func (s *MemStore) BrowseOfferings(_ context.Context, f BrowseFilter, after *BrowseKey, limit int) ([]Offering, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := textx.LowerTR(f.Q)
	endKey := func(o Offering) time.Time {
		if o.SalesEndAt == nil {
			return farFuture
		}
		return *o.SalesEndAt
	}
	keyCmp := func(a, b Offering) int {
		switch f.Sort {
		case SortPrice:
			return cmp.Or(cmp.Compare(a.UnitPriceAmount, b.UnitPriceAmount), compareUUID(a.ID, b.ID))
		case SortSalesEnd:
			return cmp.Or(endKey(a).Compare(endKey(b)), compareUUID(a.ID, b.ID))
		}
		return newestFirst(a, b)
	}
	rows := s.sorted(func(o Offering) bool {
		if o.Status != OfferingActive || o.SalesEndAt != nil && !o.SalesEndAt.After(f.Now) {
			return false
		}
		if q != "" && !strings.Contains(textx.LowerTR(o.Title), q) {
			return false
		}
		if f.OrgID != nil && o.OrgID != *f.OrgID || f.OrgIDs != nil && !slices.Contains(f.OrgIDs, o.OrgID) {
			return false
		}
		if f.EventID != nil && (o.EventID == nil || *o.EventID != *f.EventID) || f.PriceMax != nil && o.UnitPriceAmount > *f.PriceMax {
			return false
		}
		if f.Category != "" && s.services[o.ServiceID].Category != f.Category {
			return false
		}
		if after == nil {
			return true
		}
		switch f.Sort {
		case SortPrice:
			return cmp.Or(cmp.Compare(o.UnitPriceAmount, after.Price), compareUUID(o.ID, after.ID)) > 0
		case SortSalesEnd:
			return cmp.Or(endKey(o).Compare(after.At), compareUUID(o.ID, after.ID)) > 0
		}
		return cmp.Or(o.CreatedAt.Compare(after.At), compareUUID(o.ID, after.ID)) < 0
	}, keyCmp)
	return limitOf(rows, limit), nil
}

// ReviewQueue implements Store.
func (s *MemStore) ReviewQueue(_ context.Context, after *TimeKey, limit int) ([]Offering, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.sorted(func(o Offering) bool {
		k := submittedKey(o)
		return o.Status == OfferingPendingApproval && o.ApprovedBy == nil && afterTime(after, k.At, k.ID, false)
	}, submittedFirst)
	return limitOf(rows, limit), nil
}

// UniversityQueue implements Store.
func (s *MemStore) UniversityQueue(_ context.Context, after *TimeKey, limit int) ([]Offering, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.sorted(func(o Offering) bool {
		k := submittedKey(o)
		return inUniversityQueue(o) && afterTime(after, k.At, k.ID, false)
	}, submittedFirst)
	return limitOf(rows, limit), nil
}

// inUniversityQueue is UniversityQueue's and CountUniversityQueue's filter.
func inUniversityQueue(o Offering) bool {
	return o.Status == OfferingPendingApproval && o.RequiresUniversityApproval && o.UniversityApprovalStatus == UniPending
}

// CountUniversityQueue implements Store (BE-163).
func (s *MemStore) CountUniversityQueue(_ context.Context, sellerOrgIDs []uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, o := range s.offerings {
		if inUniversityQueue(o) && slices.Contains(sellerOrgIDs, o.OrgID) {
			n++
		}
	}
	return n, nil
}

// LiveEventOfferings implements Store.
func (s *MemStore) LiveEventOfferings(_ context.Context, orgID, eventID uuid.UUID) ([]Offering, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return limitOf(s.sorted(func(o Offering) bool {
		return o.OrgID == orgID && o.EventID != nil && *o.EventID == eventID && o.Status != OfferingCancelled
	}, func(a, b Offering) int { return compareUUID(a.ID, b.ID) }), 0), nil
}

// Reserve implements Store: the conditional UPDATE of 02 §18.2 (plus the
// sales window at now).
func (s *MemStore) Reserve(_ context.Context, id uuid.UUID, qty int, now time.Time) (Offering, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.offerings[id]
	if !ok {
		return Offering{}, false, ErrNotFound
	}
	if !o.OnSaleAt(now) || o.QuantityTotal != nil && o.QuantitySold+qty > *o.QuantityTotal {
		return cloneOffering(o), false, nil
	}
	o.QuantitySold += qty
	if o.QuantityTotal != nil && o.QuantitySold == *o.QuantityTotal {
		o.Status = OfferingSoldOut
	}
	o.UpdatedAt = s.stamp(o.UpdatedAt)
	s.offerings[id] = o
	return cloneOffering(o), true, nil
}

// Release implements Store.
func (s *MemStore) Release(_ context.Context, id uuid.UUID, qty int) (Offering, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.offerings[id]
	if !ok {
		return Offering{}, false, ErrNotFound
	}
	if o.QuantitySold < qty {
		return cloneOffering(o), false, nil
	}
	o.QuantitySold -= qty
	if o.Status == OfferingSoldOut {
		o.Status = OfferingActive
	}
	o.UpdatedAt = s.stamp(o.UpdatedAt)
	s.offerings[id] = o
	return cloneOffering(o), true, nil
}

// ---- commerce policies

func memPolicyOK(p CommercePolicy) bool {
	switch {
	case !p.Scope.Valid(), !p.ModelCode.Valid(), !p.CollectionMethod.Valid(),
		(p.Scope == ScopeDefault) != (p.ScopeID == nil),
		(p.ModelCode == ModelC) != (p.CollectionMethod == CollectDirectToPayee),
		p.ModelCode == ModelC && p.CommissionPayer != PayerBuyer,
		p.CommissionRateBP < 0 || p.CommissionRateBP > 10000, p.CommissionVATRateBP < 0 || p.CommissionVATRateBP > 10000,
		p.CommissionMinAmount != nil && *p.CommissionMinAmount < 0, p.CommissionMaxAmount != nil && *p.CommissionMaxAmount < 0,
		p.CommissionMinAmount != nil && p.CommissionMaxAmount != nil && *p.CommissionMinAmount > *p.CommissionMaxAmount,
		p.DisputeWindowDays < 0 || p.DisputeWindowDays > 60, p.PayoutHoldDays < 0 || p.PayoutHoldDays > 90,
		p.ValidTo != nil && !p.ValidTo.After(p.ValidFrom):
		return false
	}
	return true
}

func overlaps(a, b CommercePolicy) bool {
	if a.Scope != b.Scope || (a.ScopeID == nil) != (b.ScopeID == nil) || a.ScopeID != nil && *a.ScopeID != *b.ScopeID {
		return false
	}
	aEnd, bEnd := farFuture, farFuture
	if a.ValidTo != nil {
		aEnd = *a.ValidTo
	}
	if b.ValidTo != nil {
		bEnd = *b.ValidTo
	}
	return a.ValidFrom.Before(bEnd) && b.ValidFrom.Before(aEnd)
}

// InsertPolicy implements Store.
func (s *MemStore) InsertPolicy(_ context.Context, p CommercePolicy) (CommercePolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !memPolicyOK(p) {
		return CommercePolicy{}, errCheck
	}
	for _, o := range s.policies {
		if overlaps(o, p) {
			return CommercePolicy{}, ErrPolicyOverlap
		}
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = s.stamp(time.Time{})
	}
	s.policies[p.ID] = p
	return p, nil
}

// GetPolicy implements Store.
func (s *MemStore) GetPolicy(_ context.Context, id uuid.UUID, _ bool) (CommercePolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.policies[id]
	if !ok {
		return CommercePolicy{}, ErrNotFound
	}
	return p, nil
}

// ClosePolicy implements Store (koro_policy_close_only: valid_to once).
func (s *MemStore) ClosePolicy(_ context.Context, id uuid.UUID, validTo time.Time) (CommercePolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.policies[id]
	switch {
	case !ok:
		return CommercePolicy{}, ErrNotFound
	case p.ValidTo != nil:
		return CommercePolicy{}, ErrInvalidState
	case !validTo.After(p.ValidFrom):
		return CommercePolicy{}, errCheck
	}
	t := validTo.UTC().Truncate(time.Microsecond)
	p.ValidTo = &t
	s.policies[id] = p
	return p, nil
}

// ListPolicies implements Store.
func (s *MemStore) ListPolicies(_ context.Context, f PolicyFilter) ([]CommercePolicy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []CommercePolicy{}
	for _, p := range s.policies {
		if f.Scope != "" && p.Scope != f.Scope || f.At != nil && !p.ValidAt(*f.At) {
			continue
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b CommercePolicy) int {
		return cmp.Or(b.ValidFrom.Compare(a.ValidFrom), compareUUID(b.ID, a.ID))
	})
	return out, nil
}

// PolicyAt implements Store.
func (s *MemStore) PolicyAt(_ context.Context, offeringID, serviceID, sellerOrgID uuid.UUID, t time.Time) (CommercePolicy, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best CommercePolicy
	found := false
	for _, p := range s.policies {
		match := p.Scope == ScopeDefault ||
			p.ScopeID != nil && (p.Scope == ScopeOffering && *p.ScopeID == offeringID ||
				p.Scope == ScopeService && *p.ScopeID == serviceID || p.Scope == ScopeSellerOrg && *p.ScopeID == sellerOrgID)
		if !match || !p.ValidAt(t) {
			continue
		}
		if !found || p.Scope.rank() < best.Scope.rank() {
			best, found = p, true
		}
	}
	return best, found, nil
}
