package consent

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register adds the consent operations (04 §2.5) to api. svc may be nil
// (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	tags := []string{"consent"}
	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "listCurrentNotices", Method: http.MethodGet, Path: "/notices/current", Tags: tags,
		Summary: "List the current notices and consent texts",
		Description: "The published version of every notice for the audience (falling back to audience all) in the " +
			"Accept-Language locale (falling back to tr-TR). Privacy notices are shown, never consented to.",
	}, op.Meta{Action: string(authz.ConsentNoticeRead), Surface: op.SurfacePublic}), h.listCurrent)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "getNoticeVersion", Method: http.MethodGet, Path: "/notices/{notice_version_id}", Tags: tags,
		Summary:     "Get a notice version",
		Description: "Retired versions stay readable (proof of earlier consents); drafts are visible to dpo only.",
	}, op.Meta{Action: string(authz.ConsentNoticeRead), Surface: op.SurfacePublic}), h.getNotice)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "recordMyConsent", Method: http.MethodPost, Path: "/me/consents", Tags: tags,
		Summary: "Grant or withdraw a consent", DefaultStatus: http.StatusCreated,
		Description: "Appends one act to the consent ledger. granted needs the current consent text of the purpose " +
			"(notice_version_id) and the SHA-256 of the shown body (text_sha256); withdrawn is always allowed. " +
			"Users under 18 cannot grant commercial, discovery, contact or sponsor purposes (422 consent.minor_restricted).",
	}, op.Meta{Action: string(authz.ConsentRecordCreate), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional}), h.recordMine)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "listMyConsents", Method: http.MethodGet, Path: "/me/consents", Tags: tags,
		Summary: "List my consents",
	}, op.Meta{Action: string(authz.ConsentRecordListOwn), Surface: op.SurfaceSelf}), h.listMine)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminListUserConsents", Method: http.MethodGet, Path: "/admin/users/{user_id}/consents", Tags: tags,
		Summary: "List the consent proof of a user",
		Description: "IP, channel and text digest of every consent act. support needs X-Koro-Access-Reason " +
			"(break-glass); every read is audited.",
	}, op.Meta{Action: string(authz.ConsentRecordListAdmin), Surface: op.SurfaceAdmin, BreakGlass: true}), h.adminListUser)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminCreateNotice", Method: http.MethodPost, Path: "/admin/notices", Tags: tags,
		Summary: "Create a notice draft", DefaultStatus: http.StatusCreated,
		Description: "A consent_text belongs to exactly one explicit-consent purpose; other kinds carry none.",
	}, op.Meta{Action: string(authz.ConsentNoticeManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional,
		Reauth: true, RateLimit: op.RateReauth}), h.createNotice)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminPublishNotice", Method: http.MethodPost, Path: "/admin/notices/{notice_version_id}/publish", Tags: tags,
		Summary:     "Publish a notice draft",
		Description: "The published version of the same kind, purpose, audience and locale is retired in the same step. A published version never changes.",
	}, op.Meta{Action: string(authz.ConsentNoticeManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional,
		Reauth: true, RateLimit: op.RateReauth}), h.publishNotice)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminRetireNotice", Method: http.MethodPost, Path: "/admin/notices/{notice_version_id}/retire", Tags: tags,
		Summary: "Retire a published notice",
	}, op.Meta{Action: string(authz.ConsentNoticeManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional,
		Reauth: true, RateLimit: op.RateReauth}), h.retireNotice)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminListConsentPurposes", Method: http.MethodGet, Path: "/admin/consent-purposes", Tags: tags,
		Summary: "List the consent purposes",
	}, op.Meta{Action: string(authz.ConsentPurposeManage), Surface: op.SurfaceAdmin}), h.listPurposes)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminUpdateConsentPurpose", Method: http.MethodPatch, Path: "/admin/consent-purposes/{purpose_code}", Tags: tags,
		Summary:     "Switch a consent purpose on or off, or edit its description",
		Description: "An inactive purpose is never in force for anyone; the ledger keeps its rows.",
	}, op.Meta{Action: string(authz.ConsentPurposeManage), Surface: op.SurfaceAdmin, Reauth: true, RateLimit: op.RateReauth}), h.updatePurpose)

	// One-click unsubscribe (BE-062): the token in the path is the
	// subject's credential (consent.record.create, O); cookies and bearer
	// tokens play no part. read_default/write_default (anonymous: per IP,
	// fail open), not token_confirm: mailbox providers post one-click
	// unsubscribes from shared server addresses and a limiter outage must
	// not turn the legal opt-out into a 503.
	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "getUnsubscribeLink", Method: http.MethodGet, Path: "/unsubscribe-links/{token}", Tags: tags,
		Summary: "Show what an unsubscribe link withdraws",
		Description: "The data of the unsubscribe confirmation page: the consent purpose of the link (commercial_email). " +
			"Nothing about the recipient or the current consent state; no side effect (mail scanners open links). " +
			"A malformed, tampered or expired link is 422 consent.invalid_unsubscribe_link, the same answer for every reason.",
	}, op.Meta{Action: string(authz.ConsentRecordCreate), Surface: op.SurfacePublic}), h.getUnsubscribe)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "confirmUnsubscribe", Method: http.MethodPost, Path: "/unsubscribe-links/{token}", Tags: tags,
		Summary: "Unsubscribe with one click (RFC 8058)", DefaultStatus: http.StatusNoContent,
		Description: "The List-Unsubscribe-Post target of commercial e-mail: the request body (List-Unsubscribe=One-Click, " +
			"form or multipart) and its content type are ignored. Withdraws the recipient's consent to the link's purpose " +
			"through the email_link channel and queues the İYS refusal (RET). Idempotent: 204 whether a consent was " +
			"withdrawn or none was in force. A malformed, tampered or expired link is 422 consent.invalid_unsubscribe_link.",
	}, op.Meta{Action: string(authz.ConsentRecordCreate), Surface: op.SurfacePublic, Idempotency: op.IdempotencyOptional}), h.confirmUnsubscribe)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "receiveIYSWebhook", Method: http.MethodPost, Path: "/webhooks/iys/{provider}/{token}", Tags: tags,
		Summary: "Receive an İYS integrator webhook",
		Description: "İYS's answers to Koro's consent uploads and the approvals and refusals recipients give İYS " +
			"itself, pushed by the configured İYS integrator (netgsm, BE-110). The identity is the secret token " +
			"of the URL (and the source address when CONSENT_IYS_NETGSM_WEBHOOK_ALLOWED_IPS is set): cookies and " +
			"bearer tokens are ignored. A wrong token or address is 401 and nothing is stored; a body that is not " +
			"the integrator's item list is 422; a stored delivery (a redelivery too) is 200, its items are applied " +
			"by consent.iys_status_sync. A provider that is not the configured, pushing one is 404.",
		DefaultStatus: http.StatusOK, MaxBodyBytes: maxIYSWebhookBody,
		// The raw body is parsed by the adapter, never by huma.
		SkipValidateBody: true,
	}, op.Meta{Action: string(authz.ConsentIysWebhookReceive), Surface: op.SurfaceWebhook,
		RateLimit: op.RateWebhook, Idempotency: op.IdempotencyOptional}), h.receiveIYSWebhook)
}

