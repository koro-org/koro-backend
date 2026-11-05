package identity

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/problem"
)

// registerPhone adds the BE-172 operations: the caller's phone number, the
// SMS second step of sign-in and the password reset by SMS.
func registerPhone(api huma.API, h handler) {
	op.Register(api, "identity", huma.Operation{
		OperationID: "startPhoneVerification", Method: http.MethodPost, Path: "/me/phone", DefaultStatus: http.StatusAccepted,
		Summary: "Add or change my mobile number",
		Description: "reauth is the proof (password, or totp_code on a TOTP account). A 6-digit code goes by SMS to the " +
			"Turkish mobile number (valid 10 minutes); confirmPhoneVerification makes it the account's. The answer is the " +
			"same whether or not another account holds the number (only its holder learns that, at confirmation). " +
			"422 identity.invalid_phone for anything but +90 5XX XXX XX XX; 503 identity.sms_unavailable without an SMS " +
			"provider. Sends are limited per number, account and address (429).",
	}, op.Meta{Action: string(authz.IdentityPhoneChange), Surface: op.SurfaceSelf, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.startPhone)

	op.Register(api, "identity", huma.Operation{
		OperationID: "confirmPhoneVerification", Method: http.MethodPost, Path: "/me/phone/confirm",
		Summary: "Confirm my mobile number with the SMS code",
		Description: "The code of startPhoneVerification (single use, five tries). A wrong, used or expired code is 422 " +
			"identity.invalid_sms_code; a number another account verified meanwhile is 409 identity.phone_taken. The " +
			"number replaces an earlier one; the SMS second step stays as it was. A security notice goes by e-mail.",
	}, op.Meta{Action: string(authz.IdentityPhoneVerify), Surface: op.SurfaceSelf, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.confirmPhone)

	op.Register(api, "identity", huma.Operation{
		OperationID: "removeMyPhone", Method: http.MethodPost, Path: "/me/phone/remove", DefaultStatus: http.StatusNoContent,
		Summary: "Remove my mobile number",
		Description: "reauth is the proof. The number goes, and with it the SMS second step. 409 " +
			"identity.phone_not_verified without a number.",
	}, op.Meta{Action: string(authz.IdentityPhoneChange), Surface: op.SurfaceSelf, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.removePhone)

	op.Register(api, "identity", huma.Operation{
		OperationID: "enableSmsMfa", Method: http.MethodPost, Path: "/me/mfa/sms", DefaultStatus: http.StatusNoContent,
		Summary: "Turn the SMS second step on",
		Description: "reauth is the proof. Sign-ins then ask for a code sent by SMS to the verified number (next to TOTP " +
			"when that is on too). An SMS sign-in never makes the session MFA-verified. Holders of a role that requires " +
			"MFA (platform roles, university_rep, company_admin/owner) get 409 identity.sms_mfa_not_allowed: TOTP only. " +
			"409 identity.phone_not_verified without a number. Already on: 204, nothing changes.",
	}, op.Meta{Action: string(authz.IdentitySmsMfaEnable), Surface: op.SurfaceSelf, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.enableSMSMFA)

	op.Register(api, "identity", huma.Operation{
		OperationID: "disableSmsMfa", Method: http.MethodPost, Path: "/me/mfa/sms/disable", DefaultStatus: http.StatusNoContent,
		Summary:     "Turn the SMS second step off",
		Description: "reauth is the proof. The number stays. Already off: 204, nothing changes.",
	}, op.Meta{Action: string(authz.IdentitySmsMfaDisable), Surface: op.SurfaceSelf, Reauth: true, RateLimit: op.RateReauth,
		Idempotency: op.IdempotencyOptional}, h.disableSMSMFA)

	op.Register(api, "identity", huma.Operation{
		OperationID: "sendMfaLoginSms", Method: http.MethodPost, Path: "/auth/sessions/mfa/sms", DefaultStatus: http.StatusAccepted,
		Summary: "Send the SMS code of an MFA sign-in",
		Description: "For a challenge of createSession whose mfa_methods include sms: a 6-digit code goes to the " +
			"account's number (valid 5 minutes, never past the challenge); completeMfaLogin takes it as sms_code. An " +
			"unknown or expired challenge, or an account without the SMS second step, is 401 identity.mfa_login_failed " +
			"(the challenge stays usable). At most three codes per challenge, within the SMS send limits (429).",
	}, op.Meta{Action: string(authz.IdentitySessionCreate), Surface: op.SurfacePublic, RateLimit: op.RateAuthLogin,
		Idempotency: op.IdempotencyOptional}, h.sendMFALoginSMS)

	op.Register(api, "identity", huma.Operation{
		OperationID: "confirmPasswordResetSms", Method: http.MethodPost, Path: "/auth/password-resets/sms/confirm",
		DefaultStatus: http.StatusNoContent,
		Summary:       "Set a new password with an SMS code",
		Description: "The code of requestPasswordReset with channel sms. A wrong, used or expired code, an unknown " +
			"address and an account that may no longer reset by SMS all answer 422 identity.invalid_sms_code. Every " +
			"session of the account is closed.",
	}, op.Meta{Action: string(authz.IdentityPasswordReset), Surface: op.SurfacePublic, RateLimit: op.RateAuthPasswordReset,
		Idempotency: op.IdempotencyOptional}, h.resetConfirmSMS)
}

// SMSCodeSentDTO answers an operation that sent a code by SMS.
type SMSCodeSentDTO struct {
	ExpiresAt time.Time `json:"expires_at" doc:"When the code stops working"`
	PhoneHint string    `json:"phone_hint" doc:"The number the code went to, masked (last two digits)"`
}

// PhoneDTO is the caller's verified number, masked (BE-172).
type PhoneDTO struct {
	NumberHint    string    `json:"number_hint" doc:"The number, masked (last two digits); the full number is never returned"`
	VerifiedAt    time.Time `json:"verified_at"`
	SMSMFAEnabled bool      `json:"sms_mfa_enabled" doc:"The SMS second step of sign-in is on"`
}

func toPhoneDTO(v PhoneView) PhoneDTO {
	return PhoneDTO{NumberHint: v.Hint, VerifiedAt: v.VerifiedAt, SMSMFAEnabled: v.SMSMFAEnabled}
}

type smsSentOutput struct {
	Body SMSCodeSentDTO
}

func sentOutput(s SMSCodeSent) *smsSentOutput {
	return &smsSentOutput{Body: SMSCodeSentDTO(s)}
}

type startPhoneInput struct {
	Body struct {
		Phone  string             `json:"phone" minLength:"10" maxLength:"30" doc:"A Turkish mobile number, e.g. +90 5XX XXX XX XX or 05XX XXX XX XX"`
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2): password, or totp_code on a TOTP account"`
	}
}

func (h handler) startPhone(ctx context.Context, in *startPhoneInput) (*smsSentOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	sent, err := h.svc.StartPhoneVerification(ctx, a, in.Body.Phone, reauthProof(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return sentOutput(sent), nil
}

type smsCodeInput struct {
	Body struct {
		Code string `json:"code" minLength:"1" maxLength:"16" doc:"The 6-digit code of the SMS"`
	}
}

type phoneOutput struct {
	Body PhoneDTO
}

func (h handler) confirmPhone(ctx context.Context, in *smsCodeInput) (*phoneOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.ConfirmPhoneVerification(ctx, a, in.Body.Code)
	if err != nil {
		return nil, err
	}
	return &phoneOutput{Body: toPhoneDTO(v)}, nil
}

func (h handler) removePhone(ctx context.Context, in *reauthOnlyInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.RemovePhone(ctx, a, reauthProof(in.Body.Reauth))
}

func (h handler) enableSMSMFA(ctx context.Context, in *reauthOnlyInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.EnableSMSMFA(ctx, a, reauthProof(in.Body.Reauth))
}

func (h handler) disableSMSMFA(ctx context.Context, in *reauthOnlyInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.DisableSMSMFA(ctx, a, reauthProof(in.Body.Reauth))
}

type challengeInput struct {
	Body struct {
		ChallengeID string `json:"challenge_id" format:"uuid"`
	}
}

func (h handler) sendMFALoginSMS(ctx context.Context, in *challengeInput) (*smsSentOutput, error) {
	id, err := uuid.Parse(in.Body.ChallengeID)
	if err != nil {
		return nil, problem.Field("body.challenge_id", "must be a UUID")
	}
	sent, err := h.svc.SendMFALoginSMS(ctx, authz.ActorOrAnonymous(ctx), id)
	if err != nil {
		return nil, err
	}
	return sentOutput(sent), nil
}

type resetConfirmSMSInput struct {
	Body struct {
		Email       emailText   `json:"email"`
		Code        string      `json:"code" minLength:"1" maxLength:"16" doc:"The 6-digit code of the SMS"`
		NewPassword newPassword `json:"new_password"`
	}
}

func (h handler) resetConfirmSMS(ctx context.Context, in *resetConfirmSMSInput) (*struct{}, error) {
	return nil, h.svc.ConfirmPasswordResetSMS(ctx, authz.ActorOrAnonymous(ctx), PasswordResetSMS{
		Email: string(in.Body.Email), Code: in.Body.Code, NewPassword: string(in.Body.NewPassword),
	})
}
