package orgs

import (
	"context"
	"uuid"
)

// orgStore is the organizations rows (02 §7.1): the org root, addressed by
// its own id or slug, and the public directory and admin list over every
// org (read.go, manage.go, admin.go, company.go, the review workers).
type orgStore interface {
	// InsertOrg writes a new org and returns it as stored (created_at and
	// updated_at from the database). It reports false and writes nothing
	// when the slug is taken (ON CONFLICT DO NOTHING: the transaction stays
	// usable, so the caller can try another slug).
	InsertOrg(ctx context.Context, o Org) (Org, bool, error)
	// GetOrg returns the org or ErrNotFound.
	GetOrg(ctx context.Context, id uuid.UUID) (Org, error)
	// GetOrgBySlug returns the org of a slug or ErrNotFound.
	GetOrgBySlug(ctx context.Context, slug string) (Org, error)
	// GetOrgs returns the orgs of ids that exist (any order).
	GetOrgs(ctx context.Context, ids []uuid.UUID) ([]Org, error)
	// LockOrg returns the org locked for the rest of the transaction
	// (SELECT … FOR UPDATE) or ErrNotFound.
	LockOrg(ctx context.Context, id uuid.UUID) (Org, error)
	// UpdateOrg writes the mutable columns of o (name, status, description,
	// logo, website, city, verified_*, closed_at, rejection_reason) and
	// returns the stored row:
	// updated_at changes on every call, so the ETag does too (04 §1.13).
	// ErrNotFound when the org is gone.
	UpdateOrg(ctx context.Context, o Org) (Org, error)
	// ListOrgs returns up to limit orgs matching f after the position
	// after (nil: the first page), in f.Sort order.
	ListOrgs(ctx context.Context, f ListFilter, after *ListAfter, limit int) ([]Org, error)
}
