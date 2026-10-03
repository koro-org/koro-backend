package notify

import (
	"context"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
)

// Register adds the notify operations (04 §2.19, §2.0) to api. svc may be
// nil (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}
	tags := []string{"notify"}

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "receiveEmailWebhook", Method: http.MethodPost, Path: "/webhooks/email/{provider}", Tags: tags,
		Summary: "Receive an e-mail provider webhook", DefaultStatus: http.StatusNoContent,
		Description: "Bounce, complaint and delivery notifications of the e-mail provider (ses: SNS-signed SES " +
			"notifications; resend: svix-signed events). The identity is the signature: cookies and bearer tokens are " +
			"ignored. A delivery that does not verify is 401; a verified one is stored in the webhook inbox under the " +
			"provider's event ID (a redelivery is a no-op, also 204) and processed asynchronously.",
		// The raw body is verified by its signature, never parsed by huma
		// (SNS posts text/plain; the bytes must stay exactly as signed).
		SkipValidateBody: true,
	}, op.Meta{Action: string(authz.NotifyProviderWebhookReceive), Surface: op.SurfaceWebhook,
		RateLimit: op.RateWebhook, Idempotency: op.IdempotencyOptional}), h.receiveWebhook)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "listMyNotifications", Method: http.MethodGet, Path: "/me/notifications", Tags: tags,
		Summary: "List my in-app notifications",
		Description: "Newest first. Texts are fixed per notification kind and carry no personal data; data holds " +
			"opaque IDs only.",
	}, op.Meta{Action: string(authz.NotifyInAppListOwn), Surface: op.SurfaceSelf}), h.listMine)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "markMyNotificationRead", Method: http.MethodPost, Path: "/me/notifications/{notification_id}/read",
		Tags: tags, Summary: "Mark one of my notifications read", DefaultStatus: http.StatusNoContent,
	}, op.Meta{Action: string(authz.NotifyInAppMarkRead), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional}), h.markRead)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "markAllMyNotificationsRead", Method: http.MethodPost, Path: "/me/notifications/read-all", Tags: tags,
		Summary: "Mark all my notifications read", DefaultStatus: http.StatusNoContent,
	}, op.Meta{Action: string(authz.NotifyInAppMarkRead), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional}), h.markAllRead)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "getMyNotificationPreferences", Method: http.MethodGet, Path: "/me/notification-preferences", Tags: tags,
		Summary:     "Get my notification preferences",
		Description: "Every category × channel pair, defaults included. Commercial messages are governed by consents, not here.",
	}, op.Meta{Action: string(authz.NotifyPreferenceReadOwn), Surface: op.SurfaceSelf}), h.getPreferences)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "putMyNotificationPreferences", Method: http.MethodPut, Path: "/me/notification-preferences", Tags: tags,
		Summary: "Change my notification preferences",
		Description: "Only the listed pairs change. Security notifications cannot be switched off " +
			"(422 notify.security_preference_locked).",
	}, op.Meta{Action: string(authz.NotifyPreferenceUpdateOwn), Surface: op.SurfaceSelf}), h.putPreferences)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "registerMyPushToken", Method: http.MethodPost, Path: "/me/push-tokens", Tags: tags,
		Summary: "Register a push token", DefaultStatus: http.StatusCreated,
		Description: "Bound to the current session. Registering a known token again moves it to this account and session.",
	}, op.Meta{Action: string(authz.NotifyPushTokenManage), Surface: op.SurfaceSelf, Idempotency: op.IdempotencyOptional}), h.registerPush)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "deleteMyPushToken", Method: http.MethodDelete, Path: "/me/push-tokens/{push_token_id}", Tags: tags,
		Summary: "Delete a push token", DefaultStatus: http.StatusNoContent,
	}, op.Meta{Action: string(authz.NotifyPushTokenManage), Surface: op.SurfaceSelf}), h.deletePush)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminListEmailSuppressions", Method: http.MethodGet, Path: "/admin/email-suppressions", Tags: tags,
		Summary: "List e-mail suppressions",
		Description: "Addresses are kept as blind indexes only: email finds one address by exact match. " +
			"Break-glass: X-Koro-Access-Reason is mandatory and every read is audited.",
	}, op.Meta{Action: string(authz.NotifySuppressionManage), Surface: op.SurfaceAdmin, BreakGlass: true}), h.adminListSuppressions)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminDeleteEmailSuppression", Method: http.MethodDelete, Path: "/admin/email-suppressions/{suppression_id}",
		Tags: tags, Summary: "Remove an e-mail suppression", DefaultStatus: http.StatusNoContent,
		Description: "The address may receive e-mail again. Break-glass: X-Koro-Access-Reason is mandatory; audited.",
	}, op.Meta{Action: string(authz.NotifySuppressionManage), Surface: op.SurfaceAdmin, BreakGlass: true}), h.adminDeleteSuppression)

	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "adminListUserDeliveries", Method: http.MethodGet, Path: "/admin/users/{user_id}/notification-deliveries",
		Tags: tags, Summary: "List a user's notification deliveries",
		Description: "IDs, channel, template, status and time only: never the content, the data or the address. " +
			"Break-glass: X-Koro-Access-Reason is mandatory; the read is audited and the user is notified.",
	}, op.Meta{Action: string(authz.NotifyDeliveryList), Surface: op.SurfaceAdmin, BreakGlass: true}), h.adminListDeliveries)
}

