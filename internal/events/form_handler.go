package events

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/problem"
)

// registerForms adds the 7 form and lint rule operations of BE-026 (04
// §2.6).
func registerForms(api huma.API, h handler) {
	op.Register(api, "events", huma.Operation{
		OperationID: "getEventForm", Method: http.MethodGet, Path: "/events/{event_id}/form",
		Summary: "Get the registration form of an event",
		Description: "The current published version (404 when the event has none or is not visible). answers_schema is " +
			"the JSON Schema 2020-12 document the server validates the answers with (plus phone, URL host and date " +
			"range rules). A minor does not get the sponsor-sharing consent field. consent_notices are the current " +
			"texts of the consent fields (Accept-Language: tr or en).",
	}, op.Meta{Action: string(authz.EventsFormRead), Surface: op.SurfacePublic}, h.getForm)

	op.Register(api, "events", huma.Operation{
		OperationID: "getEventFormDraft", Method: http.MethodGet, Path: "/events/{event_id}/form/draft",
		Summary: "Get the form draft",
		Description: "The draft schema with a KVKK lint preview over the active rules. The ETag is the If-Match value " +
			"of putEventFormDraft.",
	}, op.Meta{Action: string(authz.EventsFormManage), Surface: op.SurfaceResource}, h.getFormDraft)

	op.Register(api, "events", huma.Operation{
		OperationID: "putEventFormDraft", Method: http.MethodPut, Path: "/events/{event_id}/form/draft",
		Summary: "Replace the form draft",
		Description: "If-Match with the ETag of getEventFormDraft is required: missing → 428, stale → 412. At most 20 " +
			"fields; keys match ^[a-z][a-z0-9_]{1,40}$; help and description are HTML cleaned by the server's " +
			"allow-list. Every save returns the lint preview. Cancelled and completed events: 409.",
	}, op.Meta{Action: string(authz.EventsFormManage), Surface: op.SurfaceResource}, h.putFormDraft)

	op.Register(api, "events", huma.Operation{
		OperationID: "publishEventForm", Method: http.MethodPost, Path: "/events/{event_id}/form/versions",
		DefaultStatus: http.StatusCreated,
		Summary:       "Publish the form draft as a new version",
		Description: "The draft becomes the next immutable version and the current form. A blocked field: 422 " +
			"events.form_blocked_field (no override). A warn_special or review_identifier field needs " +
			"lint_override_reason (422 events.lint_override_required). A draft equal to the current version: 409 " +
			"events.form_unchanged. An earlier key with another type, or a new required field after the first " +
			"registration: 422 events.form_incompatible. Consent fields need a published consent text.",
	}, op.Meta{Action: string(authz.EventsFormPublish), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional},
		h.publishForm)

	op.Register(api, "events", huma.Operation{
		OperationID: "adminListFormLintRules", Method: http.MethodGet, Path: "/admin/form-lint-rules",
		Summary: "List the form lint rules", Description: "Active and retired rules, blocked first.",
	}, op.Meta{Action: string(authz.EventsLintRuleManage), Surface: op.SurfaceAdmin}, h.listLintRules)

	op.Register(api, "events", huma.Operation{
		OperationID: "adminCreateFormLintRule", Method: http.MethodPost, Path: "/admin/form-lint-rules",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create a form lint rule",
		Description: "pattern is an RE2 expression matched on the lower-case, accent-free text of a field (\"saglik\"); " +
			"a pattern that does not compile or matches the empty text is 422.",
	}, op.Meta{Action: string(authz.EventsLintRuleManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional},
		h.createLintRule)

	op.Register(api, "events", huma.Operation{
		OperationID: "adminUpdateFormLintRule", Method: http.MethodPatch, Path: "/admin/form-lint-rules/{rule_id}",
		Summary: "Update a form lint rule", Description: "Rules are never deleted: active=false retires one.",
	}, op.Meta{Action: string(authz.EventsLintRuleManage), Surface: op.SurfaceAdmin}, h.updateLintRule)
}

// ---- DTOs

// FormLintHitDTO is one lint finding: a rule matching a field.
type FormLintHitDTO struct {
	FieldKey      string `json:"field_key" doc:"Key of the field; empty for the form description"`
	RuleID        string `json:"rule_id" format:"uuid"`
	DescriptionTR string `json:"description_tr"`
}

// FormLintReportDTO is a lint report (02 §11.4 lint_report).
type FormLintReportDTO struct {
	Blocked          []FormLintHitDTO `json:"blocked" doc:"Cannot be published"`
	WarnSpecial      []FormLintHitDTO `json:"warn_special" doc:"Needs lint_override_reason; answers are stored encrypted"`
	ReviewIdentifier []FormLintHitDTO `json:"review_identifier" doc:"Needs lint_override_reason"`
}

func toLintDTO(r LintReport) FormLintReportDTO {
	conv := func(hs []LintHit) []FormLintHitDTO {
		out := make([]FormLintHitDTO, len(hs))
		for i, h := range hs {
			out[i] = FormLintHitDTO{FieldKey: h.FieldKey, RuleID: h.RuleID.String(), DescriptionTR: h.DescriptionTR}
		}
		return out
	}
	return FormLintReportDTO{Blocked: conv(r.Blocked), WarnSpecial: conv(r.WarnSpecial), ReviewIdentifier: conv(r.ReviewIdentifier)}
}

// EventFormDTO is getEventForm (04 §2.6, §E.1).
type EventFormDTO struct {
	FormVersionID  string              `json:"form_version_id" format:"uuid"`
	VersionNo      int                 `json:"version_no"`
	Schema         FormSchema          `json:"schema"`
	AnswersSchema  map[string]any      `json:"answers_schema" doc:"JSON Schema 2020-12 of the answers object"`
	ConsentNotices []consent.NoticeDTO `json:"consent_notices"`
}

func toFormDTO(v FormView) EventFormDTO {
	out := EventFormDTO{
		FormVersionID: v.Version.ID.String(), VersionNo: v.Version.VersionNo, Schema: v.Schema,
		AnswersSchema: v.Schema.AnswersSchema(), ConsentNotices: make([]consent.NoticeDTO, len(v.Notices)),
	}
	for i, n := range v.Notices {
		out.ConsentNotices[i] = noticeDTO(n)
	}
	return out
}

// noticeDTO is consent's NoticeDTO of a current consent text (the shape of
// listCurrentNotices).
func noticeDTO(n consent.Notice) consent.NoticeDTO {
	d := consent.NoticeDTO{
		NoticeVersionID: n.ID.String(), Kind: string(n.Kind), Audience: string(n.Audience), Locale: n.Locale,
		Version: n.Version, Title: n.Title, BodyMD: n.BodyMD, BodySHA256: hex.EncodeToString(n.BodySHA256),
		Status: string(n.Status()), PublishedAt: n.PublishedAt, RetiredAt: n.RetiredAt,
	}
	if n.PurposeCode != nil {
		d.PurposeCode = string(*n.PurposeCode)
	}
	return d
}

// EventFormDraftDTO is getEventFormDraft / putEventFormDraft.
type EventFormDraftDTO struct {
	DraftSchema      FormSchema        `json:"draft_schema"`
	LintReport       FormLintReportDTO `json:"lint_report" doc:"Preview with the active rules"`
	CurrentVersionNo *int              `json:"current_version_no,omitempty" doc:"Absent before the first publication"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

func toDraftDTO(v FormDraftView) EventFormDraftDTO {
	return EventFormDraftDTO{DraftSchema: v.Draft, LintReport: toLintDTO(v.Lint), CurrentVersionNo: v.CurrentVersionNo,
		UpdatedAt: v.Form.UpdatedAt}
}

// EventFormVersionDTO is publishEventForm.
type EventFormVersionDTO struct {
	FormVersionID      string            `json:"form_version_id" format:"uuid"`
	VersionNo          int               `json:"version_no"`
	LintReport         FormLintReportDTO `json:"lint_report"`
	HasSensitiveFields bool              `json:"has_sensitive_fields" doc:"warn_special fields: answers are stored encrypted"`
	PublishedAt        time.Time         `json:"published_at"`
}

// FormLintRuleDTO is a form lint rule (02 §11.5).
type FormLintRuleDTO struct {
	RuleID        string    `json:"rule_id" format:"uuid"`
	Class         string    `json:"class" enum:"blocked,warn_special,review_identifier"`
	Pattern       string    `json:"pattern" doc:"RE2 on the lower-case, accent-free text"`
	DescriptionTR string    `json:"description_tr"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// FormLintRuleList is adminListFormLintRules.
type FormLintRuleList struct {
	Items []FormLintRuleDTO `json:"items"`
}

func toRuleDTO(r LintRule) FormLintRuleDTO {
	return FormLintRuleDTO{RuleID: r.ID.String(), Class: string(r.Class), Pattern: r.Pattern, DescriptionTR: r.DescriptionTR,
		Active: r.Active, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// ---- getEventForm

type getFormInput struct {
	EventPath
	AcceptLanguage string `header:"Accept-Language" maxLength:"1000" doc:"tr or en; tr-TR otherwise"`
}

type formOutput struct {
	Body EventFormDTO
}

// noticeLocale maps Accept-Language to a notice locale (04 §1.12).
func noticeLocale(acceptLanguage string) string {
	for tag := range strings.SplitSeq(acceptLanguage, ",") {
		lang, _, _ := strings.Cut(strings.TrimSpace(tag), ";")
		lang = strings.ToLower(strings.TrimSpace(lang))
		switch {
		case lang == "tr" || strings.HasPrefix(lang, "tr-"):
			return consent.LocaleTR
		case lang == "en" || strings.HasPrefix(lang, "en-"):
			return consent.LocaleEN
		}
	}
	return consent.LocaleTR
}

func (h handler) getForm(ctx context.Context, in *getFormInput) (*formOutput, error) {
	v, err := h.svc.GetForm(ctx, authz.ActorOrAnonymous(ctx), in.id(), noticeLocale(in.AcceptLanguage))
	if err != nil {
		return nil, err
	}
	return &formOutput{Body: toFormDTO(v)}, nil
}

// ---- getEventFormDraft / putEventFormDraft

type draftOutput struct {
	ETag string `header:"ETag"`
	Body EventFormDraftDTO
}

func (h handler) getFormDraft(ctx context.Context, in *EventPath) (*draftOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetFormDraft(ctx, a, in.id())
	if err != nil {
		return nil, err
	}
	return &draftOutput{ETag: v.ETag, Body: toDraftDTO(v)}, nil
}

type putFormDraftInput struct {
	EventPath
	IfMatch string `header:"If-Match" doc:"ETag of getEventFormDraft (required: missing → 428, stale → 412)"`
	Body    struct {
		DraftSchema FormSchema `json:"draft_schema"`
	}
}

func (h handler) putFormDraft(ctx context.Context, in *putFormDraftInput) (*draftOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(in.Body.DraftSchema)
	if err != nil {
		return nil, problem.Field("body.draft_schema", "is not a form schema")
	}
	v, err := h.svc.PutFormDraft(ctx, a, in.id(), in.IfMatch, raw)
	if err != nil {
		return nil, err
	}
	return &draftOutput{ETag: v.ETag, Body: toDraftDTO(v)}, nil
}

// ---- publishEventForm

type publishFormInput struct {
	EventPath
	Body *struct {
		LintOverrideReason string `json:"lint_override_reason,omitempty" maxLength:"500" doc:"10-500 characters; required when the lint finds warn_special or review_identifier fields"`
	}
}

type versionOutput struct {
	Body EventFormVersionDTO
}

func (h handler) publishForm(ctx context.Context, in *publishFormInput) (*versionOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var reason string
	if in.Body != nil {
		reason = in.Body.LintOverrideReason
	}
	v, err := h.svc.PublishForm(ctx, a, in.id(), reason)
	if err != nil {
		return nil, err
	}
	return &versionOutput{Body: EventFormVersionDTO{
		FormVersionID: v.Version.ID.String(), VersionNo: v.Version.VersionNo, LintReport: toLintDTO(v.Lint),
		HasSensitiveFields: v.Version.HasSensitiveFields, PublishedAt: v.Version.PublishedAt,
	}}, nil
}

// ---- lint rules

type ruleListOutput struct {
	Body FormLintRuleList
}

func (h handler) listLintRules(ctx context.Context, _ *struct{}) (*ruleListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	rs, err := h.svc.ListLintRules(ctx, a)
	if err != nil {
		return nil, err
	}
	out := &ruleListOutput{Body: FormLintRuleList{Items: make([]FormLintRuleDTO, len(rs))}}
	for i, r := range rs {
		out.Body.Items[i] = toRuleDTO(r)
	}
	return out, nil
}

type ruleOutput struct {
	Body FormLintRuleDTO
}

type createRuleInput struct {
	Body struct {
		Class         string `json:"class" enum:"blocked,warn_special,review_identifier"`
		Pattern       string `json:"pattern" minLength:"1" maxLength:"500" doc:"RE2 expression on the lower-case, accent-free text"`
		DescriptionTR string `json:"description_tr" minLength:"3" maxLength:"500"`
	}
}

func (h handler) createLintRule(ctx context.Context, in *createRuleInput) (*ruleOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.CreateLintRule(ctx, a, LintRuleInput{
		Class: LintClass(in.Body.Class), Pattern: in.Body.Pattern, DescriptionTR: in.Body.DescriptionTR,
	})
	if err != nil {
		return nil, err
	}
	return &ruleOutput{Body: toRuleDTO(r)}, nil
}

type updateRuleInput struct {
	RuleID string `path:"rule_id" doc:"Lint rule ID"`
	Body   struct {
		Class         *string `json:"class,omitempty" enum:"blocked,warn_special,review_identifier"`
		Pattern       *string `json:"pattern,omitempty" minLength:"1" maxLength:"500"`
		DescriptionTR *string `json:"description_tr,omitempty" minLength:"3" maxLength:"500"`
		Active        *bool   `json:"active,omitempty" doc:"false retires the rule"`
	}
}

func (h handler) updateLintRule(ctx context.Context, in *updateRuleInput) (*ruleOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	p := LintRulePatch{Pattern: in.Body.Pattern, DescriptionTR: in.Body.DescriptionTR, Active: in.Body.Active}
	if in.Body.Class != nil {
		p.Class = ptr(LintClass(*in.Body.Class))
	}
	r, err := h.svc.UpdateLintRule(ctx, a, httpx.ParseID(in.RuleID), p)
	if err != nil {
		return nil, err
	}
	return &ruleOutput{Body: toRuleDTO(r)}, nil
}
