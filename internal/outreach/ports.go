package outreach

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/directory"
	"koro/internal/orgs"
	"koro/internal/platform/ratelimit"
	"koro/internal/students"
)

// AuditLogger is what outreach needs from audit (L1): every segment write
// is logged in the business transaction.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Audience is what outreach needs from students (L7; 01 §4.9 Audience):
// the size of a targeting through count_segment. It counts only audience
// eligible students (an active verification, an adult, an active account
// and profile) and answers students.ErrAudienceTooSmall below 10 without a
// number. It is students.Service.
type Audience interface {
	// Count returns the number of eligible students spec targets or
	// students.ErrAudienceTooSmall.
	Count(ctx context.Context, spec students.AudienceSpec) (int, error)
}

// Catalog is what outreach needs from directory (L6; 01 §4.8 Catalog): the
// sensitive club flag (03 Y2) and a university's org (the university_rep's
// own university, 03 k16). It is directory.Service.
type Catalog interface {
	// SensitiveOrgIDs reports which of orgIDs are sensitive clubs.
	SensitiveOrgIDs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error)
	// GetUniversity returns a university or directory.ErrNotFound.
	GetUniversity(ctx context.Context, id uuid.UUID) (directory.University, error)
}

// Orgs is what outreach needs from orgs (L5; 01 §4.7 OrgDirectory): the
// org's existence and status (03 §2.3). It is orgs.Service.
type Orgs interface {
	// Get returns the summary of an org (any status) or orgs.ErrNotFound.
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
}

// Limiter counts the audience counts of an org (the daily budget,
// Targeting). It is the platform's rate limiter (Redis in production).
type Limiter interface {
	Allow(ctx context.Context, rule ratelimit.Rule, key string) (ratelimit.Result, error)
}
