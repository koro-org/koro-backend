package consent

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/outbox"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// RecordInput is one consent act (Recorder.Record). The scope type comes
// from the purpose row, never from the caller (06 H-38); IP and User-Agent
// come from the request context.
type RecordInput struct {
	UserID  uuid.UUID
	Purpose Purpose
	Action  Action
	// NoticeVersionID and TextSHA256 prove the consent text that was shown:
	// required for a grant (the current consent text of Purpose and the
	// SHA-256 of its body), optional for a withdrawal.
	NoticeVersionID *uuid.UUID
	TextSHA256      []byte
	// ScopeID is the event registration of a per-registration purpose
	// (event_sponsor_share); nil for every other purpose.
	ScopeID *uuid.UUID
	Channel Channel
	// Minor is true when the data subject is under 18 (Actor.IsMinor or the
	// birth date at registration): 03 k12 refuses restricted grants.
	Minor bool
}

// MyConsent is the body of recordMyConsent (the subject is the actor).
type MyConsent struct {
	Purpose         Purpose
	Action          Action
	NoticeVersionID *uuid.UUID
	TextSHA256      []byte
	ScopeID         *uuid.UUID
	Channel         Channel // web | mobile (X-Koro-Client)
}

// PageQuery is one page of a consent list.
type PageQuery struct {
	Purpose *Purpose
	Limit   int
	After   *After
}

// MyConsents is listMyConsents: the current state of every purpose and
// scope, and one page of the full history.
type MyConsents struct {
	Current []Consent
	Page    Page
}

// Access carries the break-glass headers of adminListUserConsents (03 §6.3).
type Access struct {
	Reason    string // X-Koro-Access-Reason
	TicketRef string // X-Koro-Ticket-Ref
}

// recorderJob is the audit actor of records made by the platform rather
// than by the data subject (admin and import channels).
const recorderJob = "consent.record"

// invalid builds a 422 naming the offending request member.
func invalid(sentinel *problem.Error, location, message string) error {
	return sentinel.WithFields(problem.FieldError{Location: location, Message: message})
}

// Record implements Recorder.
func (s *service) Record(ctx context.Context, in RecordInput) (uuid.UUID, error) {
	by := authz.SystemActor(recorderJob)
	if in.Channel == ChannelWeb || in.Channel == ChannelMobile || in.Channel == ChannelEmailLink {
		by = authz.Actor{Kind: authz.KindUser, UserID: in.UserID} // the subject's own act
	}
	c, err := s.record(ctx, in, by)
	return c.ID, err
}

// RecordMine implements Service (recordMyConsent).
func (s *service) RecordMine(ctx context.Context, a authz.Actor, in MyConsent) (Consent, error) {
	me := a.UserID
	if err := authz.Can(ctx, a, authz.ConsentRecordCreate, authz.Resource{Type: authz.TypeConsentRecord, OwnerUserID: &me}); err != nil {
		return Consent{}, err
	}
	if in.Channel != ChannelWeb && in.Channel != ChannelMobile {
		return Consent{}, fmt.Errorf("consent: recordMyConsent channel %q", in.Channel)
	}
	hash, err := idempotency.Hash(struct {
		User  uuid.UUID
		In    MyConsent
		Minor bool
	}{me, in, a.IsMinor})
	if err != nil {
		return Consent{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Consent, error) {
		return s.record(ctx, RecordInput{
			UserID: me, Purpose: in.Purpose, Action: in.Action, NoticeVersionID: in.NoticeVersionID,
			TextSHA256: in.TextSHA256, ScopeID: in.ScopeID, Channel: in.Channel, Minor: a.IsMinor,
		}, a)
	})
	return out, err
}

// record validates in and appends it in a transaction (joining the
// caller's): consent row, audit row, the consent.recorded event (every
// purpose, BE-104: students clears the contact shares of a
// contact_info_share withdrawal) and, for a requires_iys purpose, the İYS
// submission.
func (s *service) record(ctx context.Context, in RecordInput, by authz.Actor) (Consent, error) {
	return s.recordWith(ctx, in, by, nil)
}

