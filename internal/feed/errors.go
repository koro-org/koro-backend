package feed

import "koro/internal/platform/problem"

// Errors of the feed module (05 §4.2; 04 §1.10, §2.7). The org status rules
// of 03 §2.3 reuse the orgs codes (orgs.ErrOrgSuspended,
// orgs.ErrOrgNotVerified, orgs.ErrInvalidState) and a medium that is not
// cleared by the scan is files.ErrNotClean: a problem code is registered
// once (06 H-52). Field errors (Tiptap body, shape of a kind) are
// problem.ErrValidation with the offending member's location.
var (
	// ErrInvalidState: a transition outside the state machine: publishing
	// a post that is not a draft, editing or deleting a hidden or removed
	// post.
	ErrInvalidState = problem.New(problem.Conflict, "feed.invalid_state", "Post status does not allow this change")
	// ErrPublishedImmutable: a published post keeps its kind, audience and
	// commercial label (04 §2.7 updatePost; 03 §5.7 feed.post.update).
	ErrPublishedImmutable = problem.New(problem.Conflict, "feed.published_immutable",
		"Kind, audience and commercial label of a published post cannot change")
	// ErrPostingSuspended: the org's feed trust level is suspended (02
	// §12.1, set by moderation): it cannot publish.
	ErrPostingSuspended = problem.New(problem.Forbidden, "feed.posting_suspended", "Organization cannot publish posts")
	// ErrEmptyPatch: updatePost without a field to change.
	ErrEmptyPatch = problem.New(problem.Invalid, "feed.empty_patch", "Nothing to update")

	// ErrNotFound: the post does not exist or is not visible to the caller
	// (03 §3.6).
	ErrNotFound = problem.ErrNotFound
	// ErrPreconditionRequired / ErrPreconditionFailed: If-Match missing or
	// stale on updatePost (04 §1.13).
	ErrPreconditionRequired = problem.ErrPreconditionRequired
	ErrPreconditionFailed   = problem.ErrPreconditionFailed
)
