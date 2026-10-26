package consent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// NoticeFilter is listCurrentNotices: the audience and locale of the
// reader, optionally one kind or one purpose.
type NoticeFilter struct {
	Audience Audience
	Locale   string // tr-TR | en-US (Accept-Language)
	Kind     *NoticeKind
	Purpose  *Purpose
}

// NoticeDraft is the body of adminCreateNotice.
type NoticeDraft struct {
	Kind     NoticeKind
	Purpose  *Purpose
	Audience Audience
	Locale   string
	Version  string
	Title    string
	BodyMD   string
}

// PurposePatch is the body of adminUpdateConsentPurpose; nil fields stay.
type PurposePatch struct {
	Active        *bool
	DescriptionTR *string
}

// Limits of a notice draft and a purpose description.
const (
	maxTitleRunes       = 200    // notice_versions_title_chk
	maxBodyRunes        = 100000 // a long privacy notice fits; the request body limit is 1 MB
	maxDescriptionRunes = 1000
)

// noticeResource is the authz view of a notice (03 §3.5: no org, no
// owner). Attrs status lets the policy hide drafts from the public.
func noticeResource(n Notice) authz.Resource {
	return authz.Resource{Type: authz.TypeNoticeVersion, ID: n.ID, Attrs: map[string]string{authz.AttrStatus: string(n.Status())}}
}

// publishedNotices is the resource of a list of current notices.
var publishedNotices = authz.Resource{Type: authz.TypeNoticeVersion, Attrs: map[string]string{authz.AttrStatus: string(StatusPublished)}}

// adminSurface is the resource of the purpose management operations.
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// candidates returns the audiences and locales to try for a reader, most
// specific first: the reader's audience, then "all"; the reader's locale,
// then tr-TR.
func candidates(aud Audience, locale string) ([]Audience, []string) {
	auds, locs := []Audience{aud}, []string{locale}
	if aud != AudienceAll {
		auds = append(auds, AudienceAll)
	}
	if locale != LocaleTR {
		locs = append(locs, LocaleTR)
	}
	return auds, locs
}

// best picks, per (kind, purpose), the notice of the reader's locale and
// audience: (audience, locale), (all, locale), (audience, tr-TR), (all, tr-TR).
func best(rows []Notice, aud Audience, locale string) []Notice {
	// The reader's language first (an informed consent needs a text the
	// reader understands), then the reader's audience.
	rank := func(n Notice) int {
		r := 0
		if n.Locale != locale {
			r += 2
		}
		if n.Audience != aud {
			r++
		}
		return r
	}
	type slot struct {
		kind    NoticeKind
		purpose Purpose
	}
	chosen := map[slot]Notice{}
	var order []slot
	for _, n := range rows {
		k := slot{n.Kind, n.Key().purpose()}
		cur, ok := chosen[k]
		if !ok {
			order = append(order, k)
		}
		if !ok || rank(n) < rank(cur) {
			chosen[k] = n
		}
	}
	out := make([]Notice, 0, len(order))
	for _, k := range order {
		out = append(out, chosen[k])
	}
	return out
}

func validLocale(l string) bool { return l == LocaleTR || l == LocaleEN }

func validAudience(a Audience) bool {
	switch a {
	case AudienceAll, AudienceStudent, AudienceCompany, AudienceClub, AudienceUniversity:
		return true
	}
	return false
}

func validKind(k NoticeKind) bool {
	switch k {
	case KindPrivacyNotice, KindConsentText, KindTermsOfUse, KindCookieNotice:
		return true
	}
	return false
}

// Current implements Notices.
func (s *service) Current(ctx context.Context, key NoticeKey) (Notice, error) {
	switch {
	case !validKind(key.Kind), !validAudience(key.Audience), !validLocale(key.Locale),
		(key.Kind == KindConsentText) != (key.Purpose != nil):
		return Notice{}, errors.New("consent: invalid notice key")
	}
	auds, locs := candidates(key.Audience, key.Locale)
	kind := key.Kind
	rows, err := s.d.Store.CurrentNotices(ctx, CurrentQuery{Kind: &kind, Purpose: key.Purpose, Audiences: auds, Locales: locs})
	if err != nil {
		return Notice{}, err
	}
	if out := best(rows, key.Audience, key.Locale); len(out) > 0 {
		return out[0], nil
	}
	return Notice{}, ErrNoNotice
}

