package identity

import (
	"context"
	"uuid"

	"koro/internal/authz"
)

// roleStore is the platform_role_assignments rows (02 §5.7): the roles in
// effect of the Actor, the grant, approval and revocation flow with its
// four-eyes rule, the bootstrap (bootstrap.go) and the admin alerts.
type roleStore interface {
	// PlatformRoles returns the roles IN EFFECT of userID, sorted: rows with
	// status active or revoke_pending only (06 H-33). A pending request, a
	// rejected one and a revoked role grant nothing.
	PlatformRoles(ctx context.Context, userID uuid.UUID) ([]authz.PlatformRole, error)
	// RoleAssignmentsOf returns every assignment row of userID, oldest
	// first (KVKK export, bootstrap).
	RoleAssignmentsOf(ctx context.Context, userID uuid.UUID) ([]RoleAssignment, error)
	// UsersWithRole returns the users holding role in effect, sorted.
	UsersWithRole(ctx context.Context, role authz.PlatformRole) ([]uuid.UUID, error)
	// InsertRoleAssignment writes an assignment row (bootstrap, grant
	// requests and direct grants). The 02 §5.7 CHECKs and the open-row
	// unique index apply: a second open row of (user, role) is
	// ErrRoleAssignmentOpen, approved_by = requested_by ErrFourEyes, a
	// self-assignment ErrForbidden.
	InsertRoleAssignment(ctx context.Context, a RoleAssignment) error
	// RoleAssignment returns an assignment row or ErrNotFound.
	RoleAssignment(ctx context.Context, id uuid.UUID) (RoleAssignment, error)
	// LockRoleAssignment returns an assignment row locked for the
	// transaction, or ErrNotFound.
	LockRoleAssignment(ctx context.Context, id uuid.UUID) (RoleAssignment, error)
	// UpdateRoleAssignment writes the state columns of a: status,
	// approved_by/at, granted_at, rejected_by/at, revoke_reason,
	// revoke_requested_by/at, revoked_by/at. The CHECKs map as on insert
	// (the DB is the second layer of the four-eyes rule, 03 §6.1).
	UpdateRoleAssignment(ctx context.Context, a RoleAssignment) error
	// RoleAssignments returns up to q.Limit assignments newest first (id
	// DESC) after q.After; open ones only unless q.IncludeEnded.
	RoleAssignments(ctx context.Context, q RoleQuery) ([]RoleAssignment, error)
	// CountRoleAssignments returns how many users hold role in effect and
	// how many requests of it are pending (k3 talent_auditor limit, last
	// platform_admin; 06 H-33 condition for "in effect").
	CountRoleAssignments(ctx context.Context, role authz.PlatformRole) (inEffect, pending, revokePending int, err error)
}
