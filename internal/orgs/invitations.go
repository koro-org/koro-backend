package orgs

import (
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
	"koro/internal/platform/textx"
	"koro/internal/platform/token"
)

// Invitation tokens (02 §7.4): HMAC-SHA256 of the invitation ID under the
// (key_id, purpose) subkey of CRYPTO_TOKEN_KEYS, base32 without padding (52
// characters, case-insensitive when typed). The database keeps SHA-256 of
// the token only; the worker re-derives it for the e-mail.
const (
	invitationTokenPurpose = "orgs.invitation"
	invitationTokenLen     = 52
	invitationTTL          = 7 * 24 * time.Hour // 02 §7.4: created_at + 7 days
	invitationAggType      = "org_invitation"
	invitationTemplate     = "orgs.invitation"
	invitationLinkPath     = "/invitations/accept"
	maxEmail               = 254
)

var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// InvitationQuery is the page of listOrgInvitations.
type InvitationQuery struct {
	Limit int
	After *uuid.UUID
}

// InvitationPage is a page of open invitations.
type InvitationPage struct {
	Items []Invitation
	Next  *uuid.UUID
}

// deriveToken returns the token of an invitation.
func (s *service) deriveToken(keyID string, id uuid.UUID) (string, error) {
	if s.d.Deriver == nil {
		return "", problem.ErrUnavailable
	}
	b, err := s.d.Deriver.DerivedToken(invitationTokenPurpose, keyID, id[:])
	if err != nil {
		return "", fmt.Errorf("orgs: derive invitation token: %w", err)
	}
	return tokenEncoding.EncodeToString(b), nil
}

// invitationTokenHash normalises a token typed or pasted by a user (spaces
// removed, upper case) and returns its SHA-256; ok=false when it cannot be
// a token (it is then simply not found).
func invitationTokenHash(raw string) ([]byte, bool) {
	t := strings.ToUpper(strings.Join(strings.Fields(raw), ""))
	if len(t) != invitationTokenLen {
		return nil, false
	}
	if _, err := tokenEncoding.DecodeString(t); err != nil {
		return nil, false
	}
	return token.Hash(t), true
}

// invitationResource is the authz view of an invitation (03 §3.5:
// OrgID = the org; Attrs org_kind, role). owner is set by acceptance only:
// the invitation is the caller's once its address matched the account.
func invitationResource(orgID uuid.UUID, kind Kind, role authz.MemberRole, owner *uuid.UUID) authz.Resource {
	org := orgID
	return authz.Resource{Type: authz.TypeOrgInvitation, OrgID: &org, OwnerUserID: owner, Attrs: map[string]string{
		"org_kind": string(kind), "role": string(role),
	}}
}

// cleanEmail validates an invited address and returns it with its
// normalised form (users.email_normalized rules).
func cleanEmail(loc, s string) (string, string, error) {
	e := strings.TrimSpace(s)
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e || addr.Name != "" || len(e) > maxEmail || !strings.Contains(e, "@") {
		return "", "", fieldErr(loc, "must be a bare e-mail address")
	}
	return e, textx.NormalizeEmail(e), nil
}

func invitationAudit(a authz.Actor, act authz.Action, inv Invitation, meta map[string]any) audit.Event {
	org, id := inv.OrgID, inv.ID
	if meta == nil {
		meta = map[string]any{}
	}
	meta["invitation_id"] = inv.ID.String()
	meta["role"] = string(inv.Role)
	return audit.Event{
		Action: string(act), Outcome: audit.Success, OrgID: &org, ResourceType: authz.TypeOrgInvitation, ResourceID: &id,
		Metadata: meta,
	}.By(a)
}

