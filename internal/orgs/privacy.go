package orgs

import (
	"context"
	"errors"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/privacy"
	"koro/internal/platform/textx"
)

// retainedReason explains why Erase keeps the membership rows it ends.
const retainedReason = "ended membership rows are kept for the dispute window of retention policy membership_ended " +
	"(6 months, purged by orgs.ended_memberships_purge, BE-057); sensitive club memberships are deleted at once; " +
	"organizations and companies hold no personal data (02 §7.1-7.2: created_by/verified_by reference the account, " +
	"which identity anonymises)"

// eraseJob is the system actor of the erasure's audit records.
const eraseJob = "orgs.erase_account"

// privacyProvider is the KVKK provider of orgs (01 §3.3). The org and
// company rows are not personal data (02 §7.1-7.2; the company's tax
// number and MERSİS live in parties, the authorised person's details are
// the user's own account in identity). What orgs knows about a person is
// their memberships (02 §7.3), the invitations addressed to them or
// accepted by them (02 §7.4) and the orgs they registered.
//
// Export returns all three to the data subject. Erase ends every open
// membership (account_erased; a sensitive club's row is deleted at once)
// and deletes the invitations; the ended rows stay until the membership
// retention purges them (retainedReason). kvkk calls the providers before
// identity anonymises the account, so the account's address is still
// readable here.
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "orgs" }

// email returns the normalised address of the account ("" when identity is
// not wired or the account is gone).
func (p privacyProvider) email(ctx context.Context, userID uuid.UUID) (string, error) {
	if p.s.d.Users == nil {
		return "", nil
	}
	c, err := p.s.d.Users.Contact(ctx, userID)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return "", nil
	case err != nil:
		return "", err
	}
	return textx.NormalizeEmail(c.Email), nil
}

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	ms, err := p.s.d.Store.MembershipsOfUser(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	created, err := p.s.d.Store.OrgsCreatedBy(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	addr, err := p.email(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	invs, err := p.s.d.Store.InvitationsOfUser(ctx, userID, addr)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	out := privacy.ExportSection{Module: "orgs", Items: make([]map[string]any, 0, len(ms)+len(created)+len(invs))}
	for _, m := range ms {
		item := map[string]any{
			"record": "membership", "membership_id": m.ID.String(), "org_id": m.OrgID.String(),
			"org_kind": string(m.OrgKind), "role": string(m.Role), "status": string(m.Status), "source": string(m.Source),
		}
		putTime(item, "requested_at", m.RequestedAt)
		putTime(item, "joined_at", m.JoinedAt)
		putTime(item, "ended_at", m.EndedAt)
		if m.EndReason != nil {
			item["end_reason"] = string(*m.EndReason)
		}
		out.Items = append(out.Items, item)
	}
	for _, i := range invs {
		item := map[string]any{
			"record": "invitation", "invitation_id": i.ID.String(), "org_id": i.OrgID.String(),
			"org_kind": string(i.OrgKind), "role": string(i.Role), "email": i.Email,
			"created_at": i.CreatedAt.UTC().Format(time.RFC3339), "expires_at": i.ExpiresAt.UTC().Format(time.RFC3339),
		}
		putTime(item, "accepted_at", i.AcceptedAt)
		putTime(item, "revoked_at", i.RevokedAt)
		out.Items = append(out.Items, item)
	}
	for _, o := range created {
		out.Items = append(out.Items, map[string]any{
			"record": "registered_org", "org_id": o.ID.String(), "org_kind": string(o.Kind), "name": o.Name,
			"created_at": o.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

func putTime(item map[string]any, key string, t *time.Time) {
	if t != nil {
		item[key] = t.UTC().Format(time.RFC3339)
	}
}

// Erase implements privacy.Provider. It is idempotent: a second run finds
// no open membership and no invitation.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	addr, err := p.email(ctx, userID)
	if err != nil {
		return privacy.EraseResult{}, err
	}
	s := p.s
	a := authz.SystemActor(eraseJob)
	res := privacy.EraseResult{Module: "orgs"}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		ms, err := s.d.Store.MembershipsOfUser(ctx, userID)
		if err != nil {
			return err
		}
		now := s.now()
		for _, m := range ms {
			sens, err := s.sensitive(ctx, m.OrgID, m.OrgKind)
			if err != nil {
				return err
			}
			switch {
			case m.Status != MemberEnded:
				e, err := s.endMember(ctx, m, EndAccountErased, sens, now)
				if err != nil {
					return err
				}
				if err := s.d.Audit.Log(ctx, memberAudit(a, authz.OrgsMemberEndRole, e, sens, "",
					map[string]any{"end_reason": string(EndAccountErased)})); err != nil {
					return err
				}
			case sens: // an ended sensitive row that survived (e.g. the club became sensitive later)
				if err := s.d.Store.DeleteMember(ctx, m.OrgID, m.ID); err != nil {
					return err
				}
			}
			if sens {
				res.Deleted++
			} else {
				res.Retained++
			}
		}
		n, err := s.d.Store.DeleteInvitationsOfUser(ctx, userID, addr)
		res.Deleted += n
		return err
	})
	if err != nil {
		return privacy.EraseResult{}, err
	}
	if res.Retained > 0 {
		res.RetainedReason = retainedReason
	}
	return res, nil
}
