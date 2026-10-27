package students

import (
	"context"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// Per-company contact sharing (BE-089; user decisions Q-06/T-335 and
// T-345, 2026-10-23). The student shares the e-mail with one company: the
// company of the contact request being accepted (respondMyContactRequest
// share_contact), recorded on that company's level 3 grant
// (profile_access_grants.contact_shared_at). A company sees the e-mail only
// while its grant is active and shares it and the student's
// contact_info_share consent is in force (read at every view, T-237). The
// student stops it for one company (unshareMyAccessGrantContact) or ends
// the company's access (revokeMyAccessGrant).
//
// The privacy setting show_contact_on_grant no longer opens the e-mail to
// anyone: it mirrors the contact_info_share consent and is the default of
// the "share my contact" box of the next acceptance (the client pre-ticks
// it; a share turns it on). Turning it on records the consent and shares
// with nobody; turning it off withdraws the consent and stops the sharing
// with every company in the same transaction; a withdrawal recorded
// elsewhere (the consent module) stops it through consent.recorded, and
// the nightly privacy reconciliation turning the setting off does too
// (BE-104). A
// share never outlives the consent it was given under: shares older than
// a withdrawal are cleared, so a later consent revives none of them.
const (
	// ClearContactSharesTaskName consumes consent.recorded: a
	// contact_info_share withdrawal clears the shares given before it.
	ClearContactSharesTaskName = "students.clear_contact_shares"

	// auditContactShare is the audit action of a share given with an
	// acceptance (03 §4.1 format, written in messaging's transaction; not
	// an HTTP action).
	auditContactShare = "students.access_grant.share_contact"
	// contactShareJob is the system actor of a share cleared by a consent
	// withdrawal.
	contactShareJob = "students.contact_shares"
)

// ContactSharing is the per-company contact sharing of messaging's
// acceptance (01 §6.3 "İletişim talebi kabulü", T-335/T-345; BE-089).
type ContactSharing interface {
	// ShareContactWithCompany shares the student a's e-mail with
	// companyOrgID, whose active level 3 grant the acceptance has just
	// given (AccessGrants.Grant), in the transaction carried by ctx
	// (messaging → students is a sync write, 01 §6.3): the acceptance and
	// the sharing commit or roll back together. The contact_info_share
	// consent is recorded with the text the student was shown (proof)
	// unless it is in force; no other company starts seeing the e-mail.
	// A minor never shares (422 students.minor_restricted, 03 k9); a proof
	// that is not the current contact_info_share text is the consent
	// module's 422/409 and nothing is written; no active grant of the
	// company is not found. It does not authorise: messaging decides who
	// answers (messaging.contact_request.respond). requestID is the
	// accepted contact request (audit).
	ShareContactWithCompany(ctx context.Context, a authz.Actor, companyOrgID uuid.UUID, proof ConsentProof,
		channel consent.Channel, requestID uuid.UUID) error
}

// contactShareService is ContactSharing plus the HTTP use case of the
// sharing (students.Service embeds it).
type contactShareService interface {
	ContactSharing
	// UnshareMyAccessGrantContact is unshareMyAccessGrantContact
	// (students.access_grant.revoke: a partial revocation of the
	// student's own grant): the company stops seeing the e-mail at once;
	// its level 3 access stays. A grant that does not share it changes
	// nothing; another user's grant is 404.
	UnshareMyAccessGrantContact(ctx context.Context, a authz.Actor, grantID uuid.UUID) error
}

var _ contactShareService = (*service)(nil)

// ShareContactWithCompany implements ContactSharing. In the caller's
// transaction: the student's grant lock, the company's active grant (none:
// not found), the settings row; when the contact_info_share consent is not
// in force the shares given under an earlier consent are cleared and the
// shown text is recorded; show_contact_on_grant (the consent mirror and
// the default of the next acceptance) is set; the grant's
// contact_shared_at; the audit record of the student's act (org-scoped
// like the grant itself: the company is the other party).
func (s *service) ShareContactWithCompany(ctx context.Context, a authz.Actor, companyOrgID uuid.UUID, proof ConsentProof,
	channel consent.Channel, requestID uuid.UUID,
) error {
	if a.IsMinor {
		return ErrMinorRestricted
	}
	if channel != consent.ChannelMobile {
		channel = consent.ChannelWeb
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.grants().LockGrants(ctx, a.UserID); err != nil {
			return err
		}
		g, err := s.grants().ActiveGrant(ctx, a.UserID, companyOrgID)
		if err != nil {
			return err
		}
		pv, err := s.privacy().LockPrivacy(ctx, a.UserID)
		if err != nil {
			return err
		}
		meta := map[string]any{"contact_request_id": requestID.String()}
		on, err := s.consentActive(ctx, a.UserID, consent.PurposeContactInfoShare)
		if err != nil {
			return err
		}
		if !on {
			stale, err := s.grants().ClearGrantContacts(ctx, a.UserID, nil)
			if err != nil {
				return err
			}
			if err := s.auditUnshares(ctx, a, stale, "consent_renewed"); err != nil {
				return err
			}
			p := proof
			id, err := s.recordConsent(ctx, a, consent.PurposeContactInfoShare, true, &p, channel)
			if err != nil {
				return err
			}
			meta[string(consent.PurposeContactInfoShare)+"_consent_id"] = id.String()
		}
		if !pv.ShowContactOnGrant {
			next := pv
			next.ShowContactOnGrant = true
			if _, err := s.privacy().UpdatePrivacy(ctx, next); err != nil {
				return err
			}
			meta["show_contact_on_grant"] = true
		}
		shared, err := s.grants().ShareGrantContact(ctx, g.ID, s.now())
		if err != nil || (!shared && len(meta) == 1) {
			return err // already shared under the consent in force: nothing to record
		}
		ev := grantEvent(auditContactShare, g, meta)
		return s.d.Audit.Log(ctx, ev.By(a))
	})
}