// CreateInvitation implements Service. Order: the org (404) → authz
// (404/403) → the org status → the role (422 orgs.role_not_allowed: it must
// exist for the org's kind and never be company_owner; university roles
// are the platform admin's by the policy) → the address → in one
// transaction: an expired open invitation to the address is closed, an
// open one is 409 orgs.invitation_exists, an account of the address that
// already holds the role is 409 orgs.already_member, then the row (token
// hash only) and org_invitation.created (O). The address is never audited or
// logged.
func (s *service) CreateInvitation(ctx context.Context, a authz.Actor, orgID uuid.UUID, email string, role authz.MemberRole) (Invitation, error) {
	o, err := s.d.Store.GetOrg(ctx, orgID)
	if err != nil {
		return Invitation{}, err
	}
	if err := authz.Can(ctx, a, authz.OrgsInvitationCreate, invitationResource(o.ID, o.Kind, role, nil)); err != nil {
		return Invitation{}, err
	}
	if err := writable(a, o); err != nil {
		return Invitation{}, err
	}
	if !invitableRole(o.Kind, role) {
		return Invitation{}, ErrRoleNotAllowed
	}
	addr, norm, err := cleanEmail("body.email", email)
	if err != nil {
		return Invitation{}, err
	}
	if s.d.Deriver == nil {
		return Invitation{}, problem.ErrUnavailable
	}
	hash, err := idempotency.Hash(struct {
		Op, Email string
		Org       uuid.UUID
		Role      authz.MemberRole
	}{"invite", norm, o.ID, role})
	if err != nil {
		return Invitation{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Invitation, error) {
		if _, err := s.d.Store.LockOrg(ctx, o.ID); err != nil {
			return Invitation{}, err
		}
		now := s.now()
		switch cur, err := s.d.Store.LockOpenInvitationTo(ctx, o.ID, norm); {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return Invitation{}, err
		case !cur.Expired(now):
			return Invitation{}, ErrInvitationExists
		default: // expired but not closed by the hourly job yet
			if _, err := s.d.Store.CloseInvitation(ctx, o.ID, cur.ID, cur.ExpiresAt); err != nil {
				return Invitation{}, err
			}
		}
		if err := s.notMember(ctx, o.ID, addr, role); err != nil {
			return Invitation{}, err
		}
		inv := Invitation{
			ID: uuid.NewV7(), OrgID: o.ID, Email: addr, EmailNormalized: norm, Role: role,
			KeyID: s.d.Deriver.ActiveKeyID(), InvitedBy: a.UserID, ExpiresAt: now.Add(invitationTTL),
		}
		raw, err := s.deriveToken(inv.KeyID, inv.ID)
		if err != nil {
			return Invitation{}, err
		}
		inv.TokenHash = token.Hash(raw)
		inv, err = s.d.Store.InsertInvitation(ctx, inv)
		if err != nil {
			return Invitation{}, err
		}
		env, err := domainevent.New(domainevent.OrgInvitationCreated{InvitationID: inv.ID, OrgID: inv.OrgID}, invitationAggType, inv.ID)
		if err != nil {
			return Invitation{}, err
		}
		if err := s.d.Outbox.Add(ctx, env); err != nil {
			return Invitation{}, err
		}
		return inv, s.d.Audit.Log(ctx, invitationAudit(a, authz.OrgsInvitationCreate, inv, nil))
	})
	return out, err
}

// notMember is 409 orgs.already_member when the account of addr already
// holds role in orgID (an invitation to another role is a promotion or a
// change: acceptance replaces the membership).
func (s *service) notMember(ctx context.Context, orgID uuid.UUID, addr string, role authz.MemberRole) error {
	if s.d.Users == nil {
		return nil
	}
	uid, found, err := s.d.Users.FindByEmail(ctx, addr)
	if err != nil || !found {
		return err
	}
	switch cur, err := s.d.Store.LockOpenMember(ctx, orgID, uid); {
	case err == nil && cur.Status == MemberActive && cur.Role == role:
		return ErrAlreadyMember
	case err == nil, errors.Is(err, ErrNotFound):
		return nil
	default:
		return err
	}
}