type handler struct{ svc Service }

// localeOf maps Accept-Language to a notice locale (04 §1.12): the first
// tag of a supported language wins, tr-TR otherwise.
func localeOf(acceptLanguage string) string {
	for tag := range strings.SplitSeq(acceptLanguage, ",") {
		lang, _, _ := strings.Cut(strings.TrimSpace(tag), ";")
		lang = strings.ToLower(strings.TrimSpace(lang))
		switch {
		case lang == "tr" || strings.HasPrefix(lang, "tr-"):
			return LocaleTR
		case lang == "en" || strings.HasPrefix(lang, "en-"):
			return LocaleEN
		}
	}
	return LocaleTR
}

// ---- listCurrentNotices

type listCurrentInput struct {
	Audience       string `query:"audience" required:"true" enum:"all,student,company,club,university" doc:"Who reads the notices; audience all is the fallback"`
	Kind           string `query:"kind" enum:"privacy_notice,consent_text,terms_of_use,cookie_notice"`
	PurposeCode    string `query:"purpose_code" pattern:"^[a-z][a-z0-9_]{2,40}$" doc:"Only the consent text of this purpose"`
	AcceptLanguage string `header:"Accept-Language" maxLength:"1000" doc:"tr or en; tr-TR otherwise"`
}

type listCurrentOutput struct {
	ContentLanguage string `header:"Content-Language"`
	Body            NoticeList
}

