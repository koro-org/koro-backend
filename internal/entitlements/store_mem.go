package entitlements

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// errCheck mirrors a CHECK violation of 02 §10 (the GORM store gets 422
// from db.Translate).
var errCheck = problem.ErrValidation.Wrap(errors.New("entitlements: check constraint violated"))

// memStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It follows the GORM store's contract, including the CHECK and
// unique constraints a test could otherwise only meet in PostgreSQL. Every
// method is atomic under one mutex, so TryConsume is as race-free as the
// conditional UPDATE; it has no transactions (db.NopTx).
type memStore struct {
	mu     sync.Mutex
	rows   map[uuid.UUID]Entitlement
	usages []Usage // insertion order
	// now stamps updated_at like the entitlements_touch_updated_at trigger.
	now func() time.Time
}

// NewMemStore returns an empty in-memory Store; now stamps updated_at.
func NewMemStore(now func() time.Time) Store {
	return &memStore{rows: map[uuid.UUID]Entitlement{}, now: now}
}

// check applies the CHECK constraints of 02 §10.1.
func check(e Entitlement) error {
	switch {
	case !e.Feature.Valid(), !e.Status.Valid(), !e.SourceType.Valid(), e.OrgID == uuid.Nil():
		return errCheck
	case e.Kind != KindQuota && e.Kind != KindAccess:
		return errCheck
	case (e.Kind == KindQuota) != (e.QuantityTotal != nil): // entitlements_quota_chk
		return errCheck
	case (e.Feature == FeatureDiscoveryAccess) != (e.Kind == KindAccess): // entitlements_access_feature_chk
		return errCheck
	case e.QuantityTotal != nil && (*e.QuantityTotal < 1 || e.QuantityUsed > *e.QuantityTotal):
		return errCheck
	case e.QuantityUsed < 0, !e.ValidTo.After(e.ValidFrom):
		return errCheck
	case e.SourceType != SourceAdminGrant && e.SourceID == nil: // entitlements_source_chk
		return errCheck
	case e.SourceType == SourceAdminGrant && e.GrantedBy == nil: // entitlements_granted_by_chk
		return errCheck
	case (e.Status == StatusActive || e.Status == StatusExhausted) && e.ActivatedAt == nil:
		return errCheck
	case (e.Status == StatusRevoked) != (e.RevokedAt != nil):
		return errCheck
	case e.RevokeReason != nil && len([]rune(*e.RevokeReason)) > 300:
		return errCheck
	}
	return nil
}

// Insert implements Store.
func (s *memStore) Insert(_ context.Context, e Entitlement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := check(e); err != nil {
		return err
	}
	if _, dup := s.rows[e.ID]; dup {
		return problem.ErrConflict
	}
	if e.SourceID != nil { // entitlements_source_uq
		for _, o := range s.rows {
			if o.SourceType == e.SourceType && o.SourceID != nil && *o.SourceID == *e.SourceID && o.Feature == e.Feature {
				return problem.ErrConflict
			}
		}
	}
	s.rows[e.ID] = clone(e)
	return nil
}

func clone(e Entitlement) Entitlement {
	c := e
	if e.QuantityTotal != nil {
		v := *e.QuantityTotal
		c.QuantityTotal = &v
	}
	if e.RevokeReason != nil {
		v := *e.RevokeReason
		c.RevokeReason = &v
	}
	return c
}

