package talent

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/students"
)

// Students is what talent reads of the students module (01 §4.11:
// talent → students; students never knows talent). students.Service
// satisfies it. Both reads run on app_rw, before a talent transaction
// opens.
type Students interface {
	// Cohorts returns the cohort (with the minor flag) of the students
	// among userIDs; a user without a student profile is missing.
	Cohorts(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]students.Cohort, error)
	// RankingChoice returns the activity ranking choice of userID
	// (default without a settings row).
	RankingChoice(ctx context.Context, userID uuid.UUID) (students.RankingChoice, error)
}

// Directory reports sensitive clubs (01 §4.11: club membership signals
// leave sensitive clubs out). directory.Service satisfies it.
type Directory interface {
	SensitiveOrgIDs(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

// AuditLogger writes audit records; audit.Service satisfies it.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx (inside
	// a talent transaction: within Store.AsApp, on the session role).
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}
