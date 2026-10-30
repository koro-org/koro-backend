package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"uuid"

	"koro/internal/notify"
	"koro/internal/platform/applink"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
	"koro/internal/platform/token"
)

// Task names (01 §8: <module>.<verb>_<object>).
const (
	// SendEmailTokenTaskName consumes identity.email_token_issued: it
	// re-derives the token and sends the verification, reset or change
	// e-mail.
	SendEmailTokenTaskName = "identity.send_email_token"
	// SendSecurityNoticeTaskName sends a security notice (registration
	// attempt on an existing account, password changed, e-mail change
	// requested or done). Mechanism A without a topic: the payload holds
	// the user ID and the notice kind only.
	SendSecurityNoticeTaskName = "identity.send_security_notice"
)

// Notice kinds of identity.security_notice.
const (
	NoticeSignupAttempt        = "signup_attempt"
	NoticePasswordChanged      = "password_changed"
	NoticeEmailChangeRequested = "email_change_requested"
	NoticeEmailChanged         = "email_changed"

	// MFA notices (03 §6.4-4; identity.mfa_notice template, BE-016).
	NoticeMFAEnabled               = "mfa_enabled"
	NoticeMFADisabled              = "mfa_disabled"
	NoticeRecoveryCodeUsed         = "recovery_code_used"
	NoticeRecoveryCodesRegenerated = "recovery_codes_regenerated"
	NoticeMFAReset                 = "mfa_reset" // by a platform admin (k2)
)

// Templates (internal/notify/templates/identity/, BE-001 R8).
// The link paths are the public client contract of internal/platform/applink
// (BE-096).
var tokenTemplates = map[TokenPurpose]struct {
	template string
	path     applink.TokenPath
}{
	PurposeAccountEmail:  {"identity.verify_email", applink.VerifyEmail},
	PurposePasswordReset: {"identity.password_reset", applink.ResetPassword},
	PurposeEmailChange:   {"identity.email_change", applink.ConfirmEmailChange},
	PurposeEduEmail:      {"identity.edu_verify", applink.VerifyEdu},
}

const (
	securityNoticeTemplate = "identity.security_notice"
	mfaNoticeTemplate      = "identity.mfa_notice"
)

// noticeTemplates maps every security notice kind to its template.
var noticeTemplates = map[string]string{
	NoticeSignupAttempt: securityNoticeTemplate, NoticePasswordChanged: securityNoticeTemplate,
	NoticeEmailChangeRequested: securityNoticeTemplate, NoticeEmailChanged: securityNoticeTemplate,
	NoticeMFAEnabled: mfaNoticeTemplate, NoticeMFADisabled: mfaNoticeTemplate, NoticeRecoveryCodeUsed: mfaNoticeTemplate,
	NoticeRecoveryCodesRegenerated: mfaNoticeTemplate, NoticeMFAReset: mfaNoticeTemplate,
}

// issueToken opens a new e-mail token of purpose for u in the transaction
// carried by ctx: the open tokens of the same purpose close, the row keeps
// only the SHA-256 of the derived token, and identity.email_token_issued
// (O) carries the token ID, never the token (01 §6.2).
func (s *service) issueToken(ctx context.Context, u User, purpose TokenPurpose, target string) (EmailToken, error) {
	now := s.now()
	t := EmailToken{
		ID: uuid.NewV7(), UserID: u.ID, Purpose: purpose, TargetEmail: target,
		KeyID: s.d.Deriver.ActiveKeyID(), ExpiresAt: now.Add(tokenTTL(purpose)), CreatedAt: now,
	}
	raw, err := s.deriveToken(t.KeyID, t.ID)
	if err != nil {
		return EmailToken{}, err
	}
	t.TokenHash = token.Hash(raw)
	if err := s.emailTokens().CloseOpenTokens(ctx, u.ID, purpose, now); err != nil {
		return EmailToken{}, err
	}
	if err := s.emailTokens().InsertEmailToken(ctx, t); err != nil {
		return EmailToken{}, err
	}
	env, err := domainevent.New(domainevent.IdentityEmailTokenIssued{TokenID: t.ID, UserID: u.ID, Purpose: string(purpose)}, emailTokenAggType, t.ID)
	if err != nil {
		return EmailToken{}, err
	}
	if err := s.d.Outbox.Add(ctx, env); err != nil {
		return EmailToken{}, err
	}
	return t, nil
}

