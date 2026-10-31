// Package stats is the panel summaries of SR §8 (BE-163; uyum denetimi
// #70, #72–#74): the platform admin's operational counts and the club,
// company and university panels' figures. It owns no table and writes
// nothing: every figure comes from the module that owns its rows through a
// small read port (ports.go) that the composition root fills with the
// modules' Statistics reads (orgs, identity, feed, events, orders,
// outreach, messaging, entitlements, students, directory, catalog). The
// product KPIs of SR §6 are BE-164's (kvkk, analytics), not this module's.
//
// Rules the package enforces:
//
//   - Who (03 §5.23): the platform panel is platform_admin's (admin
//     surface: 403 for everyone else); an org panel is its own role's on
//     the org's surface (club_admin; recruiter and company_admin/owner;
//     university_rep and career_staff) plus platform_admin, members
//     without the role 403, everyone else 404. A sensitive club's panel is
//     its members' only (Y2: platform staff 404). The panel of another
//     org kind is 404 once the caller may read the org's panels at all.
//   - Read only, scoped by the path: the org comes from the path and is
//     read from orgs (H-38), never from a body; every provider read is
//     keyed by it (or by the university the directory row of that org
//     names).
//   - Small groups (SR §8 "10 kişinin altındaki gruplar gösterilmez"; 06
//     H-61): a number about people the panel's org does not already see
//     one by one is published only as an estimate (outreach.PublicEstimate:
//     null below 10, rounded down to its bucket): a university's own
//     students (students suppresses the groups below 10 itself; one
//     breakdown at a time, never a cross-tabulation), the participants of
//     its clubs' events, a company's audiences and dispatch reach. A
//     sensitive club and an event of a sensitive category never appear in
//     a university's figures (special category data, KVKK md. 6). The
//     figures an org sees one by one already — a club's own members and
//     registrations, a company's own contact requests, posts, likes and
//     package — are exact.
//   - Range: whole days in Europe/Istanbul, from and to inclusive, at most
//     366 days; the default is the last 30 days (platform, company) or
//     the last 12 calendar months (club, university: monthly growth). A
//     malformed range is 422, after the authorisation (404/403 first).
//   - Personal data: none in an answer, a log line or an audit record;
//     the panels are counts. Nothing is audited (aggregate reads, 03 §6.5
//     lists none of them).
package stats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/clock"
)

// Service is the stats service: the four panel reads. Every method calls
// authz.Can first (05 §5.3).
type Service interface {
	// Platform is getAdminStats (stats.platform.read).
	Platform(ctx context.Context, a authz.Actor, q RangeQuery) (PlatformStats, error)
	// Club is getClubStats (stats.club.read).
	Club(ctx context.Context, a authz.Actor, orgID uuid.UUID, q RangeQuery) (ClubStats, error)
	// Company is getCompanyStats (stats.company.read).
	Company(ctx context.Context, a authz.Actor, orgID uuid.UUID, q RangeQuery) (CompanyStats, error)
	// University is getUniversityStats (stats.university.read).
	University(ctx context.Context, a authz.Actor, orgID uuid.UUID, q RangeQuery) (UniversityStats, error)
}

// Deps are the dependencies of the stats module; all are required.
type Deps struct {
	Orgs         Orgs
	Clubs        Clubs
	Users        Users
	OrgCounts    OrgCounts
	Content      Content
	Events       Events
	Sales        Sales
	Outreach     Outreach
	Contacts     Contacts
	Packages     Packages
	Students     Students
	Universities Universities
	Offerings    Offerings
	Clock        clock.Clock
	Log          *slog.Logger
}

func (d Deps) validate() error {
	var errs []error
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Orgs", d.Orgs != nil}, {"Clubs", d.Clubs != nil}, {"Users", d.Users != nil}, {"OrgCounts", d.OrgCounts != nil},
		{"Content", d.Content != nil}, {"Events", d.Events != nil}, {"Sales", d.Sales != nil},
		{"Outreach", d.Outreach != nil}, {"Contacts", d.Contacts != nil}, {"Packages", d.Packages != nil},
		{"Students", d.Students != nil}, {"Universities", d.Universities != nil}, {"Offerings", d.Offerings != nil},
		{"Clock", d.Clock != nil}, {"Log", d.Log != nil},
	} {
		if !c.ok {
			errs = append(errs, fmt.Errorf("stats.Deps.%s is missing", c.name))
		}
	}
	return errors.Join(errs...)
}

// Module is what stats hands to the composition root.
type Module struct {
	Service Service
}

// New builds the stats module; a missing dependency is a wiring bug that
// stops the process at start.
func New(d Deps) (Module, error) {
	if err := d.validate(); err != nil {
		return Module{}, err
	}
	return Module{Service: &service{d: d}}, nil
}

type service struct {
	d Deps
}
