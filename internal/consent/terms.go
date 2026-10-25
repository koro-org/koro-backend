package consent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/obs"
)

// The terms of use acceptance ledger (BE-118, user decision Q-17;
// terms_acceptances, 000148). Accepting the terms of use forms the service
// contract: it is not a KVKK explicit consent, so it never goes to the
// consents ledger (Kurul 2026/347: consent is not bundled with the
// contract) and nothing here is a Checker answer. What it shares with the
// consent ledger is the notice ledger: an acceptance names a published
// terms_of_use version of notice_versions and the digest of its body, the
// row is append-only, and the subject's own act is audited in the same
// transaction.

// TermsLedger records and reads the terms of use acceptances (identity:
// registration and acceptTerms; 01 §6.3 doc patch).
type TermsLedger interface {
	// CheckTerms returns nil when noticeID is the current terms of use of a
	// reader of audience aud in the notice's own language (the version
	// Notices.Current resolves: aud before "all"), ErrTermsNotCurrent
	// otherwise. It writes nothing: registration checks before it looks the
	// address up, so a taken and a new address fail alike.
	CheckTerms(ctx context.Context, noticeID uuid.UUID, aud Audience) error
	// RecordTerms appends the acceptance of in.NoticeVersionID by in.UserID
	// in the transaction carried by ctx (a rollback of the caller removes
	// it) with its audit row. The notice must pass CheckTerms for
	// in.Audience: ErrTermsNotCurrent otherwise. Accepting a version the
	// user already accepted writes nothing and returns the earlier
	// acceptance.
	RecordTerms(ctx context.Context, in TermsInput) (TermsAcceptance, error)
	// TermsStatus returns the terms of use in force for a reader of
	// audience and locale (fallbacks: audience all, locale tr-TR) and
	// whether userID accepted them.
	TermsStatus(ctx context.Context, userID uuid.UUID, aud Audience, locale string) (TermsStatus, error)
}

// TermsInput is one acceptance (TermsLedger.RecordTerms).
type TermsInput struct {
	UserID          uuid.UUID
	NoticeVersionID uuid.UUID
	// Audience is the reader's: the accepted notice is of it or of "all".
	Audience Audience
	// Channel is web or mobile (X-Koro-Client): an acceptance is an act on
	// a page that shows the terms.
	Channel Channel
}

// TermsAcceptance is one terms_acceptances row (000148, append-only).
// TextSHA256 is the body digest of the accepted notice (the database's
// composite FK holds it to the notice row); RequestID correlates the audit
// row and the request log ("" when the context carried none).
type TermsAcceptance struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	NoticeVersionID uuid.UUID
	TextSHA256      []byte
	Channel         Channel
	RequestID       string
	AcceptedAt      time.Time
	CreatedAt       time.Time
}

func (t TermsAcceptance) clone() TermsAcceptance {
	c := t
	c.TextSHA256 = bytes.Clone(t.TextSHA256)
	return c
}

// TermsStatus is a reader's position towards the terms of use.
type TermsStatus struct {
	// Current is the terms of use in force for the reader; nil when none is
	// published (nothing to accept).
	Current *Notice
	// Accepted is the reader's acceptance of Current or, when there is
	// none, the newest acceptance of an earlier version; nil when the
	// reader never accepted any.
	Accepted *TermsAcceptance
	// Required: Current is set and the reader has not accepted it (a new
	// version was published, or the account predates the ledger). The
	// client shows the terms and calls acceptTerms.
	Required bool
}

// auditTermsAccepted is the audit action of an acceptance (not an authz
// action: the caller authorised the request; like identity's notice.shown).
const auditTermsAccepted = "terms.accepted"

// resourceTermsAcceptance is the audit resource type of an acceptance.
const resourceTermsAcceptance = "terms_acceptance"

// maxRequestID fits terms_acceptances_request_id_chk (the httpx request ID
// is at most 64 characters; a worker's "task:<id>" may be longer).
const maxRequestID = 80

