package outreach

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/platform/problem"
	"koro/internal/platform/ratelimit"
)

// countBudgetRule is the name of the daily count budget of an org.
const countBudgetRule = "outreach_count_daily"

// ScopeOf returns a's targeting scope in orgID (03 k16): the role a holds
// in the org decides; a platform_admin without such a role acts with the
// platform's scope (k-po targeting, the ✓ cell of outreach.audience.count).
func ScopeOf(a authz.Actor, orgID uuid.UUID) Scope {
	if !a.IsUser() || orgID == uuid.Nil() {
		return ScopeNone
	}
	for _, m := range a.Memberships {
		if m.OrgID != orgID {
			continue
		}
		switch {
		case m.OrgKind == authz.OrgClub && m.Role == authz.MemberClubAdmin && !a.IsMinor: // 03 Y4
			return ScopeClub
		case m.OrgKind == authz.OrgUniversity && (m.Role == authz.MemberUniversityRep || m.Role == authz.MemberCareerStaff):
			return ScopeUniversity
		case m.OrgKind == authz.OrgCompany && (m.Role == authz.MemberRecruiter || m.Role == authz.MemberCompanyAdmin ||
			m.Role == authz.MemberCompanyOwner):
			return ScopeCompany
		}
	}
	if a.HasPlatformRole(authz.PlatformAdmin) {
		return ScopePlatform
	}
	return ScopeNone
}

// needsK reports whether a targeting in scope must reach at least
// KThreshold students: everyone's but a club's addressing its own members
// (02 §12.2, 03 k16).
func (sc Scope) needsK() bool { return sc != ScopeClub }

// checkScope applies 03 k16 and Y2 to a normalized definition of orgID.
func (s *service) checkScope(ctx context.Context, sc Scope, orgID uuid.UUID, def Definition) error {
	switch sc {
	case ScopeClub:
		// club:<own club> and nothing else: the club's members, sensitive
		// club or not (its admin already sees them).
		if len(def.Uni)+len(def.Dept)+len(def.Year)+len(def.Event) > 0 || len(def.Club) != 1 || def.Club[0] != orgID {
			return ErrTargetOutOfScope
		}
		return nil
	case ScopeUniversity:
		// uni:<own university>, narrowed by department or year; never a
		// club or an event.
		if len(def.Club)+len(def.Event) > 0 || len(def.Uni) != 1 {
			return ErrTargetOutOfScope
		}
		u, err := s.d.Catalog.GetUniversity(ctx, def.Uni[0])
		switch {
		case errors.Is(err, directory.ErrNotFound):
			return ErrTargetOutOfScope
		case err != nil:
			return err
		case u.OrgID != orgID:
			return ErrTargetOutOfScope
		}
		return nil
	case ScopeCompany, ScopePlatform:
		if err := s.checkClubs(ctx, def.Club); err != nil {
			return err
		}
		return s.checkEvents(ctx, def.Event)
	}
	return ErrTargetOutOfScope
}

// checkClubs refuses a sensitive club (03 Y2) of a company's or the
// platform's targeting.
func (s *service) checkClubs(ctx context.Context, clubs []uuid.UUID) error {
	if len(clubs) == 0 {
		return nil
	}
	sensitive, err := s.d.Catalog.SensitiveOrgIDs(ctx, clubs)
	if err != nil {
		return err
	}
	for _, id := range clubs {
		if sensitive[id] {
			return ErrSensitiveTarget
		}
	}
	return nil
}

// checkEvents refuses an event that is not targetable now (BE-161, Q-39;
// events' rule). Every reason is the same 422; without the events reader
// nothing is targetable (fail closed).
func (s *service) checkEvents(ctx context.Context, events []uuid.UUID) error {
	if len(events) == 0 {
		return nil
	}
	if s.d.Events == nil {
		return ErrEventNotTargetable
	}
	ok, err := s.d.Events.TargetableEvents(ctx, events)
	if err != nil {
		return err
	}
	for _, id := range events {
		if !ok[id] {
			return ErrEventNotTargetable
		}
	}
	return nil
}