// recordWith is record with extra audit metadata (IDs and codes only). A
// withdrawal through ChannelIYS came from İYS (consent.iys_status_sync,
// BE-103): İYS already holds the refusal, so no İYS submission and no
// consent.recorded event are written for it (only requires_iys purposes
// come from İYS, and no consumer but consent.submit_iys acts on them).
func (s *service) recordWith(ctx context.Context, in RecordInput, by authz.Actor, meta map[string]any) (Consent, error) {
	if in.UserID == uuid.Nil() {
		return Consent{}, errors.New("consent: record without a user")
	}
	var out Consent
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.purposes().Purpose(ctx, in.Purpose)
		if errors.Is(err, ErrUnknownPurpose) {
			return invalid(ErrUnknownPurpose, "body.purpose_code", "is not a consent purpose")
		}
		if err != nil {
			return err
		}
		if err := s.check(ctx, p, in); err != nil {
			return err
		}
		c := Consent{
			ID: uuid.NewV7(), UserID: in.UserID, PurposeCode: p.Code, Action: in.Action,
			NoticeVersionID: in.NoticeVersionID, TextSHA256: in.TextSHA256, ScopeType: p.ScopeType, ScopeID: in.ScopeID,
			Channel: in.Channel, IP: canonicalIP(httpx.ClientIPFrom(ctx)), UserAgent: cut(httpx.UserAgentFrom(ctx), maxUserAgent),
			OccurredAt: s.now(),
		}
		if err := s.consents().InsertConsent(ctx, c); err != nil {
			return err
		}
		ev := recordEvent(c, by)
		for k, v := range meta {
			ev.Metadata[k] = v
		}
		if err := s.d.Audit.Log(ctx, ev); err != nil {
			return err
		}
		if c.Channel != ChannelIYS {
			if err := s.emitRecorded(ctx, p, c); err != nil {
				return err
			}
		}
		out = c
		return nil
	})
	if err != nil {
		return Consent{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "consent recorded", slog.String("consent_id", out.ID.String()),
		slog.String("purpose", string(out.PurposeCode)), slog.String("action", string(out.Action)),
		slog.String("channel", string(out.Channel)))
	return out, nil
}

// maxUserAgent is consents_user_agent_chk.
const maxUserAgent = 400

func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// check applies the consent rules to in for purpose p.
func (s *service) check(ctx context.Context, p PurposeDef, in RecordInput) error {
	if p.LegalBasis != BasisExplicitConsent {
		return invalid(ErrNotConsentBased, "body.purpose_code", "rests on "+string(p.LegalBasis))
	}
	switch in.Channel {
	case ChannelWeb, ChannelMobile, ChannelEmailLink, ChannelAdmin, ChannelImport, ChannelIYS:
	default:
		return fmt.Errorf("consent: unknown channel %q", in.Channel)
	}
	switch {
	case p.ScopeType == ScopeRegistration && in.ScopeID == nil:
		return invalid(ErrScopeRequired, "body.scope_id", "is required for this purpose")
	case p.ScopeType == ScopeNone && in.ScopeID != nil:
		return invalid(ErrScopeNotAllowed, "body.scope_id", "is not allowed for this purpose")
	case in.ScopeID != nil && *in.ScopeID == uuid.Nil():
		return invalid(ErrScopeRequired, "body.scope_id", "must not be the nil UUID")
	}
	if (in.NoticeVersionID == nil) != (len(in.TextSHA256) == 0) {
		return invalid(ErrProofRequired, "body.text_sha256", "notice_version_id and text_sha256 go together")
	}
	switch in.Action {
	case Granted:
		switch {
		case !p.Active:
			return invalid(ErrPurposeInactive, "body.purpose_code", "is not active")
		case in.Minor && p.minorRestricted():
			return ErrMinorRestricted
		case in.Channel == ChannelEmailLink, in.Channel == ChannelIYS:
			return ErrChannelNotAllowed // withdrawal-only channels
		case s.iysGateClosed(p):
			return ErrIYSUnavailable
		case in.NoticeVersionID == nil:
			return invalid(ErrProofRequired, "body.notice_version_id", "is required to grant")
		}
		return s.checkText(ctx, p, *in.NoticeVersionID, in.TextSHA256, true)
	case Withdrawn: // always allowed (03 k12); a given text must still fit
		if in.NoticeVersionID != nil {
			return s.checkText(ctx, p, *in.NoticeVersionID, in.TextSHA256, false)
		}
		return nil
	}
	return invalid(problem.ErrValidation, "body.action", "must be granted or withdrawn")
}