// RecordTerms implements TermsLedger.
func (s *service) RecordTerms(ctx context.Context, in TermsInput) (TermsAcceptance, error) {
	switch {
	case in.UserID == uuid.Nil():
		return TermsAcceptance{}, errors.New("consent: terms acceptance without a user")
	case in.Channel != ChannelWeb && in.Channel != ChannelMobile:
		return TermsAcceptance{}, fmt.Errorf("consent: terms acceptance through channel %q", in.Channel)
	}
	var out TermsAcceptance
	var inserted bool
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		n, err := s.currentTerms(ctx, in.NoticeVersionID, in.Audience)
		if err != nil {
			return err
		}
		t := TermsAcceptance{
			ID: uuid.NewV7(), UserID: in.UserID, NoticeVersionID: n.ID, TextSHA256: n.BodySHA256,
			Channel: in.Channel, RequestID: cut(obs.RequestID(ctx), maxRequestID), AcceptedAt: s.now(),
		}
		if inserted, err = s.d.Store.InsertTermsAcceptance(ctx, t); err != nil {
			return err
		}
		if !inserted { // the version was accepted before: that acceptance stands
			prev, err := s.d.Store.TermsAcceptancesOf(ctx, in.UserID)
			if err != nil {
				return err
			}
			for _, p := range prev {
				if p.NoticeVersionID == n.ID {
					out = p
					return nil
				}
			}
			return fmt.Errorf("consent: terms acceptance of %s neither inserted nor found", n.ID)
		}
		id := t.ID
		ev := audit.Event{
			Action: auditTermsAccepted, Outcome: audit.Success, ResourceType: resourceTermsAcceptance, ResourceID: &id,
			Metadata: map[string]any{
				"notice_version_id": n.ID.String(), "version": n.Version, "audience": string(n.Audience),
				"locale": n.Locale, "channel": string(t.Channel),
			},
		}.By(authz.Actor{Kind: authz.KindUser, UserID: in.UserID}) // the subject's own act
		if err := s.d.Audit.Log(ctx, ev); err != nil {
			return err
		}
		out = t
		return nil
	})
	if err != nil {
		return TermsAcceptance{}, err
	}
	if inserted {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "terms of use accepted", slog.String("acceptance_id", out.ID.String()),
			slog.String("notice_id", out.NoticeVersionID.String()), slog.String("channel", string(out.Channel)))
	}
	return out, nil
}

// CheckTerms implements TermsLedger.
func (s *service) CheckTerms(ctx context.Context, noticeID uuid.UUID, aud Audience) error {
	_, err := s.currentTerms(ctx, noticeID, aud)
	return err
}

// currentTerms returns notice id when it is the current terms of use of a
// reader of audience aud in the notice's language: published, unretired,
// and the version Notices.Current resolves for (aud, its locale), so a
// generic "all" version does not stand in for the audience's own one.
// ErrTermsNotCurrent otherwise (unknown, draft, retired, another kind,
// another audience). The caller names the request member (identity); this
// is the ledger's own rule, so the error names no field.
func (s *service) currentTerms(ctx context.Context, id uuid.UUID, aud Audience) (Notice, error) {
	if id == uuid.Nil() || !validAudience(aud) {
		return Notice{}, ErrTermsNotCurrent
	}
	n, err := s.d.Store.Notice(ctx, id)
	switch {
	case errors.Is(err, ErrNotFound):
		return Notice{}, ErrTermsNotCurrent
	case err != nil:
		return Notice{}, err
	case n.Kind != KindTermsOfUse || n.Status() != StatusPublished:
		return Notice{}, ErrTermsNotCurrent
	}
	cur, err := s.Current(ctx, NoticeKey{Kind: KindTermsOfUse, Audience: aud, Locale: n.Locale})
	switch {
	case errors.Is(err, ErrNoNotice):
		return Notice{}, ErrTermsNotCurrent
	case err != nil:
		return Notice{}, err
	case cur.ID != n.ID:
		return Notice{}, ErrTermsNotCurrent
	}
	return n, nil
}

// TermsStatus implements TermsLedger.
func (s *service) TermsStatus(ctx context.Context, userID uuid.UUID, aud Audience, locale string) (TermsStatus, error) {
	if !validAudience(aud) || !validLocale(locale) {
		return TermsStatus{}, fmt.Errorf("consent: terms status for audience %q, locale %q", aud, locale)
	}
	var st TermsStatus
	cur, err := s.Current(ctx, NoticeKey{Kind: KindTermsOfUse, Audience: aud, Locale: locale})
	switch {
	case errors.Is(err, ErrNoNotice):
	case err != nil:
		return TermsStatus{}, err
	default:
		st.Current = &cur
	}
	all, err := s.d.Store.TermsAcceptancesOf(ctx, userID)
	if err != nil {
		return TermsStatus{}, err
	}
	for i := len(all) - 1; i >= 0; i-- { // newest first
		t := all[i]
		if st.Current != nil && t.NoticeVersionID == st.Current.ID {
			st.Accepted = &t
			break
		}
		if st.Accepted == nil {
			st.Accepted = &t
		}
	}
	st.Required = st.Current != nil && (st.Accepted == nil || st.Accepted.NoticeVersionID != st.Current.ID)
	return st, nil
}