// ListInvitations implements Service: open, unexpired invitations, newest
// first.
func (s *service) ListInvitations(ctx context.Context, a authz.Actor, orgID uuid.UUID, q InvitationQuery) (InvitationPage, error) {
	o, err := s.d.Store.GetOrg(ctx, orgID)
	if err != nil {
		return InvitationPage{}, err
	}
	if err := authz.Can(ctx, a, authz.OrgsInvitationList, orgResource(o)); err != nil {
		return InvitationPage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListOpenInvitations(ctx, o.ID, s.now(), q.After, limit+1)
	if err != nil {
		return InvitationPage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := InvitationPage{Items: rows}
	if more {
		last := rows[len(rows)-1].ID
		out.Next = &last
	}
	return out, nil
}

// RevokeInvitation implements Service: an open invitation of the org is
// revoked (another org's ID, or a closed one, is 404).
func (s *service) RevokeInvitation(ctx context.Context, a authz.Actor, orgID, id uuid.UUID) error {
	o, err := s.d.Store.GetOrg(ctx, orgID)
	if err != nil {
		return err
	}
	inv, err := s.d.Store.GetInvitation(ctx, o.ID, id)
	if err != nil {
		return err
	}
	if err := authz.Can(ctx, a, authz.OrgsInvitationRevoke, invitationResource(o.ID, o.Kind, inv.Role, nil)); err != nil {
		return err
	}
	if err := writable(a, o); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		closed, err := s.d.Store.CloseInvitation(ctx, o.ID, inv.ID, s.now())
		if err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, invitationAudit(a, authz.OrgsInvitationRevoke, closed, nil))
	})
}

// AcceptInvitation implements Service. Order: the token (malformed or
// unknown → 404) → the address: the caller's account e-mail must equal the
// invited one (otherwise 404: the invitation is not the caller's; decision
// recorded in the ticket) → authz (O, the caller's own invitation) → a used
// or revoked invitation is 404 (single use), an expired one 409
// orgs.invitation_expired → the org (closed 404, suspended 403) → k5 for
// club_admin (422) → the membership: a pending request or an active
// membership with another role ends (role_changed) and the invited role
// starts; the same role is 409 orgs.already_member.
func (s *service) AcceptInvitation(ctx context.Context, a authz.Actor, raw string) (Membership, error) {
	h, ok := invitationTokenHash(raw)
	if !ok || s.d.Users == nil {
		return Membership{}, ErrNotFound
	}
	inv, err := s.d.Store.LockInvitationByTokenHash(ctx, h)
	if err != nil {
		return Membership{}, err
	}
	if !token.Equal(h, inv.TokenHash) {
		return Membership{}, ErrNotFound
	}
	c, err := s.d.Users.Contact(ctx, a.UserID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return Membership{}, ErrNotFound
	case err != nil:
		return Membership{}, err
	}
	if textx.NormalizeEmail(c.Email) != inv.EmailNormalized {
		return Membership{}, ErrNotFound
	}
	me := a.UserID
	if err := authz.Can(ctx, a, authz.OrgsInvitationAccept, invitationResource(inv.OrgID, inv.OrgKind, inv.Role, &me)); err != nil {
		return Membership{}, err
	}
	hash, err := idempotency.Hash(struct {
		Op string
		ID uuid.UUID
	}{"accept", inv.ID})
	if err != nil {
		return Membership{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Membership, error) {
		inv, err := s.d.Store.LockInvitationByTokenHash(ctx, h)
		if err != nil {
			return Membership{}, err
		}
		now := s.now()
		switch {
		case !inv.Open():
			return Membership{}, ErrNotFound
		case inv.Expired(now):
			return Membership{}, ErrInvitationExpired
		}
		o, err := s.d.Store.LockOrg(ctx, inv.OrgID)
		if err != nil {
			return Membership{}, err
		}
		switch o.Status {
		case StatusClosed:
			return Membership{}, ErrNotFound
		case StatusSuspended:
			return Membership{}, ErrOrgSuspended
		}
		if inv.Role == authz.MemberClubAdmin && !actorAdultStudent(a) {
			return Membership{}, ErrClubAdminNeedsAdultStudent
		}
		sens, err := s.sensitive(ctx, o.ID, o.Kind)
		if err != nil {
			return Membership{}, err
		}
		switch cur, err := s.d.Store.LockOpenMember(ctx, o.ID, a.UserID); {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return Membership{}, err
		case cur.Status == MemberActive && cur.Role == inv.Role:
			return Membership{}, ErrAlreadyMember
		default:
			if err := s.keepsAdmin(ctx, cur); err != nil {
				return Membership{}, err
			}
			if _, err := s.endMember(ctx, cur, EndRoleChanged, sens, now); err != nil {
				return Membership{}, err
			}
		}
		by := inv.InvitedBy
		m, err := s.d.Store.InsertMember(ctx, Membership{
			ID: uuid.NewV7(), OrgID: o.ID, UserID: a.UserID, Role: inv.Role, Status: MemberActive, Source: SourceInvitation,
			JoinedAt: &now, InvitedBy: &by,
		})
		if err != nil {
			return Membership{}, err
		}
		if _, err := s.d.Store.AcceptInvitation(ctx, o.ID, inv.ID, a.UserID, now); err != nil {
			return Membership{}, err
		}
		if err := s.d.Audit.Log(ctx, memberAudit(a, authz.OrgsInvitationAccept, m, sens, "",
			map[string]any{"invitation_id": inv.ID.String()})); err != nil {
			return Membership{}, err
		}
		s.emitStarted(ctx, m)
		if mfaRole(m.Role) {
			s.revokeAfterCommit(ctx, a.UserID)
		}
		return m, nil
	})
	return out, err
}