func (h handler) listCurrent(ctx context.Context, in *listCurrentInput) (*listCurrentOutput, error) {
	q := NoticeFilter{Audience: Audience(in.Audience), Locale: localeOf(in.AcceptLanguage)}
	if in.Kind != "" {
		k := NoticeKind(in.Kind)
		q.Kind = &k
	}
	if in.PurposeCode != "" {
		p := Purpose(in.PurposeCode)
		q.Purpose = &p
	}
	rows, err := h.svc.ListCurrentNotices(ctx, authz.ActorOrAnonymous(ctx), q)
	if err != nil {
		return nil, err
	}
	out := &listCurrentOutput{ContentLanguage: q.Locale, Body: NoticeList{Items: make([]NoticeDTO, len(rows))}}
	for i, n := range rows {
		d := toNoticeDTO(n.Notice)
		d.Collectable = n.Collectable
		if n.ScopeType != nil {
			d.ScopeType = string(*n.ScopeType)
		}
		out.Body.Items[i] = d
	}
	return out, nil
}

// ---- getNoticeVersion

type noticeIDInput struct {
	NoticeVersionID string `path:"notice_version_id" doc:"Notice version ID (an unknown or malformed ID is 404)"`
}

type noticeOutput struct {
	Body NoticeDTO
}

func (h handler) getNotice(ctx context.Context, in *noticeIDInput) (*noticeOutput, error) {
	id, err := uuid.Parse(in.NoticeVersionID)
	if err != nil {
		return nil, ErrNotFound // IDOR rule: an unknown ID is simply not found
	}
	n, err := h.svc.GetNotice(ctx, authz.ActorOrAnonymous(ctx), id)
	if err != nil {
		return nil, err
	}
	return &noticeOutput{Body: toNoticeDTO(n)}, nil
}

// ---- recordMyConsent

type recordMineInput struct {
	idempotency.OptionalHeader
	Client string `header:"X-Koro-Client" enum:"web,mobile" doc:"Client kind; recorded as the consent channel (default web)"`
	Body   struct {
		PurposeCode     string `json:"purpose_code" pattern:"^[a-z][a-z0-9_]{2,40}$"`
		Action          string `json:"action" enum:"granted,withdrawn"`
		NoticeVersionID string `json:"notice_version_id,omitempty" format:"uuid" doc:"The consent text shown (required to grant)"`
		TextSHA256      string `json:"text_sha256,omitempty" pattern:"^[0-9a-f]{64}$" doc:"body_sha256 of that text (required to grant)"`
		ScopeID         string `json:"scope_id,omitempty" format:"uuid" doc:"Event registration (per-registration purposes only)"`
	}
}

type consentOutput struct {
	Body ConsentDTO
}

func (h handler) recordMine(ctx context.Context, in *recordMineInput) (*consentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	mc := MyConsent{Purpose: Purpose(in.Body.PurposeCode), Action: Action(in.Body.Action), Channel: ChannelWeb}
	if in.Client == string(ChannelMobile) {
		mc.Channel = ChannelMobile
	}
	if mc.NoticeVersionID, err = httpx.OptID("body.notice_version_id", in.Body.NoticeVersionID); err != nil {
		return nil, err
	}
	if mc.ScopeID, err = httpx.OptID("body.scope_id", in.Body.ScopeID); err != nil {
		return nil, err
	}
	if in.Body.TextSHA256 != "" {
		if mc.TextSHA256, err = hex.DecodeString(in.Body.TextSHA256); err != nil {
			return nil, problem.Field("body.text_sha256", "must be hex")
		}
	}
	c, err := h.svc.RecordMine(ctx, a, mc)
	if err != nil {
		return nil, err
	}
	return &consentOutput{Body: toConsentDTO(c)}, nil
}

// ---- listMyConsents

type listMineInput struct {
	page.Params
	PurposeCode string `query:"purpose_code" pattern:"^[a-z][a-z0-9_]{2,40}$"`
}

type listMineOutput struct {
	Body MyConsentsDTO
}

