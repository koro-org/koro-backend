package identity

import (
	"context"
	"encoding/hex"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/civil"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register adds the identity operations (04 §2.1; BE-014, BE-016, BE-018) to
// api. svc may be nil (`koro openapi`): handlers only touch it when called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	reg(api, huma.Operation{
		OperationID: "registerAccount", Method: http.MethodPost, Path: "/auth/register", DefaultStatus: http.StatusAccepted,
		Summary: "Register an account",
		Description: "Always 202 for a valid request, whether the address is new or already registered (no account " +
			"enumeration); a verification e-mail follows. birth_date is required for students (age gate). The privacy " +
			"notice shown on the page is recorded as shown, never as consent; consents are optional, one per purpose.",
	}, op.Meta{Action: string(authz.IdentityAccountRegister), Surface: op.SurfacePublic, RateLimit: op.RateAuthRegister,
		Idempotency: op.IdempotencyOptional}, h.register)

	reg(api, huma.Operation{
		OperationID: "createSession", Method: http.MethodPost, Path: "/auth/sessions",
		Summary: "Sign in",
		Description: "Web clients (X-Koro-Client: web, the default) get the __Host-koro_session cookie and no token; " +
			"mobile clients get the bearer token once in the body. Wrong credentials, unknown or locked accounts all " +
			"answer 401 identity.invalid_credentials; an unverified account 403 identity.email_not_verified. An MFA " +
			"account gets mfa_required with a challenge (5 minutes) instead of a session: completeMfaLogin opens it.",
	}, op.Meta{Action: string(authz.IdentitySessionCreate), Surface: op.SurfacePublic, RateLimit: op.RateAuthLogin,
		Idempotency: op.IdempotencyOptional}, h.login)

	reg(api, huma.Operation{
		OperationID: "deleteCurrentSession", Method: http.MethodDelete, Path: "/auth/sessions/current", DefaultStatus: http.StatusNoContent,
		Summary: "Sign out", Description: "Revokes the session of this request and clears the cookie.",
	}, op.Meta{Action: string(authz.IdentitySessionDeleteCurrent), Surface: op.SurfaceSelf}, h.logout)

	reg(api, huma.Operation{
		OperationID: "listMySessions", Method: http.MethodGet, Path: "/me/sessions",
		Summary: "List my sessions", Description: "Live sessions only, newest first. IP addresses and user agents are not returned.",
	}, op.Meta{Action: string(authz.IdentitySessionListOwn), Surface: op.SurfaceSelf}, h.listSessions)

	reg(api, huma.Operation{
		OperationID: "revokeMySession", Method: http.MethodDelete, Path: "/me/sessions/{session_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Revoke one of my sessions", Description: "Another user's session is 404.",
	}, op.Meta{Action: string(authz.IdentitySessionRevokeOwn), Surface: op.SurfaceSelf}, h.revokeSession)

	reg(api, huma.Operation{
		OperationID: "confirmEmail", Method: http.MethodPost, Path: "/auth/email-verifications/confirm", DefaultStatus: http.StatusNoContent,
		Summary: "Verify an e-mail address", Description: "Single use; an unknown, used or expired token is 422 identity.invalid_token.",
	}, op.Meta{Action: string(authz.IdentityEmailVerify), Surface: op.SurfacePublic, RateLimit: op.RateTokenConfirm,
		Idempotency: op.IdempotencyOptional}, h.confirmEmail)

	reg(api, huma.Operation{
		OperationID: "resendEmailVerification", Method: http.MethodPost, Path: "/auth/email-verifications/resend", DefaultStatus: http.StatusAccepted,
		Summary: "Send the verification e-mail again", Description: "Always 202 (no account enumeration).",
	}, op.Meta{Action: string(authz.IdentityEmailResendVerification), Surface: op.SurfacePublic, RateLimit: op.RateAuthEmail,
		Idempotency: op.IdempotencyOptional}, h.resend)

	reg(api, huma.Operation{
		OperationID: "requestPasswordReset", Method: http.MethodPost, Path: "/auth/password-resets", DefaultStatus: http.StatusAccepted,
		Summary: "Request a password reset e-mail", Description: "Always 202 (no account enumeration). The link is valid for 30 minutes.",
	}, op.Meta{Action: string(authz.IdentityPasswordResetRequest), Surface: op.SurfacePublic, RateLimit: op.RateAuthPasswordReset,
		Idempotency: op.IdempotencyOptional}, h.resetRequest)

	reg(api, huma.Operation{
		OperationID: "confirmPasswordReset", Method: http.MethodPost, Path: "/auth/password-resets/confirm", DefaultStatus: http.StatusNoContent,
		Summary: "Set a new password with a reset token", Description: "Every session of the account is closed.",
	}, op.Meta{Action: string(authz.IdentityPasswordReset), Surface: op.SurfacePublic, RateLimit: op.RateAuthPasswordReset,
		Idempotency: op.IdempotencyOptional}, h.resetConfirm)

	reg(api, huma.Operation{
		OperationID: "changeMyPassword", Method: http.MethodPost, Path: "/me/password", DefaultStatus: http.StatusNoContent,
		Summary: "Change my password",
		Description: "current_password is the re-authentication proof (MFA accounts add reauth.totp_code): missing → 403 " +
			"identity.reauth_required, wrong → 403 identity.reauth_failed. The other sessions are closed.",
	}, op.Meta{Action: string(authz.IdentityPasswordChange), Surface: op.SurfaceSelf, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.changePassword)

	reg(api, huma.Operation{
		OperationID: "requestEmailChange", Method: http.MethodPost, Path: "/me/email-change", DefaultStatus: http.StatusAccepted,
		Summary: "Change my e-mail address", Description: "A confirmation link goes to the new address, a notice to the current one.",
	}, op.Meta{Action: string(authz.IdentityEmailChangeRequest), Surface: op.SurfaceSelf, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.emailChange)

	reg(api, huma.Operation{
		OperationID: "confirmEmailChange", Method: http.MethodPost, Path: "/auth/email-change/confirm", DefaultStatus: http.StatusNoContent,
		Summary: "Confirm a new e-mail address",
	}, op.Meta{Action: string(authz.IdentityEmailChangeConfirm), Surface: op.SurfacePublic, RateLimit: op.RateTokenConfirm,
		Idempotency: op.IdempotencyOptional}, h.confirmEmailChange)

	reg(api, huma.Operation{
		OperationID: "getMe", Method: http.MethodGet, Path: "/me",
		Summary: "My account", Description: "capabilities are menu hints, not authorization decisions.",
	}, op.Meta{Action: string(authz.IdentityAccountReadOwn), Surface: op.SurfaceSelf}, h.getMe)

	reg(api, huma.Operation{
		OperationID: "updateMe", Method: http.MethodPatch, Path: "/me",
		Summary: "Update my name or language",
	}, op.Meta{Action: string(authz.IdentityAccountUpdateOwn), Surface: op.SurfaceSelf}, h.updateMe)

	registerMFAAndAdmin(api, h)
	registerEdu(api, h)
}

// reg registers one identity operation with its Koro metadata.
func reg[I, O any](api huma.API, o huma.Operation, m op.Meta, fn func(context.Context, *I) (*O, error)) {
	o.Tags = []string{"identity"}
	huma.Register(api, op.Koro(o, m), fn)
}

type handler struct{ svc Service }

func fieldErr(location, msg string) error {
	return problem.ErrValidation.WithFields(problem.FieldError{Location: location, Message: msg})
}

func clientKind(s string) ClientKind {
	if s == string(ClientMobile) {
		return ClientMobile
	}
	return ClientWeb
}

// sessionCookie returns the Set-Cookie value of a web session (04 §1.5).
func sessionCookie(value string, expires time.Time, maxAge int) string {
	c := http.Cookie{ //nolint:gosec // Secure, HttpOnly and SameSite=Lax are set (04 §1.5); Lax keeps e-mail links working
		Name: httpx.SessionCookieName, Value: value, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
	}
	if !expires.IsZero() {
		c.Expires = expires
	}
	return c.String()
}

type acceptedOutput struct {
	Body struct{}
}

// ---- registerAccount

type consentGrantBody struct {
	PurposeCode     string `json:"purpose_code" pattern:"^[a-z][a-z0-9_]{2,40}$"`
	NoticeVersionID string `json:"notice_version_id" format:"uuid" doc:"The consent text shown"`
	TextSHA256      string `json:"text_sha256" pattern:"^[0-9a-f]{64}$" doc:"body_sha256 of that text"`
}

type registerInput struct {
	Client string `header:"X-Koro-Client" enum:"web,mobile" doc:"Client kind (consent channel); default web"`
	Body   struct {
		Email                  string             `json:"email" maxLength:"320"`
		Password               string             `json:"password" minLength:"1" maxLength:"1024" doc:"15-128 characters; common passwords are refused"`
		FirstName              string             `json:"first_name" minLength:"1" maxLength:"200"`
		LastName               string             `json:"last_name" minLength:"1" maxLength:"200"`
		BirthDate              string             `json:"birth_date,omitempty" format:"date" doc:"YYYY-MM-DD; required for students (age gate)"`
		SignupPersona          string             `json:"signup_persona" enum:"student,company,university,club"`
		Locale                 string             `json:"locale" enum:"tr-TR,en-US"`
		PrivacyNoticeVersionID string             `json:"privacy_notice_version_id" format:"uuid" doc:"The privacy notice shown on the page"`
		Consents               []consentGrantBody `json:"consents,omitempty" maxItems:"10" doc:"Optional consents, one per purpose (unticked by default)"`
	}
}

func (h handler) register(ctx context.Context, in *registerInput) (*acceptedOutput, error) {
	r := Registration{
		Email: in.Body.Email, Password: in.Body.Password, FirstName: in.Body.FirstName, LastName: in.Body.LastName,
		Persona: Persona(in.Body.SignupPersona), Locale: in.Body.Locale, Client: clientKind(in.Client),
	}
	if in.Body.BirthDate != "" {
		d, err := civil.Parse(in.Body.BirthDate)
		if err != nil {
			return nil, fieldErr("body.birth_date", "must be YYYY-MM-DD")
		}
		r.BirthDate = &d
	}
	id, err := uuid.Parse(in.Body.PrivacyNoticeVersionID)
	if err != nil {
		return nil, fieldErr("body.privacy_notice_version_id", "must be a UUID")
	}
	r.PrivacyNoticeID = id
	for _, c := range in.Body.Consents {
		nid, err := uuid.Parse(c.NoticeVersionID)
		if err != nil {
			return nil, fieldErr("body.consents.notice_version_id", "must be a UUID")
		}
		sum, err := hex.DecodeString(c.TextSHA256)
		if err != nil {
			return nil, fieldErr("body.consents.text_sha256", "must be hex")
		}
		r.Consents = append(r.Consents, ConsentGrant{Purpose: consent.Purpose(c.PurposeCode), NoticeVersionID: nid, TextSHA256: sum})
	}
	if err := h.svc.Register(ctx, authz.ActorOrAnonymous(ctx), r); err != nil {
		return nil, err
	}
	return &acceptedOutput{}, nil
}

// ---- createSession

type loginInput struct {
	Client string `header:"X-Koro-Client" enum:"web,mobile" doc:"web (default): cookie; mobile: bearer token in the body"`
	Body   struct {
		Email       string `json:"email" maxLength:"320"`
		Password    string `json:"password" minLength:"1" maxLength:"1024"`
		DeviceLabel string `json:"device_label,omitempty" maxLength:"100" doc:"Shown in the session list"`
	}
}

type loginOutput struct {
	SetCookie string `header:"Set-Cookie"`
	Body      SessionDTO
}

func (h handler) login(ctx context.Context, in *loginInput) (*loginOutput, error) {
	res, err := h.svc.CreateSession(ctx, authz.ActorOrAnonymous(ctx), Login{
		Email: in.Body.Email, Password: in.Body.Password, Client: clientKind(in.Client), DeviceLabel: in.Body.DeviceLabel,
	})
	if err != nil {
		return nil, err
	}
	if res.MFARequired {
		exp := res.Challenge.ExpiresAt
		return &loginOutput{Body: SessionDTO{MFARequired: true, ChallengeID: res.Challenge.ID.String(), ExpiresAt: &exp}}, nil
	}
	return sessionOutput(res), nil
}

// ---- deleteCurrentSession

type cookieOutput struct {
	SetCookie string `header:"Set-Cookie"`
}

func (h handler) logout(ctx context.Context, _ *struct{}) (*cookieOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.DeleteCurrentSession(ctx, a); err != nil {
		return nil, err
	}
	return &cookieOutput{SetCookie: sessionCookie("", time.Time{}, -1)}, nil
}

// ---- listMySessions

type listSessionsInput struct {
	page.Params
}

type listSessionsOutput struct {
	Body MySessionList
}

const sessionSort = "id_desc"

func (h handler) listSessions(ctx context.Context, in *listSessionsInput) (*listSessionsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMySessions")
	c, err := page.Decode(in.Cursor, sessionSort, fp, 1)
	if err != nil {
		return nil, err
	}
	q := SessionQuery{Limit: in.Limit}
	if c != nil {
		id, err := c.UUID(0)
		if err != nil {
			return nil, err
		}
		q.After = &id
	}
	p, err := h.svc.ListMySessions(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &listSessionsOutput{Body: MySessionList{Items: make([]MySessionDTO, len(p.Items))}}
	for i, s := range p.Items {
		out.Body.Items[i] = MySessionDTO{
			SessionID: s.ID.String(), ClientKind: string(s.ClientKind), DeviceLabel: s.DeviceLabel,
			CreatedAt: s.CreatedAt, LastSeenAt: s.LastSeenAt, Current: s.ID == p.Current,
		}
	}
	if p.More && len(p.Items) > 0 {
		out.Body.NextCursor = page.Next(sessionSort, fp, page.UUIDVal(p.Items[len(p.Items)-1].ID))
	}
	return out, nil
}

// ---- revokeMySession

type sessionIDInput struct {
	SessionID string `path:"session_id" doc:"Session ID (an unknown or malformed ID is 404)"`
}

func (h handler) revokeSession(ctx context.Context, in *sessionIDInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.SessionID)
	if err != nil {
		return nil, ErrNotFound
	}
	return nil, h.svc.RevokeMySession(ctx, a, id)
}

// ---- token confirmations

type tokenInput struct {
	Body struct {
		Token string `json:"token" minLength:"1" maxLength:"128" doc:"The token from the e-mail link"`
	}
}

func (h handler) confirmEmail(ctx context.Context, in *tokenInput) (*struct{}, error) {
	return nil, h.svc.ConfirmEmail(ctx, authz.ActorOrAnonymous(ctx), in.Body.Token)
}

func (h handler) confirmEmailChange(ctx context.Context, in *tokenInput) (*struct{}, error) {
	return nil, h.svc.ConfirmEmailChange(ctx, authz.ActorOrAnonymous(ctx), in.Body.Token)
}

type emailInput struct {
	Body struct {
		Email string `json:"email" maxLength:"320"`
	}
}

func (h handler) resend(ctx context.Context, in *emailInput) (*acceptedOutput, error) {
	if err := h.svc.ResendVerification(ctx, authz.ActorOrAnonymous(ctx), in.Body.Email); err != nil {
		return nil, err
	}
	return &acceptedOutput{}, nil
}

func (h handler) resetRequest(ctx context.Context, in *emailInput) (*acceptedOutput, error) {
	if err := h.svc.RequestPasswordReset(ctx, authz.ActorOrAnonymous(ctx), in.Body.Email); err != nil {
		return nil, err
	}
	return &acceptedOutput{}, nil
}

type resetConfirmInput struct {
	Body struct {
		Token       string `json:"token" minLength:"1" maxLength:"128"`
		NewPassword string `json:"new_password" minLength:"1" maxLength:"1024"`
	}
}

func (h handler) resetConfirm(ctx context.Context, in *resetConfirmInput) (*struct{}, error) {
	return nil, h.svc.ConfirmPasswordReset(ctx, authz.ActorOrAnonymous(ctx), in.Body.Token, in.Body.NewPassword)
}

// ---- changeMyPassword

type changePasswordInput struct {
	Body struct {
		CurrentPassword string             `json:"current_password,omitempty" maxLength:"1024" doc:"Re-authentication proof (03 §6.2)"`
		NewPassword     string             `json:"new_password" minLength:"1" maxLength:"1024"`
		Reauth          *consent.ReauthDTO `json:"reauth,omitempty" doc:"MFA accounts: reauth.totp_code"`
	}
}

func (h handler) changePassword(ctx context.Context, in *changePasswordInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.ChangePassword(ctx, a, PasswordChange{
		CurrentPassword: in.Body.CurrentPassword, NewPassword: in.Body.NewPassword, TOTPCode: reauthProof(in.Body.Reauth).TOTPCode,
	})
}

// ---- requestEmailChange

type emailChangeInput struct {
	Body struct {
		NewEmail string             `json:"new_email" maxLength:"320"`
		Reauth   *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) emailChange(ctx context.Context, in *emailChangeInput) (*acceptedOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.svc.RequestEmailChange(ctx, a, in.Body.NewEmail, reauthProof(in.Body.Reauth)); err != nil {
		return nil, err
	}
	return &acceptedOutput{}, nil
}

// ---- getMe / updateMe

type meOutput struct {
	Body MeDTO
}

func (h handler) getMe(ctx context.Context, _ *struct{}) (*meOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.Me(ctx, a)
	if err != nil {
		return nil, err
	}
	return &meOutput{Body: toMeDTO(m)}, nil
}

type updateMeInput struct {
	Body struct {
		FirstName *string `json:"first_name,omitempty" minLength:"1" maxLength:"200"`
		LastName  *string `json:"last_name,omitempty" minLength:"1" maxLength:"200"`
		Locale    *string `json:"locale,omitempty" enum:"tr-TR,en-US"`
	}
}

func (h handler) updateMe(ctx context.Context, in *updateMeInput) (*meOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	m, err := h.svc.UpdateMe(ctx, a, MePatch{FirstName: in.Body.FirstName, LastName: in.Body.LastName, Locale: in.Body.Locale})
	if err != nil {
		return nil, err
	}
	return &meOutput{Body: toMeDTO(m)}, nil
}