// iysGateClosed is the İYS gate of a grant (T-364, BE-105): İYS registers
// a consent taken outside İYS within three business days only (Yönetmelik
// m.7/11–12), so without an İYS adapter a grant of a requires_iys purpose
// could never become valid and is refused. listCurrentNotices' collectable
// reads the same rule (BE-114).
func (s *service) iysGateClosed(p PurposeDef) bool { return p.RequiresIYS && !s.d.IYS.Real() }

// collectable reports whether a grant of p can be recorded now (for an
// adult; 03 k12 still applies to minors): an active explicit-consent
// purpose whose İYS gate is open. The zero PurposeDef (an unknown purpose)
// is not collectable.
func (s *service) collectable(p PurposeDef) bool {
	return p.Active && p.LegalBasis == BasisExplicitConsent && !s.iysGateClosed(p)
}

// checkText verifies that noticeID is the consent text of purpose p and
// that sum is the digest of its body; a grant needs the current version.
func (s *service) checkText(ctx context.Context, p PurposeDef, noticeID uuid.UUID, sum []byte, current bool) error {
	n, err := s.notices().Notice(ctx, noticeID)
	if errors.Is(err, ErrNotFound) {
		return invalid(ErrNoticeMismatch, "body.notice_version_id", "unknown notice")
	}
	if err != nil {
		return err
	}
	switch {
	case n.Kind != KindConsentText || n.PurposeCode == nil || *n.PurposeCode != p.Code:
		// A privacy notice is never consented to and the terms of use are
		// never consent proof (Kurul 2026/347).
		return invalid(ErrNoticeMismatch, "body.notice_version_id", "is not the consent text of this purpose")
	case n.Status() == StatusDraft, current && n.Status() != StatusPublished:
		return ErrNoticeNotCurrent
	case len(sum) != len(n.BodySHA256) || subtle.ConstantTimeCompare(sum, n.BodySHA256) != 1:
		return invalid(ErrTextMismatch, "body.text_sha256", "does not match the notice body")
	}
	return nil
}

// recordEvent is the audit row of a consent record (consent.record.create).
func recordEvent(c Consent, by authz.Actor) audit.Event {
	meta := map[string]any{
		"purpose_code": string(c.PurposeCode), "consent_action": string(c.Action), "channel": string(c.Channel),
	}
	if c.NoticeVersionID != nil {
		meta["notice_version_id"] = c.NoticeVersionID.String()
	}
	if c.ScopeID != nil {
		meta["scope_id"] = c.ScopeID.String()
	}
	if !by.IsUser() || by.UserID != c.UserID {
		meta[audit.MetaSubjectUserID] = c.UserID.String()
	}
	id := c.ID
	return audit.Event{
		Action: string(authz.ConsentRecordCreate), Outcome: audit.Success,
		ResourceType: authz.TypeConsentRecord, ResourceID: &id, Metadata: meta,
	}.By(by)
}

// emitRecorded writes the consent.recorded event of c in the recording
// transaction (ADR-0009, O) for every purpose (BE-104, T-355: its
// consumers filter by purpose — consent.submit_iys delivers the
// submission of a requires_iys purpose and does nothing for the others,
// students.clear_contact_shares acts on a contact_info_share withdrawal).
// A requires_iys purpose also gets its pending İYS submission, in the
// same transaction.
func (s *service) emitRecorded(ctx context.Context, p PurposeDef, c Consent) error {
	if p.RequiresIYS {
		if p.IYSChannel == nil {
			return fmt.Errorf("consent: purpose %s requires İYS without a channel", p.Code)
		}
		sub := Submission{ID: uuid.NewV7(), ConsentID: c.ID, Channel: *p.IYSChannel, Status: SubmissionPending}
		if err := s.submissions().InsertSubmission(ctx, sub); err != nil {
			return err
		}
	}
	return outbox.Emit(ctx, s.d.Outbox, domainevent.ConsentRecorded{
		ConsentID: c.ID, UserID: c.UserID, Purpose: string(c.PurposeCode), Action: string(c.Action),
	}, "consent", c.ID)
}

// HasActive implements Checker.
func (s *service) HasActive(ctx context.Context, userID uuid.UUID, purpose Purpose, scopeID *uuid.UUID) (bool, error) {
	p, err := s.purposes().Purpose(ctx, purpose)
	if err != nil {
		return false, err
	}
	switch {
	case p.ScopeType == ScopeRegistration && scopeID == nil:
		return false, fmt.Errorf("consent: %s is per registration: %w", purpose, ErrScopeRequired)
	case p.ScopeType == ScopeNone && scopeID != nil:
		return false, fmt.Errorf("consent: %s has no scope: %w", purpose, ErrScopeNotAllowed)
	case !p.Active:
		return false, nil
	}
	c, ok, err := s.consents().Latest(ctx, userID, purpose, scopeID)
	if err != nil {
		return false, err
	}
	return ok && c.Action == Granted, nil
}

