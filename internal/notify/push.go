package notify

import (
	"context"
	"crypto/sha256"
	"strings"
	"unicode"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/problem"
)

// Push token limits (push_tokens CHECKs).
const (
	maxPushToken  = 4096
	maxAppVersion = 40
)

// PushTokenInput is the body of registerMyPushToken (04 §2.19).
type PushTokenInput struct {
	Platform   PushPlatform
	Token      string
	AppVersion string
}

func pushResource(owner, id uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypePushToken, ID: id, OwnerUserID: &owner}
}

// RegisterPushToken implements Service. The token is bound to the actor's
// session (02 §6.3: revoked with it); a device that registers the same
// token again — after signing in to another account too — moves the one row
// (token_sha256 is unique). The token itself is never logged or audited.
func (s *service) RegisterPushToken(ctx context.Context, a authz.Actor, in PushTokenInput) (uuid.UUID, error) {
	if err := authz.Can(ctx, a, authz.NotifyPushTokenManage, pushResource(a.UserID, uuid.Nil())); err != nil {
		return uuid.Nil(), err
	}
	var fe []problem.FieldError
	if !in.Platform.valid() {
		fe = append(fe, problem.FieldError{Location: "body.platform", Message: "must be ios, android or web"})
	}
	if in.Token == "" || len(in.Token) > maxPushToken || strings.IndexFunc(in.Token, unicode.IsSpace) >= 0 || !utf8.ValidString(in.Token) {
		fe = append(fe, problem.FieldError{Location: "body.token", Message: "must be 1 to 4096 characters without spaces"})
	}
	if utf8.RuneCountInString(in.AppVersion) > maxAppVersion || strings.IndexFunc(in.AppVersion, unicode.IsControl) >= 0 {
		fe = append(fe, problem.FieldError{Location: "body.app_version", Message: "must be at most 40 characters"})
	}
	if len(fe) > 0 {
		return uuid.Nil(), problem.ErrValidation.WithFields(fe...)
	}
	sum := sha256.Sum256([]byte(in.Token))
	now := s.d.Clock.Now()
	t := PushToken{
		ID: uuid.NewV7(), UserID: a.UserID, Platform: in.Platform, Token: in.Token, TokenSHA256: sum[:],
		AppVersion: strings.TrimSpace(in.AppVersion), CreatedAt: now, LastSeenAt: now,
	}
	if a.SessionID != uuid.Nil() {
		sid := a.SessionID
		t.SessionID = &sid
	}
	var stored PushToken
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var err error
		if stored, err = s.d.Store.UpsertPushToken(ctx, t); err != nil {
			return err
		}
		id := stored.ID
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.NotifyPushTokenManage), Outcome: audit.Success,
			ResourceType: authz.TypePushToken, ResourceID: &id,
			Metadata: map[string]any{"op": "register", "platform": string(stored.Platform)},
		}.By(a))
	})
	if err != nil {
		return uuid.Nil(), err
	}
	return stored.ID, nil
}

// DeletePushToken implements Service: the row is deleted (data
// minimisation); another user's token is not found.
func (s *service) DeletePushToken(ctx context.Context, a authz.Actor, id uuid.UUID) error {
	if err := authz.Can(ctx, a, authz.NotifyPushTokenManage, pushResource(a.UserID, id)); err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.DeletePushToken(ctx, a.UserID, id); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.NotifyPushTokenManage), Outcome: audit.Success,
			ResourceType: authz.TypePushToken, ResourceID: &id, Metadata: map[string]any{"op": "delete"},
		}.By(a))
	})
}
