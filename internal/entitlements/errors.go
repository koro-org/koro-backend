package entitlements

import "koro/internal/platform/problem"

// Errors of the entitlements module (05 §4.2; 04 §1.10).
var (
	// ErrNoEntitlement: the org has no usable right (no active entitlement
	// of the feature inside its validity window, or no unit left). 403, not
	// 402/409/422: the client offers a package on this code (03 §3.3, T-58).
	ErrNoEntitlement = problem.New(problem.Forbidden, "entitlements.not_entitled", "No entitlement for this feature")

	// ErrInvalidGrant: a grant breaks the shape rules (kind ↔ feature,
	// quantity, validity window, source).
	ErrInvalidGrant = problem.New(problem.Invalid, "entitlements.invalid_grant", "Entitlement grant is invalid")
	// ErrInvalidState: the entitlement's status does not allow the change
	// (e.g. revoking an expired right).
	ErrInvalidState = problem.New(problem.Conflict, "entitlements.invalid_state", "Entitlement status does not allow this change")
	// ErrUsageReleased: a consumption whose Ref was already released is
	// repeated. The ref cannot consume again (entitlement_usages_ref_uq);
	// the caller uses a new Ref.
	ErrUsageReleased = problem.New(problem.Conflict, "entitlements.usage_released", "Consumption of this reference was released")
	// ErrRefConflict: the Ref was consumed by another org or for another
	// feature (a caller bug: refs are the caller's own record IDs).
	ErrRefConflict = problem.New(problem.Conflict, "entitlements.ref_conflict", "Reference was consumed elsewhere")

	// ErrNotFound: the entitlement (or its org) does not exist or is not
	// visible.
	ErrNotFound = problem.ErrNotFound
)
