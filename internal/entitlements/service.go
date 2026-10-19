package entitlements

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/problem"
)

// Limits of the inputs.
const (
	maxQuantity     = 100_000 // one grant; a pilot package is tens of units
	maxConsume      = 1_000   // one consumption (callers consume 1)
	maxReasonRunes  = 300     // entitlements_revoke_reason_chk
	maxValidityDays = 3 * 366 // a grant runs at most three years
)

type service struct{ d Deps }

var _ Service = (*service)(nil)

// grantErr builds a 422 naming the offending member of a grant.
func grantErr(location, message string) error {
	return ErrInvalidGrant.WithFields(problem.FieldError{Location: location, Message: message})
}

// checkGrant validates the shape of a grant (02 §10.1 CHECKs plus bounds).
// prefix is the field location prefix ("body." on the HTTP path).
func checkGrant(in GrantInput, prefix string) error {
	switch {
	case in.OrgID == uuid.Nil():
		return grantErr(prefix+"org_id", "is required")
	case !in.Feature.Valid():
		return grantErr(prefix+"feature", "is not a known feature")
	case in.Feature.Kind() == KindQuota && in.Quantity == nil:
		return grantErr(prefix+"quantity_total", "is required for a quota feature")
	case in.Feature.Kind() == KindAccess && in.Quantity != nil:
		return grantErr(prefix+"quantity_total", "must be empty for an access right")
	case in.Quantity != nil && (*in.Quantity < 1 || *in.Quantity > maxQuantity):
		return grantErr(prefix+"quantity_total", fmt.Sprintf("must be between 1 and %d", maxQuantity))
	case in.ValidFrom.IsZero() || in.ValidTo.IsZero():
		return grantErr(prefix+"valid_to", "valid_from and valid_to are required")
	case !in.ValidTo.After(in.ValidFrom):
		return grantErr(prefix+"valid_to", "must be after valid_from")
	case in.ValidTo.Sub(in.ValidFrom) > maxValidityDays*24*time.Hour:
		return grantErr(prefix+"valid_to", "the validity window is longer than three years")
	case in.Status != StatusPending && in.Status != StatusActive:
		return grantErr(prefix+"status", "must be pending or active")
	case !in.SourceType.Valid():
		return grantErr(prefix+"source_type", "is not a known source type")
	case in.SourceType == SourceAdminGrant && (in.GrantedBy == nil || *in.GrantedBy == uuid.Nil()):
		return grantErr(prefix+"granted_by", "is required for an admin grant")
	case in.SourceType != SourceAdminGrant && (in.SourceID == nil || *in.SourceID == uuid.Nil()):
		return grantErr(prefix+"source_id", "is required for this source type")
	}
	return nil
}

// Grant implements Entitlements.
func (s *service) Grant(ctx context.Context, in GrantInput) (uuid.UUID, error) {
	if err := checkGrant(in, ""); err != nil {
		return uuid.Nil(), err
	}
	var id uuid.UUID
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, err := s.grant(ctx, in)
		id = e.ID
		return err
	})
	return id, err
}

// grant writes the entitlement of a validated input in ctx's transaction.
func (s *service) grant(ctx context.Context, in GrantInput) (Entitlement, error) {
	if in.SourceID != nil { // a source grants a feature once (entitlements_source_uq)
		prev, err := s.d.Store.BySource(ctx, in.SourceType, *in.SourceID)
		if err != nil {
			return Entitlement{}, err
		}
		for _, p := range prev {
			if p.Feature != in.Feature {
				continue
			}
			if p.OrgID != in.OrgID {
				return Entitlement{}, problem.ErrConflict.Wrap(errors.New("entitlements: source already granted to another org"))
			}
			return p, nil
		}
	}
	now := s.d.Clock.Now()
	e := Entitlement{
		ID: uuid.NewV7(), OrgID: in.OrgID, Feature: in.Feature, Kind: in.Feature.Kind(), QuantityTotal: in.Quantity,
		ValidFrom: in.ValidFrom.UTC(), ValidTo: in.ValidTo.UTC(), Status: in.Status, SourceType: in.SourceType,
		SourceID: in.SourceID, GrantedBy: in.GrantedBy,
	}
	if e.Status == StatusActive {
		e.ActivatedAt = &now
	}
	if err := s.d.Store.Insert(ctx, e); err != nil {
		return Entitlement{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "entitlement granted",
		slog.String("entitlement_id", e.ID.String()), slog.String("org_id", e.OrgID.String()),
		slog.String("feature", string(e.Feature)), slog.String("source_type", string(e.SourceType)),
		slog.String("status", string(e.Status)))
	return s.d.Store.Get(ctx, e.OrgID, e.ID) // created_at and updated_at as stored (BE-069)
}

// ActivateBySource implements Entitlements.
func (s *service) ActivateBySource(ctx context.Context, t SourceType, sourceID uuid.UUID) error {
	if !t.Valid() || sourceID == uuid.Nil() {
		return fmt.Errorf("entitlements: activate: invalid source %q/%s", t, sourceID)
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		n, err := s.d.Store.Activate(ctx, t, sourceID, s.d.Clock.Now())
		if err == nil && n > 0 {
			s.d.Log.LogAttrs(ctx, slog.LevelInfo, "entitlements activated",
				slog.String("source_type", string(t)), slog.String("source_id", sourceID.String()), slog.Int("count", n))
		}
		return err
	})
}