// CurrentNotice is one notice of listCurrentNotices.
type CurrentNotice struct {
	Notice
	// Collectable is set on a consent text only (nil for every other
	// kind): whether a grant of its purpose can be recorded now (BE-114,
	// T-367). false when the purpose is switched off, or when it needs İYS
	// and no İYS adapter is configured: the gate that refuses that grant
	// with consent.iys_unavailable (iysGateClosed, T-364). Clients do not
	// show the checkbox of a false one. It does not depend on the reader:
	// a minor is still refused the restricted purposes (03 k12).
	Collectable *bool
	// ScopeType is set on a consent text of a known purpose only (BE-123):
	// "registration" purposes are granted per event registration (with the
	// event form), never from the account's consent page, so clients show
	// no grant button for them (a grant without scope_id is 422
	// consent.scope_required).
	ScopeType *ScopeType
}

// ListCurrentNotices implements Service (listCurrentNotices).
func (s *service) ListCurrentNotices(ctx context.Context, a authz.Actor, q NoticeFilter) ([]CurrentNotice, error) {
	if err := authz.Can(ctx, a, authz.ConsentNoticeRead, publishedNotices); err != nil {
		return nil, err
	}
	if !validAudience(q.Audience) || !validLocale(q.Locale) {
		return nil, problem.ErrValidation
	}
	auds, locs := candidates(q.Audience, q.Locale)
	rows, err := s.d.Store.CurrentNotices(ctx, CurrentQuery{Kind: q.Kind, Purpose: q.Purpose, Audiences: auds, Locales: locs})
	if err != nil {
		return nil, err
	}
	return s.withCollectable(ctx, best(rows, q.Audience, q.Locale))
}

// withCollectable marks the consent texts of ns with CurrentNotice's
// Collectable and ScopeType; the purposes are read once, only when a consent text is
// listed. A text whose purpose is unknown is not collectable.
func (s *service) withCollectable(ctx context.Context, ns []Notice) ([]CurrentNotice, error) {
	out := make([]CurrentNotice, len(ns))
	var defs map[Purpose]PurposeDef
	for i, n := range ns {
		out[i] = CurrentNotice{Notice: n}
		if n.Kind != KindConsentText || n.PurposeCode == nil {
			continue
		}
		if defs == nil {
			ps, err := s.d.Store.Purposes(ctx)
			if err != nil {
				return nil, err
			}
			defs = make(map[Purpose]PurposeDef, len(ps))
			for _, p := range ps {
				defs[p.Code] = p
			}
		}
		def := defs[*n.PurposeCode]
		ok := s.collectable(def)
		out[i].Collectable = &ok
		if def.ScopeType != "" {
			st := def.ScopeType
			out[i].ScopeType = &st
		}
	}
	return out, nil
}

// NoticeListQuery is one page of adminListNotices.
type NoticeListQuery struct {
	Filter NoticeListFilter
	Limit  int
	After  *uuid.UUID
}

// NoticePage is a page of notices, newest first; Next is the keyset
// position of the next page (nil: the last page).
type NoticePage struct {
	Items []Notice
	Next  *uuid.UUID
}