type handler struct{ svc Service }

// ---- receiveEmailWebhook

type webhookInput struct {
	Provider       string `path:"provider" enum:"ses,resend" doc:"E-mail provider (00 T-62)"`
	SvixID         string `header:"svix-id" maxLength:"200"`
	SvixTimestamp  string `header:"svix-timestamp" maxLength:"20"`
	SvixSignature  string `header:"svix-signature" maxLength:"2000"`
	SNSMessageType string `header:"x-amz-sns-message-type" maxLength:"50"`
	RawBody        []byte `contentType:"application/json"`
}

func (h handler) receiveWebhook(ctx context.Context, in *webhookInput) (*struct{}, error) {
	headers := map[string]string{}
	for k, v := range map[string]string{
		"svix-id": in.SvixID, "svix-timestamp": in.SvixTimestamp, "svix-signature": in.SvixSignature,
		"x-amz-sns-message-type": in.SNSMessageType,
	} {
		if v != "" {
			headers[k] = v
		}
	}
	return nil, h.svc.ReceiveEmailWebhook(ctx, in.Provider, headers, in.RawBody)
}

// ---- listMyNotifications

type listMineInput struct {
	page.Params
	UnreadOnly bool `query:"unread_only" doc:"Only unread notifications"`
}

type listMineOutput struct {
	Body NotificationList
}

func (h handler) listMine(ctx context.Context, in *listMineInput) (*listMineOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyNotifications", boolText(in.UnreadOnly))
	after, err := positionOf(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListMine(ctx, a, InAppQuery{UnreadOnly: in.UnreadOnly, Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &listMineOutput{Body: NotificationList{Items: make([]NotificationDTO, len(p.Items)), NextCursor: cursorOf(fp, p.Next)}}
	for i, n := range p.Items {
		out.Body.Items[i] = toNotificationDTO(n)
	}
	return out, nil
}

// ---- markMyNotificationRead, markAllMyNotificationsRead

type markReadInput struct {
	idempotency.OptionalHeader
	NotificationID string `path:"notification_id" doc:"Notification ID (an unknown or malformed ID is 404)"`
}

func (h handler) markRead(ctx context.Context, in *markReadInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.NotificationID)
	if err != nil {
		return nil, ErrNotificationNotFound
	}
	return nil, h.svc.MarkRead(ctx, a, id)
}

type markAllInput struct {
	idempotency.OptionalHeader
}

func (h handler) markAllRead(ctx context.Context, _ *markAllInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.MarkAllRead(ctx, a)
}

// ---- preferences

type preferencesOutput struct {
	Body PreferenceList
}

func (h handler) getPreferences(ctx context.Context, _ *struct{}) (*preferencesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	prefs, err := h.svc.MyPreferences(ctx, a)
	if err != nil {
		return nil, err
	}
	return &preferencesOutput{Body: toPreferenceList(prefs)}, nil
}

type putPreferencesInput struct {
	Body struct {
		Items []PreferenceDTO `json:"items" minItems:"1" maxItems:"21"`
	}
}

func (h handler) putPreferences(ctx context.Context, in *putPreferencesInput) (*preferencesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]Preference, len(in.Body.Items))
	for i, p := range in.Body.Items {
		items[i] = Preference{Category: PrefCategory(p.Category), Channel: Channel(p.Channel), Enabled: p.Enabled}
	}
	prefs, err := h.svc.PutMyPreferences(ctx, a, items)
	if err != nil {
		return nil, err
	}
	return &preferencesOutput{Body: toPreferenceList(prefs)}, nil
}

// ---- push tokens

type registerPushInput struct {
	idempotency.OptionalHeader
	Body struct {
		Platform   string `json:"platform" enum:"ios,android,web"`
		Token      string `json:"token" minLength:"1" maxLength:"4096"`
		AppVersion string `json:"app_version,omitempty" maxLength:"40"`
	}
}

type registerPushOutput struct {
	Body PushTokenCreatedDTO
}

func (h handler) registerPush(ctx context.Context, in *registerPushInput) (*registerPushOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := h.svc.RegisterPushToken(ctx, a, PushTokenInput{
		Platform: PushPlatform(in.Body.Platform), Token: in.Body.Token, AppVersion: in.Body.AppVersion,
	})
	if err != nil {
		return nil, err
	}
	return &registerPushOutput{Body: PushTokenCreatedDTO{PushTokenID: id.String()}}, nil
}