// checkReason validates a revocation reason (entitlements_revoke_reason_chk).
func checkReason(reason, location string) error {
	if utf8.RuneCountInString(reason) > maxReasonRunes {
		return problem.ErrValidation.WithFields(problem.FieldError{Location: location, Message: "must be at most 300 characters"})
	}
	return nil
}

// RevokeBySource implements Entitlements.
func (s *service) RevokeBySource(ctx context.Context, t SourceType, sourceID uuid.UUID, reason string) error {
	if !t.Valid() || sourceID == uuid.Nil() {
		return fmt.Errorf("entitlements: revoke: invalid source %q/%s", t, sourceID)
	}
	if err := checkReason(reason, "reason"); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		n, err := s.d.Store.Revoke(ctx, t, sourceID, s.d.Clock.Now(), reason)
		if err == nil && n > 0 {
			s.d.Log.LogAttrs(ctx, slog.LevelInfo, "entitlements revoked",
				slog.String("source_type", string(t)), slog.String("source_id", sourceID.String()), slog.Int("count", n))
		}
		return err
	})
}

// actorOf returns the user of ctx's actor (HTTP paths), or nil (workers).
func actorOf(ctx context.Context) *uuid.UUID {
	if a := authz.ActorOrAnonymous(ctx); a.IsUser() && a.UserID != uuid.Nil() {
		id := a.UserID
		return &id
	}
	return nil
}

// Consume implements Entitlements.
func (s *service) Consume(ctx context.Context, orgID uuid.UUID, f Feature, qty int, ref Ref) (uuid.UUID, error) {
	switch {
	case orgID == uuid.Nil():
		return uuid.Nil(), errors.New("entitlements: consume: org required")
	case !f.Valid() || f.Kind() != KindQuota:
		return uuid.Nil(), fmt.Errorf("entitlements: consume: %q is not a quota feature", f)
	case qty < 1 || qty > maxConsume:
		return uuid.Nil(), fmt.Errorf("entitlements: consume: quantity %d out of range", qty)
	case ref.Type != f.RefType() || ref.ID == uuid.Nil():
		return uuid.Nil(), fmt.Errorf("entitlements: consume: ref %q/%s does not fit feature %q", ref.Type, ref.ID, f)
	}
	var id uuid.UUID
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		id, err = s.consume(ctx, orgID, f, qty, ref)
		return err
	})
	if err != nil {
		return uuid.Nil(), err
	}
	return id, nil
}

