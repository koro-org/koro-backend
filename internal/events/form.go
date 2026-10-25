package events

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"uuid"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// Audit resource types of the form writes.
const (
	formAggType    = "event_form"
	versionAggType = "event_form_version"
)

// Limits of lint_override_reason (event_form_versions_lint_override_reason_chk).
const (
	minOverrideReason = 10
	maxOverrideReason = 500
)

// FormView is the current form of an event as one reader answers it
// (getEventForm): the version, its schema without the fields the reader
// may not see (ForReader) and the consent texts of its consent fields.
type FormView struct {
	Version FormVersion
	Schema  FormSchema
	Notices []consent.Notice
}

// FormDraftView is the draft of an event's form with its lint preview
// (getEventFormDraft, putEventFormDraft). CurrentVersionNo is nil before
// the first publication.
type FormDraftView struct {
	Form             Form
	Draft            FormSchema
	Lint             LintReport
	CurrentVersionNo *int
	ETag             string
}

// FormVersionView is a version just published with its lint report.
type FormVersionView struct {
	Version FormVersion
	Lint    LintReport
}

// GetForm implements Service: the event (404 when missing or invisible) →
// authz on its form → the current version (404 when none: no published
// form). A minor does not get the minor-restricted consent fields.
func (s *service) GetForm(ctx context.Context, a authz.Actor, eventID uuid.UUID, locale string) (FormView, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return FormView{}, err
	}
	if err := s.authorize(ctx, a, authz.EventsFormRead, e); err != nil {
		return FormView{}, err
	}
	f, err := s.d.Store.GetForm(ctx, e.OrgID, e.ID)
	if err != nil {
		return FormView{}, err
	}
	if f.CurrentVersionID == nil {
		return FormView{}, ErrNotFound
	}
	v, err := s.d.Store.GetFormVersion(ctx, e.OrgID, *f.CurrentVersionID)
	if err != nil {
		return FormView{}, err
	}
	schema, err := DecodeFormSchema(v.Schema)
	if err != nil {
		return FormView{}, err
	}
	schema = schema.ForReader(a.IsUser() && a.IsMinor)
	notices, err := s.consentNotices(ctx, schema, locale)
	if err != nil {
		return FormView{}, err
	}
	return FormView{Version: v, Schema: schema, Notices: notices}, nil
}

// noticeKey is the consent text of a purpose for the form's readers
// (registrants are students; consent falls back to audience "all").
func noticeKey(p consent.Purpose, locale string) consent.NoticeKey {
	if locale != consent.LocaleEN {
		locale = consent.LocaleTR
	}
	return consent.NoticeKey{Kind: consent.KindConsentText, Purpose: &p, Audience: consent.AudienceStudent, Locale: locale}
}

// consentNotices returns the current consent texts of the schema's consent
// fields. A text retired without a successor is left out (the field can
// then not be granted; BE-028 refuses a consent without its text).
func (s *service) consentNotices(ctx context.Context, schema FormSchema, locale string) ([]consent.Notice, error) {
	out := []consent.Notice{}
	if s.d.Notices == nil {
		return out, nil
	}
	for _, p := range schema.ConsentPurposes() {
		n, err := s.d.Notices.Current(ctx, noticeKey(p, locale))
		switch {
		case errors.Is(err, consent.ErrNoNotice):
			s.d.Log.WarnContext(ctx, "events: consent field without a current consent text", slog.String("purpose", string(p)))
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// GetFormDraft implements Service (events.form.manage): the draft, its
// lint preview with the active rules and the current version number.
func (s *service) GetFormDraft(ctx context.Context, a authz.Actor, eventID uuid.UUID) (FormDraftView, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return FormDraftView{}, err
	}
	if err := s.authorize(ctx, a, authz.EventsFormManage, e); err != nil {
		return FormDraftView{}, err
	}
	f, err := s.d.Store.GetForm(ctx, e.OrgID, e.ID)
	if err != nil {
		return FormDraftView{}, err
	}
	return s.draftView(ctx, e, f)
}

func (s *service) draftView(ctx context.Context, e Event, f Form) (FormDraftView, error) {
	draft, err := DecodeFormSchema(f.DraftSchema)
	if err != nil {
		return FormDraftView{}, err
	}
	rep, err := s.lint(ctx, draft)
	if err != nil {
		return FormDraftView{}, err
	}
	out := FormDraftView{Form: f, Draft: draft, Lint: rep, ETag: httpx.ETag(f.UpdatedAt)}
	if f.CurrentVersionID != nil {
		v, err := s.d.Store.GetFormVersion(ctx, e.OrgID, *f.CurrentVersionID)
		if err != nil {
			return FormDraftView{}, err
		}
		out.CurrentVersionNo = &v.VersionNo
	}
	return out, nil
}

// lint runs the active rules over a schema.
func (s *service) lint(ctx context.Context, schema FormSchema) (LintReport, error) {
	rules, err := s.d.Store.ListLintRules(ctx)
	if err != nil {
		return LintReport{}, err
	}
	compiled, err := compileRules(rules)
	if err != nil {
		return LintReport{}, err
	}
	return lintSchema(schema, compiled), nil
}

// formWritable: the form of a draft or published event changes; a
// cancelled or completed event's is final (409).
func formWritable(e Event) error {
	if e.Status != StatusDraft && e.Status != StatusPublished {
		return ErrInvalidState
	}
	return nil
}

// PutFormDraft implements Service. Order: the event (404) → authz on its
// form (404/403) → the org status (403) → the event status (409) →
// If-Match on the form row (428/412) → the schema (422) → the draft and
// its audit record in one transaction, the lint preview in the answer.
func (s *service) PutFormDraft(ctx context.Context, a authz.Actor, eventID uuid.UUID, ifMatch string, draft []byte) (FormDraftView, error) {
	var out FormDraftView
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		e, _, err := s.locked(ctx, a, authz.EventsFormManage, eventID)
		if err != nil {
			return err
		}
		if err := formWritable(e); err != nil {
			return err
		}
		f, err := s.d.Store.LockForm(ctx, e.OrgID, e.ID)
		if err != nil {
			return err
		}
		if err := httpx.CheckIfMatch(ifMatch, httpx.ETag(f.UpdatedAt)); err != nil {
			return err
		}
		schema, err := ParseFormSchema(draft, "body.draft_schema")
		if err != nil {
			return err
		}
		if f, err = s.d.Store.UpdateFormDraft(ctx, e.OrgID, f.ID, JSON(schema.Canonical()), a.UserID); err != nil {
			return err
		}
		if out, err = s.draftView(ctx, e, f); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, auditEvent(a, authz.EventsFormManage, formAggType, f.ID, e.OrgID, "", map[string]any{
			"event_id": e.ID.String(), "field_count": len(schema.Fields), "lint": lintCounts(out.Lint),
		}))
	})
	if err != nil {
		return FormDraftView{}, err
	}
	return out, nil
}