// Get implements Store.
func (s *memStore) Get(_ context.Context, orgID, id uuid.UUID) (Entitlement, error) {
	if orgID == uuid.Nil() {
		return Entitlement{}, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[id]
	if !ok || e.OrgID != orgID {
		return Entitlement{}, ErrNotFound
	}
	return clone(e), nil
}

// Lock implements Store (no locking: tests run one change at a time per row).
func (s *memStore) Lock(_ context.Context, id uuid.UUID) (Entitlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[id]
	if !ok {
		return Entitlement{}, ErrNotFound
	}
	return clone(e), nil
}

// sortedDesc returns the rows matching keep, newest (largest ID) first.
func (s *memStore) sortedDesc(keep func(Entitlement) bool) []Entitlement {
	var out []Entitlement
	for _, e := range s.rows {
		if keep(e) {
			out = append(out, clone(e))
		}
	}
	slices.SortFunc(out, func(a, b Entitlement) int { return b.ID.Compare(a.ID) })
	return out
}

// List implements Store.
func (s *memStore) List(_ context.Context, orgID uuid.UUID, f ListFilter, after *uuid.UUID, limit int) ([]Entitlement, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sortedDesc(func(e Entitlement) bool {
		return e.OrgID == orgID && (f.Feature == "" || e.Feature == f.Feature) &&
			(f.Status == "" || e.Status == f.Status) && (after == nil || e.ID.Compare(*after) < 0)
	})
	return out[:min(limit, len(out))], nil
}

// Usable implements Store.
func (s *memStore) Usable(_ context.Context, orgID uuid.UUID, f Feature, now time.Time) ([]Entitlement, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entitlement
	for _, e := range s.rows {
		if e.OrgID == orgID && e.Feature == f && e.usableAt(now) {
			out = append(out, clone(e))
		}
	}
	slices.SortFunc(out, func(a, b Entitlement) int {
		return cmp.Or(a.ValidTo.Compare(b.ValidTo), a.ID.Compare(b.ID))
	})
	return out[:min(usableLimit, len(out))], nil
}

// BySource implements Store.
func (s *memStore) BySource(_ context.Context, t SourceType, sourceID uuid.UUID) ([]Entitlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sortedDesc(func(e Entitlement) bool {
		return e.SourceType == t && e.SourceID != nil && *e.SourceID == sourceID
	})
	slices.Reverse(out)
	return out, nil
}

// update stores e after the CHECKs, stamping updated_at.
func (s *memStore) update(e Entitlement) error {
	if err := check(e); err != nil {
		return err
	}
	e.UpdatedAt = s.now()
	s.rows[e.ID] = clone(e)
	return nil
}

// TryConsume implements Store.
func (s *memStore) TryConsume(_ context.Context, orgID, id uuid.UUID, qty int, now time.Time) (Entitlement, bool, error) {
	if orgID == uuid.Nil() {
		return Entitlement{}, false, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[id]
	if !ok || e.OrgID != orgID || e.Kind != KindQuota || !e.usableAt(now) || e.QuantityUsed+qty > *e.QuantityTotal {
		return Entitlement{}, false, nil
	}
	e.QuantityUsed += qty
	if e.QuantityUsed == *e.QuantityTotal {
		e.Status = StatusExhausted
	}
	if err := s.update(e); err != nil {
		return Entitlement{}, false, err
	}
	return clone(s.rows[id]), true, nil
}

// Unconsume implements Store.
func (s *memStore) Unconsume(_ context.Context, id uuid.UUID, qty int, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[id]
	if !ok || e.QuantityUsed < qty {
		return ErrNotFound
	}
	e.QuantityUsed -= qty
	if e.Status == StatusExhausted {
		e.Status = StatusExpired
		if e.ValidTo.After(now) {
			e.Status = StatusActive
		}
	}
	return s.update(e)
}

// Activate implements Store.
func (s *memStore) Activate(_ context.Context, t SourceType, sourceID uuid.UUID, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.rows {
		if e.SourceType == t && e.SourceID != nil && *e.SourceID == sourceID && e.Status == StatusPending {
			e.Status, e.ActivatedAt = StatusActive, &now
			if err := s.update(e); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

func revocable(st Status) bool {
	return st == StatusPending || st == StatusActive || st == StatusExhausted
}

func revoke(e Entitlement, now time.Time, reason string) Entitlement {
	e.Status, e.RevokedAt, e.RevokeReason = StatusRevoked, &now, nil
	if reason != "" {
		e.RevokeReason = &reason
	}
	return e
}

// Revoke implements Store.
func (s *memStore) Revoke(_ context.Context, t SourceType, sourceID uuid.UUID, now time.Time, reason string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.rows {
		if e.SourceType == t && e.SourceID != nil && *e.SourceID == sourceID && revocable(e.Status) {
			if err := s.update(revoke(e, now, reason)); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

// RevokeOne implements Store.
func (s *memStore) RevokeOne(_ context.Context, id uuid.UUID, now time.Time, reason string) (Entitlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rows[id]
	if !ok || !revocable(e.Status) {
		return Entitlement{}, ErrNotFound
	}
	if err := s.update(revoke(e, now, reason)); err != nil {
		return Entitlement{}, err
	}
	return clone(s.rows[id]), nil
}

// Expire implements Store.
func (s *memStore) Expire(_ context.Context, now time.Time, limit int) ([]Entitlement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Entitlement
	for _, e := range s.rows {
		if e.Status == StatusActive && !e.ValidTo.After(now) {
			due = append(due, e)
		}
	}
	slices.SortFunc(due, func(a, b Entitlement) int {
		return cmp.Or(a.ValidTo.Compare(b.ValidTo), a.ID.Compare(b.ID))
	})
	due = due[:min(limit, len(due))]
	for i, e := range due {
		e.Status = StatusExpired
		if err := s.update(e); err != nil {
			return nil, err
		}
		due[i] = clone(s.rows[e.ID])
	}
	return due, nil
}

// InsertUsage implements Store.
func (s *memStore) InsertUsage(_ context.Context, u Usage) (Usage, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u.ReversalOf == nil {
		e, ok := s.rows[u.EntitlementID]
		if !ok {
			return Usage{}, false, nil
		}
		u.OrgID = e.OrgID
		if u.Quantity <= 0 || !u.RefType.Valid() {
			return Usage{}, false, errCheck
		}
		for _, o := range s.usages {
			if o.ReversalOf == nil && o.RefType == u.RefType && o.RefID == u.RefID {
				return Usage{}, false, nil // entitlement_usages_ref_uq
			}
		}
	} else {
		i := slices.IndexFunc(s.usages, func(o Usage) bool { return o.ID == *u.ReversalOf && o.ReversalOf == nil })
		if i < 0 {
			return Usage{}, false, nil
		}
		o := s.usages[i]
		for _, r := range s.usages {
			if r.ReversalOf != nil && *r.ReversalOf == o.ID {
				return Usage{}, false, nil // entitlement_usages_reversal_uq
			}
		}
		u.EntitlementID, u.OrgID, u.Quantity, u.RefType, u.RefID = o.EntitlementID, o.OrgID, -o.Quantity, o.RefType, o.RefID
	}
	s.usages = append(s.usages, u)
	return u, true, nil
}

// UsageByRef implements Store.
func (s *memStore) UsageByRef(_ context.Context, ref Ref) (Usage, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.usages, func(o Usage) bool {
		return o.ReversalOf == nil && o.RefType == ref.Type && o.RefID == ref.ID
	})
	if i < 0 {
		return Usage{}, false, ErrNotFound
	}
	u := s.usages[i]
	reversed := slices.ContainsFunc(s.usages, func(r Usage) bool { return r.ReversalOf != nil && *r.ReversalOf == u.ID })
	return u, reversed, nil
}

// ListUsages implements Store.
func (s *memStore) ListUsages(_ context.Context, orgID uuid.UUID, f UsageFilter, after *uuid.UUID, limit int) ([]UsageView, error) {
	if orgID == uuid.Nil() {
		return nil, db.ErrMissingOrgScope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []UsageView
	for _, u := range s.usages {
		feature := s.rows[u.EntitlementID].Feature
		if u.OrgID == orgID && (f.Feature == "" || feature == f.Feature) && (after == nil || u.ID.Compare(*after) < 0) {
			out = append(out, UsageView{Usage: u, Feature: feature})
		}
	}
	slices.SortFunc(out, func(a, b UsageView) int { return b.ID.Compare(a.ID) })
	return out[:min(limit, len(out))], nil
}
