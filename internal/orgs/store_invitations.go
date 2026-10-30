package orgs

import (
	"context"
	"time"
	"uuid"
)

// invitationStore is the org_invitations rows (02 §7.4; invitations.go).
type invitationStore interface {
	// InsertInvitation writes an invitation of inv.OrgID and returns it as
	// stored; org_kind is the parent org's kind (06 H-38). ErrNotFound when
	// the org does not exist; ErrInvitationExists when an open invitation to
	// the same address exists (org_invitations_open_uq).
	InsertInvitation(ctx context.Context, inv Invitation) (Invitation, error)
	// GetInvitation returns invitation id of orgID or ErrNotFound.
	GetInvitation(ctx context.Context, orgID, id uuid.UUID) (Invitation, error)
	// LockInvitationByTokenHash returns the invitation of a token hash,
	// locked, or ErrNotFound (the token is the credential: any org).
	LockInvitationByTokenHash(ctx context.Context, hash []byte) (Invitation, error)
	// LockOpenInvitationTo returns the open (neither accepted nor revoked)
	// invitation of orgID to emailNormalized, locked, or ErrNotFound.
	LockOpenInvitationTo(ctx context.Context, orgID uuid.UUID, emailNormalized string) (Invitation, error)
	// CloseInvitation sets revoked_at = at on an open invitation of orgID and
	// returns it; ErrNotFound when it is not open.
	CloseInvitation(ctx context.Context, orgID, id uuid.UUID, at time.Time) (Invitation, error)
	// AcceptInvitation marks an open invitation of orgID accepted by userID
	// and returns it; ErrNotFound when it is not open.
	AcceptInvitation(ctx context.Context, orgID, id, userID uuid.UUID, now time.Time) (Invitation, error)
	// ListOpenInvitations returns up to limit open, unexpired invitations of
	// orgID, newest first, after the invitation ID after (nil: first page).
	ListOpenInvitations(ctx context.Context, orgID uuid.UUID, now time.Time, after *uuid.UUID, limit int) ([]Invitation, error)
}