// AdminListNotices implements Service (adminListNotices; BE-129): authz
// (consent.notice.manage: the DPO; 403 before reading) → every version —
// drafts, the current ones, the retired — newest first, without their
// texts (getNoticeVersion reads one).
func (s *service) AdminListNotices(ctx context.Context, a authz.Actor, q NoticeListQuery) (NoticePage, error) {
	if err := authz.Can(ctx, a, authz.ConsentNoticeManage, authz.Resource{Type: authz.TypeNoticeVersion}); err != nil {
		return NoticePage{}, err
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListNotices(ctx, q.Filter, q.After, limit+1)
	if err != nil {
		return NoticePage{}, err
	}
	rows, more := page.Cut(rows, limit)
	out := NoticePage{Items: rows}
	if more {
		next := rows[len(rows)-1].ID
		out.Next = &next
	}
	return out, nil
}

// GetNotice implements Service (getNoticeVersion). Retired versions stay
// readable: they are the proof of what earlier consents agreed to.
func (s *service) GetNotice(ctx context.Context, a authz.Actor, id uuid.UUID) (Notice, error) {
	n, err := s.d.Store.Notice(ctx, id)
	if err != nil {
		return Notice{}, err
	}
	if err := authz.Can(ctx, a, authz.ConsentNoticeRead, noticeResource(n)); err != nil {
		return Notice{}, err
	}
	return n, nil
}

// reauth verifies the proof of an R operation after authz.Can (03 §6.2) and
// returns its kind for the audit row. Only a verdict is audited: a refused
// proof (a 4xx: reauth_required, reauth_failed, rate_limited) is denied
// with reason reauth_failed. An error without a verdict — the request
// budget ran out before identity could check the proof (BE-070/071), or a
// dependency failed — writes nothing and goes back unchanged; httpx answers
// 503 (WARN) or 500 and logs it once (BE-072; audit.ReauthFailed, BE-074).
func (s *service) reauth(ctx context.Context, a authz.Actor, act authz.Action, resType string, resID *uuid.UUID, proof ReauthProof) (string, error) {
	if s.d.Reauth == nil {
		return "", ErrReauthUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		denied := audit.Event{Action: string(act), ResourceType: resType, ResourceID: resID}.By(a)
		return "", audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	return kind, nil
}

// cleanBody checks a notice body: valid UTF-8, line breaks normalised to
// LF, no control characters other than tab and newline, no bidirectional
// overrides (display spoofing), not blank, at most maxBodyRunes.
func cleanBody(raw string) (string, error) {
	bad := invalid(ErrInvalidNotice, "body.body_md", "must be printable Markdown text")
	if !utf8.ValidString(raw) {
		return "", bad
	}
	body := strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n")
	for _, r := range body {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || (r >= '\u202a' && r <= '\u202e') || (r >= '\u2066' && r <= '\u2069') {
			return "", bad
		}
	}
	switch {
	case strings.TrimSpace(body) == "":
		return "", invalid(ErrInvalidNotice, "body.body_md", "must not be empty")
	case utf8.RuneCountInString(body) > maxBodyRunes:
		return "", invalid(ErrInvalidNotice, "body.body_md", "is too long")
	}
	return body, nil
}

// checkDraft validates a draft against 02 §4.2 and the consent rules.
func (s *service) checkDraft(ctx context.Context, in NoticeDraft) (NoticeDraft, error) {
	switch {
	case !validKind(in.Kind):
		return in, invalid(ErrInvalidNotice, "body.kind", "is not a notice kind")
	case !validAudience(in.Audience):
		return in, invalid(ErrInvalidNotice, "body.audience", "is not an audience")
	case !validLocale(in.Locale):
		return in, invalid(ErrInvalidNotice, "body.locale", "must be tr-TR or en-US")
	case !versionRe.MatchString(in.Version):
		return in, invalid(ErrInvalidNotice, "body.version", "must be YYYY.MM.DD or YYYY.MM.DD-N")
	case in.Kind == KindConsentText && in.Purpose == nil:
		return in, invalid(ErrInvalidNotice, "body.purpose_code", "is required for a consent text")
	case in.Kind != KindConsentText && in.Purpose != nil:
		// Privacy notices, terms and cookie notices carry no purpose: they
		// are never a consent (Kurul 2026/347).
		return in, invalid(ErrInvalidNotice, "body.purpose_code", "is allowed for a consent text only")
	}
	title, err := textx.NormalizeText(in.Title)
	switch {
	case err != nil:
		return in, invalid(ErrInvalidNotice, "body.title", "must be printable text")
	case title == "":
		return in, invalid(ErrInvalidNotice, "body.title", "must not be empty")
	case utf8.RuneCountInString(title) > maxTitleRunes:
		return in, invalid(ErrInvalidNotice, "body.title", "must be at most 200 characters")
	}
	in.Title = title
	if in.BodyMD, err = cleanBody(in.BodyMD); err != nil {
		return in, err
	}
	if in.Purpose != nil {
		p, err := s.d.Store.Purpose(ctx, *in.Purpose)
		switch {
		case errors.Is(err, ErrUnknownPurpose):
			return in, invalid(ErrUnknownPurpose, "body.purpose_code", "is not a consent purpose")
		case err != nil:
			return in, err
		case p.LegalBasis != BasisExplicitConsent:
			return in, invalid(ErrNotConsentBased, "body.purpose_code", "rests on "+string(p.LegalBasis))
		}
	}
	return in, nil
}

// noticeEvent is the audit row of a notice change.
func noticeEvent(a authz.Actor, n Notice, outcomeMeta map[string]any) audit.Event {
	meta := map[string]any{"kind": string(n.Kind), "audience": string(n.Audience), "locale": n.Locale, "version": n.Version}
	if n.PurposeCode != nil {
		meta["purpose_code"] = string(*n.PurposeCode)
	}
	for k, v := range outcomeMeta {
		meta[k] = v
	}
	id := n.ID
	return audit.Event{Action: string(authz.ConsentNoticeManage), Outcome: audit.Success,
		ResourceType: authz.TypeNoticeVersion, ResourceID: &id, Metadata: meta}.By(a)
}

// CreateNotice implements Service (adminCreateNotice).
func (s *service) CreateNotice(ctx context.Context, a authz.Actor, in NoticeDraft, proof ReauthProof) (Notice, error) {
	if err := authz.Can(ctx, a, authz.ConsentNoticeManage, authz.Resource{Type: authz.TypeNoticeVersion}); err != nil {
		return Notice{}, err
	}
	kind, err := s.reauth(ctx, a, authz.ConsentNoticeManage, authz.TypeNoticeVersion, nil, proof)
	if err != nil {
		return Notice{}, err
	}
	in, err = s.checkDraft(ctx, in)
	if err != nil {
		return Notice{}, err
	}
	hash, err := idempotency.Hash(in)
	if err != nil {
		return Notice{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Notice, error) {
		creator := a.UserID
		n := Notice{
			ID: uuid.NewV7(), Kind: in.Kind, PurposeCode: in.Purpose, Audience: in.Audience, Locale: in.Locale,
			Version: in.Version, Title: in.Title, BodyMD: in.BodyMD, BodySHA256: BodyDigest(in.BodyMD),
			CreatedBy: &creator, CreatedAt: s.now(),
		}
		if err := s.d.Store.InsertNotice(ctx, n); err != nil {
			return Notice{}, err
		}
		if err := s.d.Audit.Log(ctx, noticeEvent(a, n, map[string]any{"step": "create", audit.MetaReauth: kind})); err != nil {
			return Notice{}, err
		}
		return n, nil
	})
	if err != nil {
		return Notice{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "notice draft created", slog.String("notice_id", out.ID.String()),
		slog.String("kind", string(out.Kind)), slog.String("version", out.Version))
	return out, nil
}

// PublishNotice implements Service (adminPublishNotice): the current
// version of the same key is retired in the same transaction, then the
// draft is published. The notice becomes immutable. A terms of use version
// is announced to Deps.TermsWatch in the transaction (BE-124: the readers
// it applies to are told after the commit).
func (s *service) PublishNotice(ctx context.Context, a authz.Actor, id uuid.UUID, proof ReauthProof) (Notice, error) {
	return s.transition(ctx, a, id, proof, "publish", func(ctx context.Context, n Notice) (map[string]any, error) {
		if n.Status() != StatusDraft {
			return nil, ErrNoticeState
		}
		now := s.now()
		meta := map[string]any{}
		cur, found, err := s.d.Store.LockCurrent(ctx, n.Key())
		if err != nil {
			return nil, err
		}
		if found {
			if err := s.d.Store.Retire(ctx, cur.ID, now); err != nil {
				return nil, err
			}
			meta["retired_notice_id"] = cur.ID.String()
		}
		if err := s.d.Store.Publish(ctx, n.ID, now); err != nil {
			return nil, err
		}
		if n.Kind == KindTermsOfUse && s.d.TermsWatch != nil {
			s.d.TermsWatch.TermsPublished(ctx, n.ID)
		}
		return meta, nil
	})
}

// RetireNotice implements Service (adminRetireNotice).
func (s *service) RetireNotice(ctx context.Context, a authz.Actor, id uuid.UUID, proof ReauthProof) (Notice, error) {
	return s.transition(ctx, a, id, proof, "retire", func(ctx context.Context, n Notice) (map[string]any, error) {
		if n.Status() != StatusPublished {
			return nil, ErrNoticeState
		}
		return nil, s.d.Store.Retire(ctx, n.ID, s.now())
	})
}

// transition runs a notice state change: authz, reauth, then step on the
// locked row in one transaction with its audit row.
func (s *service) transition(ctx context.Context, a authz.Actor, id uuid.UUID, proof ReauthProof, name string,
	step func(ctx context.Context, n Notice) (map[string]any, error),
) (Notice, error) {
	// Admin surface: 403 before anything is read (03 §7.2-3).
	if err := authz.Can(ctx, a, authz.ConsentNoticeManage, authz.Resource{Type: authz.TypeNoticeVersion, ID: id}); err != nil {
		return Notice{}, err
	}
	if _, err := s.d.Store.Notice(ctx, id); err != nil {
		return Notice{}, err
	}
	kind, err := s.reauth(ctx, a, authz.ConsentNoticeManage, authz.TypeNoticeVersion, &id, proof)
	if err != nil {
		return Notice{}, err
	}
	hash, err := idempotency.Hash(struct {
		Step string
		ID   uuid.UUID
	}{name, id})
	if err != nil {
		return Notice{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Notice, error) {
		cur, err := s.d.Store.LockNotice(ctx, id)
		if err != nil {
			return Notice{}, err
		}
		meta, err := step(ctx, cur)
		if err != nil {
			return Notice{}, err
		}
		if meta == nil {
			meta = map[string]any{}
		}
		meta["step"], meta[audit.MetaReauth] = name, kind
		if err := s.d.Audit.Log(ctx, noticeEvent(a, cur, meta)); err != nil {
			return Notice{}, err
		}
		return s.d.Store.Notice(ctx, id)
	})
	if err != nil {
		return Notice{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "notice state changed", slog.String("step", name), slog.String("notice_id", id.String()),
		slog.String("kind", string(out.Kind)), slog.String("version", out.Version))
	return out, nil
}

// ListPurposes implements Service (adminListConsentPurposes).
func (s *service) ListPurposes(ctx context.Context, a authz.Actor) ([]PurposeDef, error) {
	if err := authz.Can(ctx, a, authz.ConsentPurposeManage, adminSurface); err != nil {
		return nil, err
	}
	return s.d.Store.Purposes(ctx)
}

// UpdatePurpose implements Service (adminUpdateConsentPurpose). Switching a
// purpose off stops every Checker answer for it at once; the ledger keeps
// its rows.
func (s *service) UpdatePurpose(ctx context.Context, a authz.Actor, code Purpose, p PurposePatch, proof ReauthProof) (PurposeDef, error) {
	if err := authz.Can(ctx, a, authz.ConsentPurposeManage, adminSurface); err != nil {
		return PurposeDef{}, err
	}
	kind, err := s.reauth(ctx, a, authz.ConsentPurposeManage, authz.TypeAdminSurface, nil, proof)
	if err != nil {
		return PurposeDef{}, err
	}
	if p.Active == nil && p.DescriptionTR == nil {
		return PurposeDef{}, ErrEmptyPatch
	}
	var desc string
	if p.DescriptionTR != nil {
		d, err := textx.NormalizeText(*p.DescriptionTR)
		switch {
		case err != nil:
			return PurposeDef{}, invalid(problem.ErrValidation, "body.description_tr", "must be printable text")
		case d == "":
			return PurposeDef{}, invalid(problem.ErrValidation, "body.description_tr", "must not be empty")
		case utf8.RuneCountInString(d) > maxDescriptionRunes:
			return PurposeDef{}, invalid(problem.ErrValidation, "body.description_tr", "must be at most 1000 characters")
		}
		desc = d
	}
	var out PurposeDef
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockPurpose(ctx, code)
		if errors.Is(err, ErrUnknownPurpose) {
			return ErrNotFound // a path parameter: an unknown code is simply not found
		}
		if err != nil {
			return err
		}
		meta := map[string]any{"purpose_code": string(code), audit.MetaReauth: kind}
		if p.Active != nil && *p.Active != cur.Active {
			cur.Active = *p.Active
			meta["active"] = cur.Active
		}
		if p.DescriptionTR != nil && desc != cur.DescriptionTR {
			cur.DescriptionTR = desc
			meta["description_changed"] = true
		}
		if err := s.d.Store.UpdatePurpose(ctx, cur); err != nil {
			return err
		}
		ev := audit.Event{Action: string(authz.ConsentPurposeManage), Outcome: audit.Success,
			ResourceType: "consent_purpose", Metadata: meta}.By(a)
		if err := s.d.Audit.Log(ctx, ev); err != nil {
			return err
		}
		out = cur
		return nil
	})
	if err != nil {
		return PurposeDef{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "consent purpose updated", slog.String("purpose", string(code)), slog.Bool("active", out.Active))
	return out, nil
}
