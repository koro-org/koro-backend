// Package discovery is the level 2 anonymous discovery of companies (01
// §4.16, 03 §5.10, 04 §2.10, §V.2; BE-051): a company with a running
// discovery_access right filters the discoverable students (university,
// department, class year, degree level, skill, language; no club filter of
// any kind) and reads their anonymous profiles under a company-scoped
// pseudonym. It owns no table: the students come from students (level 2
// contract: Discoverable and DiscoveryIndex), the bands from talent.
//
// Rules the package enforces:
//
//   - Who (03 §5.10, k20): only the recruiter (cmm) and the company_admin
//     or owner (cma, MFA) of the company, on its own org surface. Members
//     of the org without one of these roles and every platform role get
//     403, everyone else 404 (03 §7.6). Then the company must be active
//     (orgs.org_suspended / orgs.org_not_verified) and hold a running
//     discovery_access right (403 entitlements.not_entitled,
//     entitlements.Available).
//   - Pseudonyms (01 §4.16): anon_id = base32(HMAC(k_discovery,
//     company_org_id ‖ user_id))[:16], k_discovery being the
//     "discovery.anon_id" subkey of the token keyring (CRYPTO_TOKEN_KEYS,
//     crypto.Deriver). A user ID never leaves the module: not in an
//     answer, a cursor, a log line or the org-visible part of an audit
//     record. An anon_id resolves only for the company it was derived for
//     and only to a student who is discoverable now: another company's
//     pseudonym, a malformed one and a student who left level 2 are the
//     same 404.
//   - k ≥ 10 (T-68): a filter that matches fewer than 10 discoverable
//     students in total answers an empty list with below_threshold; a
//     filter never singles a person out.
//   - Ranking (01 §4.16, ADR-0015): relevance × band multiplier, ties broken
//     by a seeded hash that is stable for the session (the seed is derived
//     from the session and the company, never sent). Relevance is the share
//     of the requested skills and languages a student holds (the other
//     filters are exact); the band (talent.Ranking) only lifts: neutral
//     ×1.00, bands 1–3 ×1.05/×1.10/×1.15. Talent off → every band neutral →
//     relevance and the seeded order only. The answer has no score, band,
//     rank or position field: the order is the only trace (01 §0-7, 03 Y1).
//   - Paging (04 §1.6): the ranking is recomputed for every page and the
//     opaque cursor carries the position, the filters' and the session
//     seed's fingerprint; another session or other filters → 422
//     page.invalid_cursor. A keyset over the ranking would put the score in
//     the cursor.
//   - Every search and every profile read is audited in its own
//     transaction (discovery.profile.search / .read; org = the company); an
//     unwritten audit record answers nothing. The search record holds the
//     filters and how many profiles were shown, never a total (k ≥ 10); the
//     read record names the student only in the metadata (dpo/admin view).
//   - Personal data: logs carry org IDs and counts only.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/students"
)

// Query is searchDiscovery (04 §2.10): OR inside a filter, AND across
// filters; Cursor and Limit page the ranking.
type Query struct {
	UniversityIDs []uuid.UUID
	DepartmentIDs []uuid.UUID
	ClassYears    []int16
	DegreeLevel   students.DegreeLevel // "" = any
	SkillIDs      []uuid.UUID
	LanguageCodes []string
	Cursor        string
	Limit         int // 1–100, default 25 (page.ClampLimit)

	// invalid is a malformed parameter the handler found; the service
	// answers it after the authorisation (404/403 before 422).
	invalid error
}

// Page is one page of searchDiscovery: level 2 profiles under the
// company's pseudonyms in ranking order.
type Page struct {
	Items          []students.AnonProfileDTO
	NextCursor     string
	BelowThreshold bool
}

// Service is the discovery service: the two HTTP use cases plus the
// pseudonym contract of messaging (01 §5.3: hooks.anonResolver).
type Service interface {
	// Search is searchDiscovery (discovery.profile.search).
	Search(ctx context.Context, a authz.Actor, companyOrgID uuid.UUID, q Query) (Page, error)
	// Get is getDiscoveryProfile (discovery.profile.read).
	Get(ctx context.Context, a authz.Actor, companyOrgID uuid.UUID, anonID string) (students.AnonProfileDTO, error)
	// Resolve maps a pseudonym of companyOrgID to the student it was
	// derived for when that student is discoverable now; found=false
	// otherwise (hooks.anonResolver; messaging authorises its own caller).
	Resolve(ctx context.Context, companyOrgID uuid.UUID, anonID string) (userID uuid.UUID, found bool, err error)
	// AnonID returns the pseudonym of userID for companyOrgID (messaging
	// shows a pending contact request under it, 04 §2.9).
	AnonID(companyOrgID, userID uuid.UUID) (string, error)
}

// Deps are the dependencies of the discovery module; all are required.
type Deps struct {
	Students     Students
	Ranking      Ranking
	Entitlements Entitlements
	Orgs         Orgs
	Audit        AuditLogger
	Tx           db.TxManager
	Pseudonyms   *Pseudonyms
	Log          *slog.Logger
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Students", d.Students != nil}, {"Ranking", d.Ranking != nil}, {"Entitlements", d.Entitlements != nil},
		{"Orgs", d.Orgs != nil}, {"Audit", d.Audit != nil}, {"Tx", d.Tx != nil},
		{"Pseudonyms", d.Pseudonyms != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("discovery.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what discovery hands to the composition root.
type Module struct {
	Service Service
}

// New builds the discovery module; a missing dependency is a wiring bug
// that stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	return Module{Service: &service{d: d}}, nil
}

type service struct {
	d Deps
}

var _ Service = (*service)(nil)
