// Package seed builds the synthetic Turkish demo data of `koro seed demo`
// (BE-033; 05 §1.2 db/seed, §13.7). It runs in dev and staging only (the
// command refuses prod) and never holds real personal data:
//
//   - names come from internal/platform/testutil/fake (frequent given names
//     and surnames, invented clubs and companies), e-mail addresses are
//     @example.test only (fake.EmailDomain), there are no phone numbers,
//     national IDs or IBANs, and nobody's verification uses a real .edu.tr
//     address (the demo students are verified by document);
//   - the universities, faculties and departments are the public reference
//     data of `koro seed reference` (run it first), read from the database.
//
// Build turns the reference data into a Plan of rows; Apply inserts them
// with `ON CONFLICT DO NOTHING` in one transaction. Every row has a
// deterministic ID (demoID: the same key gives the same ID on every run),
// so a second run inserts nothing and a demo user's later changes are never
// overwritten. Randomness is math/rand/v2 PCG with a fixed seed: the same
// seed and reference data give the same plan.
//
// The data goes straight into the tables: no outbox row, so no e-mail and
// no notification is ever sent for it (the demo addresses do not exist),
// and no audit record (seeding is not a business action). Derived columns
// are written consistently: audience keys and eligibility (students' rule,
// 02 §9.1), confirmed_count (confirmed + checked_in), like_count, segment
// estimates rounded like outreach.Bucket, ticket tokens derived with the
// configured CRYPTO_TOKEN_KEYS (so the QR of a seeded ticket checks in).
package seed

import (
	"errors"
	"fmt"
	"time"
	"uuid"

	"koro/internal/events"
)

// DefaultSeed is the random seed of the demo plan.
const DefaultSeed uint64 = 20261005

// Well-known demo accounts (all @example.test; the password is the one
// given to `koro seed demo -password`, none without it).
const (
	AdminEmail1    = "demo.yonetici1@example.test" // platform_admin (bootstrap)
	AdminEmail2    = "demo.yonetici2@example.test" // platform_admin (bootstrap)
	FinanceEmail1  = "demo.finans1@example.test"   // finance_admin (four eyes: two of them)
	FinanceEmail2  = "demo.finans2@example.test"   // finance_admin
	DPOEmail       = "demo.kvkk@example.test"      // dpo
	ModeratorEmail = "demo.moderator@example.test" // moderator
	SupportEmail   = "demo.destek@example.test"    // support
	RepEmail       = "demo.temsilci@example.test"  // university_rep of the main university
	ClubAdminEmail = "demo.kulup@example.test"     // club_admin of the main club (a student)
	StudentEmail   = "demo.ogrenci@example.test"   // an adult verified student of the main university
	MinorEmail     = "demo.genc@example.test"      // a verified student under 18
	CompanyEmail   = "demo.sirket@example.test"    // company_owner of the main company
)

// Department is a department of a university (reference data).
type Department struct {
	ID            uuid.UUID
	Name          string
	DegreeLevel   string
	DurationYears int
}

// University is a university of the reference data with its org.
type University struct {
	ID, OrgID                      uuid.UUID
	YokCode, Name, ShortName, City string
	Departments                    []Department
}

// Reference is what the plan builds on: the platform org and at least
// three universities (ordered by YÖK code).
type Reference struct {
	PlatformOrgID uuid.UUID
	Universities  []University
}

// Options tunes a plan.
type Options struct {
	// Now anchors every date (events, posts, verifications).
	Now time.Time
	// Seed is the random seed; 0 means DefaultSeed.
	Seed uint64
	// PasswordHash is the argon2id hash of every demo account's password;
	// "" leaves the accounts without one (a password reset sets it).
	PasswordHash string
	// Tickets derives the ticket tokens (CRYPTO_TOKEN_KEYS).
	Tickets events.TicketKeys
}

// Demo names the rows a demo or a test starts from.
type Demo struct {
	MainUniversity      University
	MainClubOrgID       uuid.UUID // open club of the main university, run by ClubAdminEmail
	CompanyOrgID        uuid.UUID // active company with the targeted_post grant, owned by CompanyEmail
	EntitlementID       uuid.UUID // CompanyOrgID's targeted_post grant
	SegmentID           uuid.UUID // CompanyOrgID's segment of the main university
	PendingCompanyOrgID uuid.UUID // a company in pending_review
	SensitiveClubOrgIDs []uuid.UUID
}

// Account is a well-known demo account and what it is for.
type Account struct {
	Email, Role string
}

// Plan is the demo data: rows in insertion order, statements to run after
// them, and the well-known rows.
type Plan struct {
	Rows     []Row
	After    []Stmt
	Demo     Demo
	Accounts []Account
}

// StudentCount is the number of demo students (BE-033: at least 300).
const StudentCount = 300

// Minimum sizes of the demo (BE-033).
const (
	minUniversities = 3
	minorEvery      = 25 // every 25th student is under 18 (12 of 300)
	clubsPerUni     = 8
)

// ErrNoReference is returned when the reference data is missing.
var ErrNoReference = errors.New("seed: the reference data is missing: run `koro seed reference` first")

// Build returns the demo plan over ref.
func Build(ref Reference, o Options) (*Plan, error) {
	switch {
	case len(ref.Universities) < minUniversities:
		return nil, fmt.Errorf("%w (need %d universities, found %d)", ErrNoReference, minUniversities, len(ref.Universities))
	case ref.PlatformOrgID == uuid.Nil():
		return nil, fmt.Errorf("%w (no platform org)", ErrNoReference)
	case o.Tickets == nil:
		return nil, errors.New("seed: Options.Tickets is required (CRYPTO_TOKEN_KEYS)")
	case o.Now.IsZero():
		return nil, errors.New("seed: Options.Now is required")
	}
	if o.Seed == 0 {
		o.Seed = DefaultSeed
	}
	b := newBuilder(ref, o)
	if err := b.build(); err != nil {
		return nil, err
	}
	return b.p, nil
}