func lintCounts(r LintReport) map[string]int {
	return map[string]int{"blocked": len(r.Blocked), "warn_special": len(r.WarnSpecial), "review_identifier": len(r.ReviewIdentifier)}
}

// PublishForm implements Service. In one transaction under the form row
// lock: the draft is checked again with the rules of today (422), a draft
// equal to the current version is refused (409 events.form_unchanged),
// consent fields need their current consent text (422), the lint decides
// (blocked: 422 events.form_blocked_field, no override; warn_special or
// review_identifier without a reason: 422 events.lint_override_required),
// earlier keys keep their type and no required field appears after the
// first registration (422 events.form_incompatible); then the version is
// written with version_no + 1 and the form points at it.
func (s *service) PublishForm(ctx context.Context, a authz.Actor, eventID uuid.UUID, overrideReason string) (FormVersionView, error) {
	reason := strings.TrimSpace(overrideReason)
	if reason != "" {
		r, err := cleanText("body.lint_override_reason", reason, minOverrideReason, maxOverrideReason)
		if err != nil {
			return FormVersionView{}, err
		}
		reason = r
	}
	hash, err := idempotency.Hash(struct {
		Op     string
		Event  uuid.UUID
		Reason string
	}{"publish_form", eventID, reason})
	if err != nil {
		return FormVersionView{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (FormVersionView, error) {
		return s.publishForm(ctx, a, eventID, reason)
	})
	return out, err
}

func (s *service) publishForm(ctx context.Context, a authz.Actor, eventID uuid.UUID, reason string) (FormVersionView, error) {
	e, _, err := s.locked(ctx, a, authz.EventsFormPublish, eventID)
	if err != nil {
		return FormVersionView{}, err
	}
	if err := formWritable(e); err != nil {
		return FormVersionView{}, err
	}
	f, err := s.d.Store.LockForm(ctx, e.OrgID, e.ID)
	if err != nil {
		return FormVersionView{}, err
	}
	schema, err := ParseFormSchema(f.DraftSchema, "draft_schema")
	if err != nil {
		return FormVersionView{}, err
	}
	versions, err := s.d.Store.ListFormVersions(ctx, e.OrgID, f.ID)
	if err != nil {
		return FormVersionView{}, err
	}
	sha := schema.SHA256()
	if n := len(versions); n > 0 && f.CurrentVersionID != nil && versions[n-1].ID == *f.CurrentVersionID &&
		bytes.Equal(versions[n-1].SchemaSHA256, sha) {
		return FormVersionView{}, ErrFormUnchanged
	}
	if err := s.checkConsentTexts(ctx, schema); err != nil {
		return FormVersionView{}, err
	}
	rep, err := s.lint(ctx, schema)
	if err != nil {
		return FormVersionView{}, err
	}
	if len(rep.Blocked) > 0 {
		return FormVersionView{}, ErrFormBlockedField.WithFields(hitFields(schema, rep.Blocked, "is blocked by the KVKK form lint (rule ")...)
	}
	if rep.NeedsOverride() && reason == "" {
		return FormVersionView{}, ErrLintOverrideRequired.WithFields(problem.FieldError{
			Location: "body.lint_override_reason", Message: "is required: the form asks for special-category or identifier data",
		})
	}
	registered, err := s.d.Store.HasRegistrations(ctx, e.OrgID, e.ID)
	if err != nil {
		return FormVersionView{}, err
	}
	if errs, err := compatibility(versions, schema, registered); err != nil {
		return FormVersionView{}, err
	} else if len(errs) > 0 {
		return FormVersionView{}, ErrFormIncompatible.WithFields(errs...)
	}
	nv := FormVersion{
		ID: uuid.NewV7(), FormID: f.ID, Schema: JSON(schema.Canonical()), SchemaSHA256: sha, FieldCount: len(schema.Fields),
		HasSensitiveFields: len(rep.WarnSpecial) > 0, LintReport: rep.JSON(), PublishedBy: a.UserID, PublishedAt: s.now(),
	}
	auditReason := ""
	if rep.NeedsOverride() {
		nv.LintOverrideBy, nv.LintOverrideReason, auditReason = &a.UserID, &reason, reason
	}
	stored, err := s.d.Store.InsertFormVersion(ctx, e.OrgID, nv)
	if err != nil {
		return FormVersionView{}, err
	}
	if _, err := s.d.Store.SetCurrentVersion(ctx, e.OrgID, f.ID, stored.ID); err != nil {
		return FormVersionView{}, err
	}
	err = s.d.Audit.Log(ctx, auditEvent(a, authz.EventsFormPublish, versionAggType, stored.ID, e.OrgID, auditReason,
		map[string]any{
			"event_id": e.ID.String(), "version_no": stored.VersionNo, "field_count": stored.FieldCount,
			"has_sensitive_fields": stored.HasSensitiveFields, "lint": lintCounts(rep), "override": nv.LintOverrideBy != nil,
		}))
	if err != nil {
		return FormVersionView{}, err
	}
	return FormVersionView{Version: stored, Lint: rep}, nil
}

// hitFields are the field errors of lint hits: the field's place in the
// draft and the rule (never the text).
func hitFields(schema FormSchema, hits []LintHit, msg string) []problem.FieldError {
	out := make([]problem.FieldError, 0, len(hits))
	for _, h := range hits {
		loc := "draft_schema.description"
		for i, f := range schema.Fields {
			if f.Key == h.FieldKey && h.FieldKey != "" {
				loc = "draft_schema.fields[" + strconv.Itoa(i) + "]"
			}
		}
		out = append(out, problem.FieldError{Location: loc, Message: msg + h.RuleID.String() + ")"})
	}
	return out
}

// checkConsentTexts: every consent field needs the current consent text of
// its purpose (the registrant agrees to that text, 02 §4.2).
func (s *service) checkConsentTexts(ctx context.Context, schema FormSchema) error {
	purposes := schema.ConsentPurposes()
	if len(purposes) == 0 {
		return nil
	}
	if s.d.Notices == nil {
		return problem.ErrUnavailable
	}
	var errs []problem.FieldError
	for i, f := range schema.Fields {
		if f.Type != FieldConsent {
			continue
		}
		_, err := s.d.Notices.Current(ctx, noticeKey(consent.Purpose(f.ConsentPurpose), consent.LocaleTR))
		switch {
		case errors.Is(err, consent.ErrNoNotice):
			errs = append(errs, problem.FieldError{Location: "draft_schema.fields[" + strconv.Itoa(i) + "].consent_purpose",
				Message: "has no published consent text"})
		case err != nil:
			return err
		}
	}
	if len(errs) > 0 {
		return problem.ErrValidation.WithFields(errs...)
	}
	return nil
}

// compatibility is R-003 §2.5.2 between the published versions and the
// draft: a key keeps the type it had in any earlier version (answers and
// export columns are per key), and after the first registration a field
// that is new or was optional in the current version cannot be required.
func compatibility(versions []FormVersion, next FormSchema, registered bool) ([]problem.FieldError, error) {
	types := map[string]FieldType{}
	var current FormSchema
	for _, v := range versions {
		sc, err := DecodeFormSchema(v.Schema)
		if err != nil {
			return nil, err
		}
		for _, f := range sc.Fields {
			types[f.Key] = f.Type
		}
		current = sc
	}
	var errs []problem.FieldError
	for i, f := range next.Fields {
		loc := "draft_schema.fields[" + strconv.Itoa(i) + "]"
		if t, ok := types[f.Key]; ok && t != f.Type {
			errs = append(errs, problem.FieldError{Location: loc + ".type",
				Message: "changes the type of a field published earlier; use a new key"})
		}
		if registered && f.Required {
			if cf, ok := current.Field(f.Key); !ok || !cf.Required {
				errs = append(errs, problem.FieldError{Location: loc + ".required",
					Message: "cannot become required after the first registration"})
			}
		}
	}
	return errs, nil
}