// FilterActive implements Checker. For an İYS purpose it is also the
// send-time hold of BE-103: while CONSENT_IYS_SYNC_MAX_AGE is set and the
// channel's last complete İYS reconciliation is older (or never ran), it
// fails with ErrIYSSyncStale instead of answering.
func (s *service) FilterActive(ctx context.Context, userIDs []uuid.UUID, purpose Purpose) ([]uuid.UUID, error) {
	p, err := s.purposes().Purpose(ctx, purpose)
	if err != nil {
		return nil, err
	}
	switch {
	case p.ScopeType != ScopeNone:
		return nil, fmt.Errorf("consent: %s is per registration: %w", purpose, ErrScopeRequired)
	case !p.Active, len(userIDs) == 0:
		return []uuid.UUID{}, nil
	}
	if err := s.iysFresh(ctx, p); err != nil {
		return nil, err
	}
	return s.consents().GrantedAmong(ctx, purpose, userIDs)
}

// ListMine implements Service (listMyConsents).
func (s *service) ListMine(ctx context.Context, a authz.Actor, q PageQuery) (MyConsents, error) {
	me := a.UserID
	if err := authz.Can(ctx, a, authz.ConsentRecordListOwn, authz.Resource{Type: authz.TypeConsentRecord, OwnerUserID: &me}); err != nil {
		return MyConsents{}, err
	}
	cur, err := s.consents().CurrentOf(ctx, me)
	if err != nil {
		return MyConsents{}, err
	}
	if q.Purpose != nil {
		kept := cur[:0]
		for _, c := range cur {
			if c.PurposeCode == *q.Purpose {
				kept = append(kept, c)
			}
		}
		cur = kept
	}
	p, err := s.page(ctx, me, q)
	if err != nil {
		return MyConsents{}, err
	}
	return MyConsents{Current: cur, Page: p}, nil
}

// page reads one page of a user's consents (limit+1 rows decide Next).
func (s *service) page(ctx context.Context, userID uuid.UUID, q PageQuery) (Page, error) {
	limit := page.ClampLimit(q.Limit)
	rows, err := s.consents().ListConsents(ctx, ListQuery{UserID: userID, Purpose: q.Purpose, After: q.After, Limit: limit + 1})
	if err != nil {
		return Page{}, err
	}
	p := Page{Items: rows}
	if len(rows) > limit {
		p.Items = rows[:limit]
		last := p.Items[limit-1]
		p.Next = &After{At: last.OccurredAt, ID: last.ID}
	}
	return p, nil
}

// AdminListUser implements Service (adminListUserConsents): the proof of a
// user's consents with IP, channel and text digest. Every call is audited
// (03 §6.5); a support read is break-glass (03 §6.3) and shows up in the
// subject's own security events.
func (s *service) AdminListUser(ctx context.Context, a authz.Actor, userID uuid.UUID, access Access, q PageQuery) (Page, error) {
	r := authz.Resource{Type: authz.TypeConsentRecord, ID: userID, OwnerUserID: &userID,
		Attrs: map[string]string{authz.AttrAccessReason: access.Reason}}
	if err := authz.Can(ctx, a, authz.ConsentRecordListAdmin, r); err != nil {
		return Page{}, err
	}
	breakGlass := !a.HasPlatformRole(authz.PlatformDPO) // the grant came from support's B cell
	var out Page
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		p, err := s.page(ctx, userID, q)
		if err != nil {
			return err
		}
		meta := map[string]any{audit.MetaSubjectUserID: userID.String(), "returned": len(p.Items)}
		if breakGlass {
			meta[audit.MetaBreakGlass] = true
			if access.TicketRef != "" {
				meta[audit.MetaTicketRef] = access.TicketRef
			}
		}
		target := userID
		ev := audit.Event{
			Action: string(authz.ConsentRecordListAdmin), Outcome: audit.Success, ResourceType: authz.TypeUser,
			ResourceID: &target, Reason: access.Reason, Metadata: meta,
		}.By(a)
		if err := s.d.Audit.Log(ctx, ev); err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, err
}