type deletePushInput struct {
	PushTokenID string `path:"push_token_id" doc:"Push token ID (an unknown or malformed ID is 404)"`
}

func (h handler) deletePush(ctx context.Context, in *deletePushInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.PushTokenID)
	if err != nil {
		return nil, ErrPushTokenNotFound
	}
	return nil, h.svc.DeletePushToken(ctx, a, id)
}

// ---- admin: suppressions

// AccessHeaders are the break-glass headers of the B operations (03 §6.3).
type AccessHeaders struct {
	AccessReason string `header:"X-Koro-Access-Reason" maxLength:"500" doc:"Break-glass justification (10–500 characters, mandatory)"`
	TicketRef    string `header:"X-Koro-Ticket-Ref" maxLength:"100" doc:"Support ticket reference (optional)"`
}

func (a AccessHeaders) access() Access { return Access{Reason: a.AccessReason, TicketRef: a.TicketRef} }

type adminListSuppressionsInput struct {
	AccessHeaders
	page.Params
	Email string `query:"email" maxLength:"254" doc:"Exact address (matched by its blind index; never stored)"`
}

type suppressionListOutput struct {
	Body SuppressionList
}

func (h handler) adminListSuppressions(ctx context.Context, in *adminListSuppressionsInput) (*suppressionListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	email := strings.TrimSpace(in.Email)
	// The fingerprint must not carry the address: hash it with the filter.
	fp := page.Fingerprint("adminListEmailSuppressions", email)
	after, err := suppressionPositionOf(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListSuppressions(ctx, a, in.access(), SuppressionQuery{Email: email, Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &suppressionListOutput{Body: SuppressionList{Items: make([]SuppressionDTO, len(p.Items))}}
	for i, s := range p.Items {
		out.Body.Items[i] = SuppressionDTO{SuppressionID: SuppressionID(s.EmailIndex), Reason: string(s.Reason),
			Source: string(s.Source), CreatedAt: s.CreatedAt}
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(suppressionSortKey, fp, page.TimeVal(p.Next.At), SuppressionID(p.Next.Index))
	}
	return out, nil
}

type adminDeleteSuppressionInput struct {
	AccessHeaders
	SuppressionID string `path:"suppression_id" maxLength:"64" doc:"Suppression ID (an unknown or malformed ID is 404)"`
}

func (h handler) adminDeleteSuppression(ctx context.Context, in *adminDeleteSuppressionInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.AdminDeleteSuppression(ctx, a, in.access(), in.SuppressionID)
}

// ---- admin: deliveries

type adminListDeliveriesInput struct {
	AccessHeaders
	page.Params
	UserID string `path:"user_id" doc:"User ID (a malformed ID is 404)"`
}

type deliveryListOutput struct {
	Body DeliveryList
}

func (h handler) adminListDeliveries(ctx context.Context, in *adminListDeliveriesInput) (*deliveryListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	userID, err := uuid.Parse(in.UserID)
	if err != nil {
		return nil, ErrNotFound
	}
	fp := page.Fingerprint("adminListUserDeliveries", userID.String())
	after, err := positionOf(in.Cursor, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.AdminListUserDeliveries(ctx, a, in.access(), userID, DeliveryQuery{Limit: in.Limit, After: after})
	if err != nil {
		return nil, err
	}
	out := &deliveryListOutput{Body: DeliveryList{Items: make([]DeliveryDTO, len(p.Items)), NextCursor: cursorOf(fp, p.Next)}}
	for i, d := range p.Items {
		out.Body.Items[i] = toDeliveryDTO(d)
	}
	return out, nil
}

// ---- cursors

const (
	sortKey            = "-created_at,-id"
	suppressionSortKey = "-created_at,-suppression_id"
)

func cursorOf(fp string, p *Position) string {
	if p == nil {
		return ""
	}
	return page.Next(sortKey, fp, page.TimeVal(p.At), page.UUIDVal(p.ID))
}

func positionOf(cursor, fp string) (*Position, error) {
	c, err := page.Decode(cursor, sortKey, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	at, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	id, err := c.UUID(1)
	if err != nil {
		return nil, err
	}
	return &Position{At: at, ID: id}, nil
}

func suppressionPositionOf(cursor, fp string) (*SuppressionPosition, error) {
	c, err := page.Decode(cursor, suppressionSortKey, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	at, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	idx, err := parseSuppressionID(c.Vals[1])
	if err != nil {
		return nil, page.ErrInvalidCursor
	}
	return &SuppressionPosition{At: at, Index: idx}, nil
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// timeOrNil returns nil for a nil pointer and the UTC instant otherwise.
func timeOrNil(t *time.Time) *time.Time { return utcPtr(t) }