// securityNotice is the payload of identity.send_security_notice: IDs and
// a kind only (05 §8.4-1).
type securityNotice struct {
	UserID uuid.UUID `json:"user_id"`
	Kind   string    `json:"kind"`
	Ref    uuid.UUID `json:"ref"` // deduplication key of the e-mail
}

// notifyUser enqueues a security notice after the transaction commits
// (mechanism A: best effort, at most once; a lost notice is not repaired).
func (s *service) notifyUser(ctx context.Context, userID uuid.UUID, kind string) {
	n := securityNotice{UserID: userID, Kind: kind, Ref: uuid.NewV7()}
	s.d.Queue.AfterCommit(ctx, SendSecurityNoticeTaskName, n, SendSecurityNoticeTaskName+":"+n.Ref.String())
}

// handleEmailTokenIssued is the identity.send_email_token handler. It is
// idempotent: notify deduplicates on the token ID, and a used, expired or
// deleted token is skipped.
func (s *service) handleEmailTokenIssued(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.IdentityEmailTokenIssued](e)
	if err != nil {
		return queue.Permanent(err)
	}
	t, err := s.emailTokens().EmailToken(ctx, p.TokenID)
	switch {
	case isNotFound(err):
		return nil // cleaned up meanwhile
	case err != nil:
		return err
	}
	if t.ConsumedAt != nil || !s.now().Before(t.ExpiresAt) {
		return nil // replaced, used or expired: nothing to send
	}
	tpl, ok := tokenTemplates[t.Purpose]
	if !ok {
		return queue.Permanent(fmt.Errorf("identity: no e-mail for token purpose %q", t.Purpose))
	}
	u, err := s.users().User(ctx, t.UserID)
	switch {
	case isNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if u.Status == StatusErased || u.Status == StatusSuspended {
		return nil
	}
	raw, err := s.deriveToken(t.KeyID, t.ID)
	if err != nil {
		return queue.Permanent(err) // the key was removed from CRYPTO_TOKEN_KEYS
	}
	if !token.Verify(raw, t.TokenHash) {
		return queue.Permanent(errors.New("identity: derived token does not match the stored hash (token keys changed)"))
	}
	to := u.Email
	if t.Purpose == PurposeEmailChange || t.Purpose == PurposeEduEmail {
		to = t.TargetEmail // the address being proven
	}
	minutes := int(tokenTTL(t.Purpose).Minutes())
	msg := notify.Message{
		UserID: &u.ID, ToEmail: to, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: tpl.template, Locale: u.Locale,
		Data: map[string]any{
			"name":          u.FirstName,
			"link":          applink.Token(s.d.LinkBaseURL, tpl.path, raw),
			"token":         raw,
			"valid_minutes": strconv.Itoa(minutes),
		},
		DedupeKey: "identity.email_token:" + t.ID.String(),
	}
	if _, err := s.d.Mailer.Send(ctx, msg); err != nil {
		if notify.IsPermanent(err) {
			return queue.Permanent(err)
		}
		return err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "e-mail token sent", slog.String("token_id", t.ID.String()),
		slog.String("purpose", string(t.Purpose)))
	return nil
}

// handleSecurityNotice is the identity.send_security_notice handler.
func (s *service) handleSecurityNotice(ctx context.Context, payload []byte) error {
	var n securityNotice
	if err := json.Unmarshal(payload, &n); err != nil || n.UserID == uuid.Nil() || n.Ref == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("identity: security notice payload: %w", errors.Join(err, errors.New("missing ids"))))
	}
	tpl, ok := noticeTemplates[n.Kind]
	if !ok {
		return queue.Permanent(fmt.Errorf("identity: unknown security notice %q", n.Kind))
	}
	u, err := s.users().User(ctx, n.UserID)
	switch {
	case isNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if u.Status == StatusErased || u.Email == "" {
		return nil
	}
	msg := notify.Message{
		UserID: &u.ID, ToEmail: u.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: tpl, Locale: u.Locale,
		Data:      map[string]any{"name": u.FirstName, "kind": n.Kind, "link": applink.Page(s.d.LinkBaseURL, string(applink.ResetPassword))},
		DedupeKey: "identity.security:" + n.Ref.String(),
	}
	if _, err := s.d.Mailer.Send(ctx, msg); err != nil {
		if notify.IsPermanent(err) {
			return queue.Permanent(err)
		}
		return err
	}
	return nil
}