// handleInvitationCreated is orgs.send_invitation_email
// (org_invitation.created, O). It is idempotent: notify deduplicates on the
// invitation ID, and an accepted, revoked, expired or deleted invitation is
// skipped. The token is re-derived and checked against the stored hash; it
// never leaves the worker except in the e-mail.
func (s *service) handleInvitationCreated(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.OrgInvitationCreated](e)
	if err != nil {
		return queue.Permanent(err)
	}
	inv, err := s.d.Store.GetInvitation(ctx, p.OrgID, p.InvitationID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	if !inv.Open() || inv.Expired(s.now()) {
		return nil
	}
	if s.d.Mailer == nil || s.d.Deriver == nil {
		return problem.ErrUnavailable // retried once the worker is configured
	}
	raw, err := s.deriveToken(inv.KeyID, inv.ID)
	if err != nil {
		return queue.Permanent(err) // the key left CRYPTO_TOKEN_KEYS
	}
	if !token.Verify(raw, inv.TokenHash) {
		return queue.Permanent(errors.New("orgs: derived invitation token does not match the stored hash (token keys changed)"))
	}
	o, err := s.d.Store.GetOrg(ctx, inv.OrgID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	if o.Status == StatusClosed {
		return nil
	}
	msg := notify.Message{
		ToEmail: inv.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: invitationTemplate, Locale: notify.DefaultLocale,
		Data: map[string]any{
			"org_name":   o.Name,
			"role":       string(inv.Role),
			"link":       s.d.LinkBaseURL + invitationLinkPath + "#token=" + raw,
			"token":      raw,
			"valid_days": "7",
		},
		DedupeKey: "orgs.invitation:" + inv.ID.String(),
	}
	if s.d.Users != nil {
		if uid, found, err := s.d.Users.FindByEmail(ctx, inv.Email); err == nil && found {
			if c, err := s.d.Users.Contact(ctx, uid); err == nil {
				msg.UserID, msg.Locale = &uid, c.Locale
			}
		}
	}
	if _, err := s.d.Mailer.Send(ctx, msg); err != nil {
		if notify.IsPermanent(err) {
			return queue.Permanent(err)
		}
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "invitation e-mail sent", slog.String("invitation_id", inv.ID.String()),
		slog.String("org_id", inv.OrgID.String()))
	return nil
}