// UnshareMyAccessGrantContact implements Service. Order: authz (O on the
// student's own grant, no verification needed: KVKK md. 11 like revoke) →
// in one transaction: the grant lock, the student's grant (another user's
// or unknown: 404), contact_shared_at cleared, the audit record.
func (s *service) UnshareMyAccessGrantContact(ctx context.Context, a authz.Actor, grantID uuid.UUID) error {
	if err := s.canOwnGrants(ctx, a, authz.StudentsAccessGrantRevoke, grantID); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.grants().LockGrants(ctx, a.UserID); err != nil {
			return err
		}
		g, err := s.grants().GetGrant(ctx, a.UserID, grantID)
		if err != nil {
			return err
		}
		ok, err := s.grants().UnshareGrantContact(ctx, a.UserID, g.ID)
		if err != nil || !ok {
			return err
		}
		return s.auditUnshares(ctx, a, []AccessGrant{g}, "student")
	})
}

// clearContactShares stops every share of userID in the caller's
// transaction (show_contact_on_grant turned off: the consent is withdrawn
// in the same transaction).
func (s *service) clearContactShares(ctx context.Context, a authz.Actor) error {
	gs, err := s.grants().ClearGrantContacts(ctx, a.UserID, nil)
	if err != nil {
		return err
	}
	return s.auditUnshares(ctx, a, gs, "privacy_setting")
}

// handleConsentRecorded is ClearContactSharesTaskName: a contact_info_share
// withdrawal recorded anywhere (the consent module's recordMyConsent, the
// admin and import channels, a KVKK erasure; consent writes the event for
// every purpose since BE-104) clears the shares given under the withdrawn
// consent, so a consent given later revives none of them. Until the task
// runs the views already hide the e-mail (the consent is read at every
// view). Every other purpose and every grant is ignored.
//
// Under the student's grant lock (ShareContactWithCompany takes it too):
// when the consent is still not in force every remaining share is stale —
// a share is only ever given while the consent is in force — including
// one given concurrently with the withdrawal; when it was given again
// since, only the shares given at or before the withdrawal event go and
// those of the new consent stay. A late redelivery is harmless either way.
func (s *service) handleConsentRecorded(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ConsentRecorded](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.Purpose != string(consent.PurposeContactInfoShare) || p.Action != string(consent.Withdrawn) {
		return nil
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.grants().LockGrants(ctx, p.UserID); err != nil {
			return err
		}
		on, err := s.consentActive(ctx, p.UserID, consent.PurposeContactInfoShare)
		if err != nil {
			return err
		}
		var before *time.Time // not in force (or no checker: fail closed): every share
		if on {
			at := e.OccurredAt
			before = &at
		}
		gs, err := s.grants().ClearGrantContacts(ctx, p.UserID, before)
		if err != nil {
			return err
		}
		return s.auditUnshares(ctx, authz.SystemActor(contactShareJob), gs, "consent_withdrawn")
	})
}

// auditUnshares writes one audit record per grant that stopped sharing the
// e-mail (students.access_grant.revoke with revoked: contact; org-scoped
// like every grant record).
func (s *service) auditUnshares(ctx context.Context, a authz.Actor, gs []AccessGrant, why string) error {
	for _, g := range gs {
		meta := map[string]any{"revoked": "contact", "by": why}
		if g.ContactSharedAt != nil {
			meta["contact_shared_at"] = g.ContactSharedAt.UTC().Format(time.RFC3339Nano)
		}
		ev := grantEvent(string(authz.StudentsAccessGrantRevoke), g, meta)
		if err := s.d.Audit.Log(ctx, ev.By(a)); err != nil {
			return err
		}
	}
	return nil
}