// consume runs in the caller's transaction.
func (s *service) consume(ctx context.Context, orgID uuid.UUID, f Feature, qty int, ref Ref) (uuid.UUID, error) {
	// A retry of the same ref (the caller's own retry or replay).
	if id, done, err := s.consumed(ctx, orgID, ref); done || err != nil {
		return id, err
	}
	now := s.d.Clock.Now()
	cands, err := s.d.Store.Usable(ctx, orgID, f, now)
	if err != nil {
		return uuid.Nil(), err
	}
	for _, c := range cands {
		e, ok, err := s.d.Store.TryConsume(ctx, orgID, c.ID, qty, now)
		if err != nil {
			return uuid.Nil(), err
		}
		if !ok {
			continue // used up or ended meanwhile: the next one (02 §10.1)
		}
		u, inserted, err := s.d.Store.InsertUsage(ctx, Usage{
			ID: uuid.NewV7(), EntitlementID: e.ID, Quantity: qty, RefType: ref.Type, RefID: ref.ID,
			ActorUserID: actorOf(ctx), CreatedAt: now,
		})
		if err != nil {
			return uuid.Nil(), err
		}
		if !inserted {
			// A concurrent consumption of the same ref committed first:
			// give the unit back and answer with its usage.
			if err := s.d.Store.Unconsume(ctx, e.ID, qty, now); err != nil {
				return uuid.Nil(), err
			}
			id, done, err := s.consumed(ctx, orgID, ref)
			if err == nil && !done {
				err = fmt.Errorf("entitlements: usage of %s/%s neither inserted nor found", ref.Type, ref.ID)
			}
			return id, err
		}
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "entitlement consumed",
			slog.String("org_id", u.OrgID.String()), slog.String("feature", string(f)),
			slog.String("entitlement_id", e.ID.String()), slog.String("usage_id", u.ID.String()),
			slog.String("ref_type", string(ref.Type)), slog.Int("quantity", qty), slog.Int("remaining", e.Remaining()))
		return u.ID, nil
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "entitlement missing",
		slog.String("org_id", orgID.String()), slog.String("feature", string(f)), slog.String("ref_type", string(ref.Type)))
	return uuid.Nil(), ErrNoEntitlement
}

// consumed looks up an earlier consumption of ref: done=true with its
// usage ID, or an error when it belongs to another org or was released.
func (s *service) consumed(ctx context.Context, orgID uuid.UUID, ref Ref) (uuid.UUID, bool, error) {
	u, reversed, err := s.d.Store.UsageByRef(ctx, ref)
	switch {
	case errors.Is(err, ErrNotFound):
		return uuid.Nil(), false, nil
	case err != nil:
		return uuid.Nil(), false, err
	case u.OrgID != orgID:
		return uuid.Nil(), true, ErrRefConflict
	case reversed:
		return uuid.Nil(), true, ErrUsageReleased
	}
	return u.ID, true, nil
}

// Release implements Entitlements.
func (s *service) Release(ctx context.Context, ref Ref) error {
	if !ref.Type.Valid() || ref.ID == uuid.Nil() {
		return fmt.Errorf("entitlements: release: invalid ref %q/%s", ref.Type, ref.ID)
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		u, reversed, err := s.d.Store.UsageByRef(ctx, ref)
		switch {
		case errors.Is(err, ErrNotFound) || (err == nil && reversed):
			return nil // nothing consumed, or already released
		case err != nil:
			return err
		}
		// Lock the parent first: the reversal and the quota change are one
		// step for every concurrent caller.
		if _, err := s.d.Store.Lock(ctx, u.EntitlementID); err != nil {
			return err
		}
		now := s.d.Clock.Now()
		of := u.ID
		r, inserted, err := s.d.Store.InsertUsage(ctx, Usage{ID: uuid.NewV7(), ReversalOf: &of, ActorUserID: actorOf(ctx), CreatedAt: now})
		if err != nil || !inserted { // !inserted: a concurrent release won
			return err
		}
		if err := s.d.Store.Unconsume(ctx, r.EntitlementID, -r.Quantity, now); err != nil {
			return fmt.Errorf("entitlements: release %s: %w", u.ID, err)
		}
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "entitlement released",
			slog.String("org_id", r.OrgID.String()), slog.String("entitlement_id", r.EntitlementID.String()),
			slog.String("usage_id", u.ID.String()), slog.String("reversal_id", r.ID.String()),
			slog.String("ref_type", string(ref.Type)), slog.Int("quantity", -r.Quantity))
		return nil
	})
}

// Available implements Entitlements.
func (s *service) Available(ctx context.Context, orgID uuid.UUID, f Feature) (Availability, error) {
	if !f.Valid() {
		return Availability{}, fmt.Errorf("entitlements: available: unknown feature %q", f)
	}
	rows, err := s.d.Store.Usable(ctx, orgID, f, s.d.Clock.Now())
	if err != nil {
		return Availability{}, err
	}
	return summarize(f, rows), nil
}

// summarize folds the usable rows of one feature into its availability.
func summarize(f Feature, rows []Entitlement) Availability {
	a := Availability{Feature: f}
	for _, e := range rows {
		if f.Kind() == KindAccess {
			if a.AccessUntil == nil || e.ValidTo.After(*a.AccessUntil) {
				until := e.ValidTo
				a.AccessUntil = &until
			}
			continue
		}
		a.Remaining += e.Remaining()
	}
	return a
}
