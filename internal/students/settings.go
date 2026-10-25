package students

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem"
)

// ConsentProof is the consent text a student saw when turning a mirrored
// flag on (04 §2.4 updateMyPrivacy "consent: {notice_version_id,
// text_sha256}" per consent opened).
type ConsentProof struct {
	NoticeVersionID uuid.UUID
	TextSHA256      []byte
}

// PrivacyPatch is updateMyPrivacy (nil: unchanged).
type PrivacyPatch struct {
	Discoverable          *bool
	ShowContactOnGrant    *bool
	ShowClubsOnGrant      *bool
	ActivityRankingChoice *RankingChoice
	// DiscoveryConsent proves the discovery_profile text when Discoverable
	// turns on; ContactConsent the contact_info_share text when
	// ShowContactOnGrant turns on.
	DiscoveryConsent *ConsentProof
	ContactConsent   *ConsentProof
	// Channel is the consent channel (web | mobile; X-Koro-Client).
	Channel consent.Channel
}

func (p PrivacyPatch) empty() bool {
	return p.Discoverable == nil && p.ShowContactOnGrant == nil && p.ShowClubsOnGrant == nil &&
		p.ActivityRankingChoice == nil
}

// GetMyPrivacy implements Service: the defaults (everything off, ranking
// "default") until the student changes a setting.
func (s *service) GetMyPrivacy(ctx context.Context, a authz.Actor) (Privacy, error) {
	if err := s.can(ctx, a, authz.StudentsPrivacyReadOwn); err != nil {
		return Privacy{}, err
	}
	p, err := s.d.Store.GetPrivacy(ctx, a.UserID)
	if errors.Is(err, ErrNotFound) {
		return defaultPrivacy(a.UserID), nil
	}
	return p, err
}

// UpdateMyPrivacy implements Service. Order: authz (k-uv 403) → the patch
// (422) → a minor turning discoverable or contact sharing on (422
// students.minor_restricted, 03 k9) → in one transaction: a mirrored flag
// turning on without the consent text shown (422 consent.proof_required,
// nothing written), the settings row, the consent record of every mirrored
// flag that changes (granted with the shown text, withdrawn without),
// student.ranking_choice_changed (O) and the audit record. A rollback
// removes all of them; an unchanged setting writes nothing.
func (s *service) UpdateMyPrivacy(ctx context.Context, a authz.Actor, patch PrivacyPatch) (Privacy, error) {
	if err := s.can(ctx, a, authz.StudentsPrivacyUpdateOwn); err != nil {
		return Privacy{}, err
	}
	if patch.empty() {
		return Privacy{}, ErrEmptyPatch
	}
	if patch.ActivityRankingChoice != nil && !patch.ActivityRankingChoice.Valid() {
		return Privacy{}, problem.Field("body.activity_ranking_choice", "must be default, enabled or disabled")
	}
	if a.IsMinor && (isTrue(patch.Discoverable) || isTrue(patch.ShowContactOnGrant)) {
		return Privacy{}, ErrMinorRestricted
	}
	channel := patch.Channel
	if channel != consent.ChannelMobile {
		channel = consent.ChannelWeb
	}
	var out Privacy
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockPrivacy(ctx, a.UserID)
		if err != nil {
			return err
		}
		next, changed := cur, map[string]any{}
		if v := patch.Discoverable; v != nil && *v != cur.Discoverable {
			next.Discoverable, changed["discoverable"] = *v, *v
		}
		if v := patch.ShowContactOnGrant; v != nil && *v != cur.ShowContactOnGrant {
			next.ShowContactOnGrant, changed["show_contact_on_grant"] = *v, *v
		}
		if v := patch.ShowClubsOnGrant; v != nil && *v != cur.ShowClubsOnGrant {
			next.ShowClubsOnGrant, changed["show_clubs_on_grant"] = *v, *v
		}
		if v := patch.ActivityRankingChoice; v != nil && *v != cur.ActivityRankingChoice {
			next.ActivityRankingChoice, changed["activity_ranking_choice"] = *v, string(*v)
		}
		if len(changed) == 0 {
			out = cur
			return nil
		}
		mirrors := []struct {
			from, to bool
			purpose  consent.Purpose
			proof    *ConsentProof
			field    string
		}{
			{cur.Discoverable, next.Discoverable, consent.PurposeDiscoveryProfile, patch.DiscoveryConsent, "body.consent.discovery_profile"},
			{cur.ShowContactOnGrant, next.ShowContactOnGrant, consent.PurposeContactInfoShare, patch.ContactConsent, "body.consent.contact_info_share"},
		}
		// A flag turning on needs the consent text shown: refused before
		// anything is written.
		for _, m := range mirrors {
			if m.to && !m.from && m.proof == nil {
				return consent.ErrProofRequired.WithFields(fieldError(m.field, "the consent text shown is required to turn this on"))
			}
		}
		if out, err = s.d.Store.UpdatePrivacy(ctx, next); err != nil {
			return err
		}
		for _, m := range mirrors {
			if m.from == m.to {
				continue
			}
			id, err := s.recordConsent(ctx, a, m.purpose, m.to, m.proof, channel)
			if err != nil {
				return err
			}
			changed[string(m.purpose)+"_consent_id"] = id.String()
		}
		// Contact sharing off stops it with every company (BE-089).
		if cur.ShowContactOnGrant && !next.ShowContactOnGrant {
			if err := s.clearContactShares(ctx, a); err != nil {
				return err
			}
		}
		if _, ok := changed["activity_ranking_choice"]; ok {
			if err := s.emitOutbox(ctx, domainevent.StudentRankingChoiceChanged{
				UserID: a.UserID, Choice: string(next.ActivityRankingChoice),
			}, "student_privacy", a.UserID); err != nil {
				return err
			}
		}
		return s.d.Audit.Log(ctx, writeEvent(a, authz.StudentsPrivacyUpdateOwn, changed))
	})
	if err != nil {
		return Privacy{}, err
	}
	return out, nil
}

// recordConsent appends the consent row of a mirrored flag in the caller's
// transaction (01 §6.3): a grant carries the shown consent text (checked
// by the caller), a withdrawal none.
func (s *service) recordConsent(ctx context.Context, a authz.Actor, purpose consent.Purpose, granted bool,
	proof *ConsentProof, channel consent.Channel,
) (uuid.UUID, error) {
	in := consent.RecordInput{UserID: a.UserID, Purpose: purpose, Action: consent.Withdrawn, Channel: channel, Minor: a.IsMinor}
	if granted {
		id := proof.NoticeVersionID
		in.Action, in.NoticeVersionID, in.TextSHA256 = consent.Granted, &id, proof.TextSHA256
	}
	return s.d.Consent.Record(ctx, in)
}

func isTrue(b *bool) bool { return b != nil && *b }
