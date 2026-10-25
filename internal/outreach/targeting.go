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
	"koro/internal/students"
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
		if len(def.Uni)+len(def.Dept)+len(def.Year) > 0 || len(def.Club) != 1 || def.Club[0] != orgID {
			return ErrTargetOutOfScope
		}
		return nil
	case ScopeUniversity:
		// uni:<own university>, narrowed by department or year; never a club.
		if len(def.Club) > 0 || len(def.Uni) != 1 {
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
		if len(def.Club) == 0 {
			return nil
		}
		sensitive, err := s.d.Catalog.SensitiveOrgIDs(ctx, def.Club)
		if err != nil {
			return err
		}
		for _, id := range def.Club {
			if sensitive[id] {
				return ErrSensitiveTarget
			}
		}
		return nil
	}
	return ErrTargetOutOfScope
}

// count sizes a normalized definition of orgID: one unit of the org's
// daily budget, then students' count_segment. Only the published Estimate
// leaves this function; the exact size is neither returned nor logged.
func (s *service) count(ctx context.Context, orgID uuid.UUID, def Definition) (Estimate, error) {
	if err := s.spend(ctx, orgID); err != nil {
		return Estimate{}, err
	}
	return s.size(ctx, def)
}

// size is students' count_segment of def as a published Estimate.
func (s *service) size(ctx context.Context, def Definition) (Estimate, error) {
	n, err := s.d.Audience.Count(ctx, students.AudienceSpec{Keys: def.Keys()})
	switch {
	case errors.Is(err, students.ErrAudienceTooSmall):
		return Estimate{BelowThreshold: true}, nil
	case err != nil:
		return Estimate{}, err
	}
	return PublicEstimate(n), nil
}

// spend takes one unit of the org's daily count budget. The budget bounds
// difference attacks over many slightly different definitions; an
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
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "audience count budget exhausted", slog.String("org_id", orgID.String()))
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
		def   Definition
		segID *uuid.UUID
		err   error
	)
	if spec.SegmentID != nil {
		seg, err := s.d.Store.Get(ctx, spec.OrgID, *spec.SegmentID)
		if err != nil {
			return Compiled{}, err
		}
		if seg.Archived() {
			return Compiled{}, ErrInvalidState
		}
		if def, err = seg.Definition.normalize("body.audience.segment_id"); err != nil {
			return Compiled{}, err
		}
		id := seg.ID
		segID = &id
	} else if def, err = spec.Definition.normalize("body.audience.definition"); err != nil {
		return Compiled{}, err
	}
	if err := s.checkScope(ctx, sc, spec.OrgID, def); err != nil {
		return Compiled{}, err
	}
	est, err := s.count(ctx, spec.OrgID, def)
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
	def, err := def.normalize("body.definition")
	if err != nil {
		return CountResult{}, err
	}
	if err := s.checkScope(ctx, ScopeOf(a, orgID), orgID, def); err != nil {
		return CountResult{}, err
	}
	est, err := s.count(ctx, orgID, def)
	if err != nil {
		return CountResult{}, err
	}
	return CountResult{Estimate: est, Dimensions: def.DimCount()}, nil
}