// count sizes a normalized definition of orgID for an actor in scope sc:
// one unit of the org's daily budget, the family watch, then measure (with
// the value floor when sc needs k). A refused value is located in raw, the
// definition as the caller wrote it at loc. Only the published Estimate
// leaves this function; the exact size is neither returned nor logged.
func (s *service) count(ctx context.Context, orgID uuid.UUID, sc Scope, def Definition, loc string, raw Definition) (Estimate, error) {
	if err := s.spend(ctx, orgID); err != nil {
		return Estimate{}, err
	}
	s.watch(ctx, orgID, def)
	est, small, err := s.measure(ctx, def, sc.needsK())
	switch {
	case err != nil:
		return Estimate{}, err
	case small != nil:
		s.raise(ctx, AlarmValueFloor, orgID, slog.String("dimension", small.dim))
		return Estimate{}, small.at(loc, raw)
	}
	return est, nil
}

// spend takes one unit of the org's daily count budget. The budget bounds
// how many slightly different definitions an org can compare; an
// unavailable limiter fails closed (503), an exhausted budget is 429.
func (s *service) spend(ctx context.Context, orgID uuid.UUID) error {
	rule := ratelimit.Rule{
		Name: countBudgetRule, Rate: s.d.DailyCountLimit, Per: 24 * time.Hour, Key: ratelimit.KeyOrg, Strict: true,
	}
	res, err := s.d.Limiter.Allow(ctx, rule, "org:"+orgID.String())
	switch {
	case err != nil:
		s.d.Log.LogAttrs(ctx, slog.LevelError, "audience count budget unavailable (fail closed)",
			slog.String("org_id", orgID.String()), slog.String("err", err.Error()))
		return problem.ErrUnavailable.Wrap(err)
	case !res.Allowed:
		s.raise(ctx, AlarmBudget, orgID) // the daily budget is spent
		return ErrRateLimited
	}
	return nil
}

// Compile implements Targeting.
func (s *service) Compile(ctx context.Context, a authz.Actor, spec SegmentSpec) (Compiled, error) {
	if spec.OrgID == uuid.Nil() {
		return Compiled{}, ErrNotFound
	}
	if (spec.SegmentID == nil) == (spec.Definition == nil) {
		return Compiled{}, problem.Field("body.audience", "exactly one of segment_id and definition is required")
	}
	sc := ScopeOf(a, spec.OrgID)
	if sc == ScopeNone { // nothing of the org is read for an actor without a targeting role there
		return Compiled{}, ErrTargetOutOfScope
	}
	var (
		def, raw Definition
		loc      string
		segID    *uuid.UUID
		err      error
	)
	if spec.SegmentID != nil {
		seg, err := s.d.Store.Get(ctx, spec.OrgID, *spec.SegmentID)
		if err != nil {
			return Compiled{}, err
		}
		if seg.Archived() {
			return Compiled{}, ErrInvalidState
		}
		raw, loc = seg.Definition, "body.audience.segment_id"
		id := seg.ID
		segID = &id
	} else {
		raw, loc = *spec.Definition, "body.audience.definition"
	}
	if def, err = raw.normalize(loc); err != nil {
		return Compiled{}, err
	}
	if err := s.checkScope(ctx, sc, spec.OrgID, def); err != nil {
		return Compiled{}, err
	}
	est, err := s.count(ctx, spec.OrgID, sc, def, loc, raw)
	if err != nil {
		return Compiled{}, err
	}
	if est.BelowThreshold && sc.needsK() {
		return Compiled{}, ErrAudienceTooSmall
	}
	out := Compiled{Keys: def.Keys(), DimCount: def.DimCount(), Definition: def, SegmentID: segID}
	if est.Count != nil {
		out.Size = *est.Count
	}
	return out, nil
}

// CountAudience implements Service.
func (s *service) CountAudience(ctx context.Context, a authz.Actor, orgID uuid.UUID, def Definition) (CountResult, error) {
	if err := authz.Can(ctx, a, authz.OutreachAudienceCount, surface(orgID)); err != nil {
		return CountResult{}, err
	}
	if err := s.usableOrg(ctx, a, orgID); err != nil {
		return CountResult{}, err
	}
	norm, err := def.normalize("body.definition")
	if err != nil {
		return CountResult{}, err
	}
	sc := ScopeOf(a, orgID)
	if err := s.checkScope(ctx, sc, orgID, norm); err != nil {
		return CountResult{}, err
	}
	est, err := s.count(ctx, orgID, sc, norm, "body.definition", def)
	if err != nil {
		return CountResult{}, err
	}
	return CountResult{Estimate: est, Dimensions: norm.DimCount()}, nil
}