func pageQuery(p page.Params, purpose, fp string) (PageQuery, error) {
	after, err := afterOf(p.Cursor, fp)
	if err != nil {
		return PageQuery{}, err
	}
	q := PageQuery{Limit: p.Limit, After: after}
	if purpose != "" {
		code := Purpose(purpose)
		q.Purpose = &code
	}
	return q, nil
}

func (h handler) listMine(ctx context.Context, in *listMineInput) (*listMineOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyConsents", in.PurposeCode)
	q, err := pageQuery(in.Params, in.PurposeCode, fp)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.ListMine(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &listMineOutput{Body: MyConsentsDTO{
		Current: make([]ConsentStateDTO, len(m.Current)), Items: make([]ConsentDTO, len(m.Page.Items)),
		NextCursor: cursorOf(fp, m.Page.Next),
	}}
	for i, c := range m.Current {
		out.Body.Current[i] = ConsentStateDTO{PurposeCode: string(c.PurposeCode), ScopeID: httpx.IDStr(c.ScopeID),
			State: string(c.Action), Since: c.OccurredAt}
	}
	for i, c := range m.Page.Items {
		out.Body.Items[i] = toConsentDTO(c)
	}
	return out, nil
}

// ---- adminListUserConsents

type adminListUserInput struct {
	UserID string `path:"user_id" doc:"User ID (a malformed ID is 404)"`
	page.Params
	PurposeCode string `query:"purpose_code" pattern:"^[a-z][a-z0-9_]{2,40}$"`
	httpx.AccessReasonHeader
	TicketRef string `header:"X-Koro-Ticket-Ref" maxLength:"100" doc:"Support ticket reference (optional)"`
}

type proofListOutput struct {
	Body ConsentProofList
}

func (h handler) adminListUser(ctx context.Context, in *adminListUserInput) (*proofListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	userID, err := uuid.Parse(in.UserID)
	if err != nil {
		return nil, ErrNotFound
	}
	fp := page.Fingerprint("adminListUserConsents", userID.String(), in.PurposeCode)
	q, err := pageQuery(in.Params, in.PurposeCode, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListUser(ctx, a, userID, Access{Reason: in.AccessReason.String(), TicketRef: in.TicketRef}, q)
	if err != nil {
		return nil, err
	}
	out := &proofListOutput{Body: ConsentProofList{Items: make([]ConsentProofDTO, len(p.Items)), NextCursor: cursorOf(fp, p.Next)}}
	for i, c := range p.Items {
		out.Body.Items[i] = toProofDTO(c)
	}
	return out, nil
}

// ---- adminCreateNotice

type createNoticeInput struct {
	idempotency.OptionalHeader
	Body struct {
		Kind        string     `json:"kind" enum:"privacy_notice,consent_text,terms_of_use,cookie_notice"`
		PurposeCode string     `json:"purpose_code,omitempty" pattern:"^[a-z][a-z0-9_]{2,40}$" doc:"Required for consent_text, forbidden otherwise"`
		Audience    string     `json:"audience" enum:"all,student,company,club,university"`
		Locale      string     `json:"locale" enum:"tr-TR,en-US"`
		Version     string     `json:"version" pattern:"^[0-9]{4}\\.[0-9]{2}\\.[0-9]{2}(-[0-9]+)?$" doc:"e.g. 2026.10.02 or 2026.10.02-2"`
		Title       string     `json:"title" minLength:"1" maxLength:"200"`
		BodyMD      string     `json:"body_md" minLength:"1" maxLength:"400000" doc:"Markdown; line breaks are stored as LF"`
		Reauth      *ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) createNotice(ctx context.Context, in *createNoticeInput) (*noticeOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	d := NoticeDraft{Kind: NoticeKind(in.Body.Kind), Audience: Audience(in.Body.Audience), Locale: in.Body.Locale,
		Version: in.Body.Version, Title: in.Body.Title, BodyMD: in.Body.BodyMD}
	if in.Body.PurposeCode != "" {
		p := Purpose(in.Body.PurposeCode)
		d.Purpose = &p
	}
	n, err := h.svc.CreateNotice(ctx, a, d, in.Body.Reauth.proof())
	if err != nil {
		return nil, err
	}
	return &noticeOutput{Body: toNoticeDTO(n)}, nil
}

// ---- adminPublishNotice, adminRetireNotice

type noticeTransitionInput struct {
	idempotency.OptionalHeader
	NoticeVersionID string `path:"notice_version_id" doc:"Notice version ID (an unknown or malformed ID is 404)"`
	Body            struct {
		Reauth *ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) publishNotice(ctx context.Context, in *noticeTransitionInput) (*noticeOutput, error) {
	return h.transition(ctx, in, h.svc.PublishNotice)
}

func (h handler) retireNotice(ctx context.Context, in *noticeTransitionInput) (*noticeOutput, error) {
	return h.transition(ctx, in, h.svc.RetireNotice)
}

func (h handler) transition(ctx context.Context, in *noticeTransitionInput,
	fn func(context.Context, authz.Actor, uuid.UUID, ReauthProof) (Notice, error),
) (*noticeOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.NoticeVersionID)
	if err != nil {
		return nil, ErrNotFound
	}
	n, err := fn(ctx, a, id, in.Body.Reauth.proof())
	if err != nil {
		return nil, err
	}
	return &noticeOutput{Body: toNoticeDTO(n)}, nil
}

// ---- adminListConsentPurposes

type purposeListOutput struct {
	Body PurposeList
}

func (h handler) listPurposes(ctx context.Context, _ *struct{}) (*purposeListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := h.svc.ListPurposes(ctx, a)
	if err != nil {
		return nil, err
	}
	out := &purposeListOutput{Body: PurposeList{Items: make([]PurposeDTO, len(rows))}}
	for i, p := range rows {
		out.Body.Items[i] = toPurposeDTO(p)
	}
	return out, nil
}

// ---- adminUpdateConsentPurpose

type updatePurposeInput struct {
	PurposeCode string `path:"purpose_code" doc:"Purpose code (an unknown code is 404)"`
	Body        struct {
		Active        *bool      `json:"active,omitempty" doc:"false switches the purpose off for everyone"`
		DescriptionTR *string    `json:"description_tr,omitempty" maxLength:"1000"`
		Reauth        *ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

type purposeOutput struct {
	Body PurposeDTO
}

func (h handler) updatePurpose(ctx context.Context, in *updatePurposeInput) (*purposeOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.UpdatePurpose(ctx, a, Purpose(in.PurposeCode),
		PurposePatch{Active: in.Body.Active, DescriptionTR: in.Body.DescriptionTR}, in.Body.Reauth.proof())
	if err != nil {
		return nil, err
	}
	return &purposeOutput{Body: toPurposeDTO(p)}, nil
}

// ---- getUnsubscribeLink, confirmUnsubscribe

type unsubscribeInput struct {
	// No schema bounds: a token of the wrong length gets the same
	// consent.invalid_unsubscribe_link as every other unusable token.
	Token string `path:"token" doc:"The signed token of the unsubscribe link (base64url, at most 200 characters)"`
}

type unsubscribeOutput struct {
	Body UnsubscribeLinkDTO
}

func (h handler) getUnsubscribe(ctx context.Context, in *unsubscribeInput) (*unsubscribeOutput, error) {
	l, err := h.svc.GetUnsubscribe(ctx, in.Token)
	if err != nil {
		return nil, err
	}
	return &unsubscribeOutput{Body: UnsubscribeLinkDTO{PurposeCode: string(l.Purpose)}}, nil
}

func (h handler) confirmUnsubscribe(ctx context.Context, in *unsubscribeInput) (*struct{}, error) {
	return nil, h.svc.Unsubscribe(ctx, in.Token)
}

// ---- receiveIYSWebhook

// maxIYSWebhookBody bounds one delivery (Netgsm: a list of items).
const maxIYSWebhookBody = 1 << 20

type iysWebhookInput struct {
	Provider string `path:"provider" maxLength:"32" doc:"The İYS integrator (CONSENT_IYS_PROVIDER): netgsm"`
	// No schema bounds: a token of the wrong length gets the same 401 as
	// every other wrong token.
	Token   string `path:"token" doc:"The secret of the webhook URL (CONSENT_IYS_NETGSM_WEBHOOK_TOKEN)"`
	RawBody []byte `contentType:"application/json"`
}

func (h handler) receiveIYSWebhook(ctx context.Context, in *iysWebhookInput) (*struct{}, error) {
	return nil, h.svc.ReceiveIYSWebhook(ctx, in.Provider, in.Token, in.RawBody)
}
