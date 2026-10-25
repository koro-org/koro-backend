package identity

import (
	"context"
	"errors"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
)

// Terms of use acceptance (BE-118, user decision Q-17). Registration
// requires the mandatory "I have read and accept the terms of use" box:
// the accepted version goes to consent's terms ledger in the registration
// transaction. When a new version is published, getMe says so (terms
// acceptance_required) and acceptTerms records the acceptance. The privacy
// notice is shown, never accepted (02 §4.2): it has no box.

// TermsAcceptanceInput is the body of acceptTerms.
type TermsAcceptanceInput struct {
	NoticeVersionID uuid.UUID  // the terms of use version shown
	Client          ClientKind // X-Koro-Client: the channel
}

// TermsState is the caller's position towards the terms of use (getMe,
// acceptTerms).
type TermsState struct {
	// Required: a current version exists and the caller has not accepted
	// it; the client shows it and calls acceptTerms.
	Required bool
	// CurrentID and CurrentVersion name the version in force for the
	// caller's audience and locale; nil / "" when none is published.
	CurrentID      *uuid.UUID
	CurrentVersion string
	// AcceptedID and AcceptedAt are the caller's acceptance of the current
	// version or, when there is none, of the newest earlier one.
	AcceptedID *uuid.UUID
	AcceptedAt *time.Time
}

// audienceOf is the notice audience of an account: its signup persona's,
// "all" for the platform persona (staff accounts are seeded, not signed up).
func audienceOf(p Persona) consent.Audience {
	if a, ok := personaAudience[p]; ok {
		return a
	}
	return consent.AudienceAll
}

// channelOf is the consent channel of a client kind.
func channelOf(c ClientKind) consent.Channel {
	if c == ClientMobile {
		return consent.ChannelMobile
	}
	return consent.ChannelWeb
}

// termsState reads the terms position of u.
func (s *service) termsState(ctx context.Context, u User) (TermsState, error) {
	st, err := s.d.Terms.TermsStatus(ctx, u.ID, audienceOf(u.Persona), u.Locale)
	if err != nil {
		return TermsState{}, err
	}
	out := TermsState{Required: st.Required}
	if st.Current != nil {
		id := st.Current.ID
		out.CurrentID, out.CurrentVersion = &id, st.Current.Version
	}
	if st.Accepted != nil {
		id, at := st.Accepted.NoticeVersionID, st.Accepted.AcceptedAt
		out.AcceptedID, out.AcceptedAt = &id, &at
	}
	return out, nil
}

// termsNotCurrent is the 422 of a request member that does not name the
// current terms of use (the code is consent's, 06 H-52).
func termsNotCurrent(location string) error {
	return invalid(consent.ErrTermsNotCurrent, location, "must be the current terms of use")
}

// AcceptTerms implements Service (acceptTerms). The account row is locked,
// so concurrent acceptances of one account run one after the other; a
// version accepted before is not recorded again (the answer is the same).
func (s *service) AcceptTerms(ctx context.Context, a authz.Actor, in TermsAcceptanceInput) (TermsState, error) {
	if err := authz.Can(ctx, a, authz.IdentityAccountUpdateOwn, ownUser(a)); err != nil {
		return TermsState{}, err
	}
	const field = "body.notice_version_id"
	if in.NoticeVersionID == uuid.Nil() {
		return TermsState{}, termsNotCurrent(field)
	}
	var u User
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if u, err = s.d.Store.LockUser(ctx, a.UserID); err != nil {
			return err
		}
		_, err = s.d.Terms.RecordTerms(ctx, consent.TermsInput{
			UserID: u.ID, NoticeVersionID: in.NoticeVersionID, Audience: audienceOf(u.Persona), Channel: channelOf(in.Client),
		})
		if errors.Is(err, consent.ErrTermsNotCurrent) {
			return termsNotCurrent(field)
		}
		return err
	})
	if err != nil {
		return TermsState{}, err
	}
	return s.termsState(ctx, u)
}
