package identity

import (
	"context"
	"uuid"

	"koro/internal/platform/privacy"
)

// privacyProvider is the KVKK provider of identity (01 §3.3). kvkk calls
// Erase last (01 §6.3 "en son identity anonimleştirme"): the account
// becomes a tombstone that stays the FK target of financial and audit rows
// (02 §0.6); sessions and e-mail tokens are deleted.
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "identity" }

// Export implements privacy.Provider: the account, its sessions (device,
// IP, user agent) and its platform role history. Hashes and tokens are
// never exported.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	s := p.s
	out := privacy.ExportSection{Module: "identity"}
	u, err := s.d.Store.User(ctx, userID)
	switch {
	case isNotFound(err):
		return out, nil
	case err != nil:
		return out, err
	}
	acct := map[string]any{
		"type": "account", "user_id": u.ID.String(), "status": string(u.Status), "email": u.Email,
		"first_name": u.FirstName, "last_name": u.LastName, "signup_persona": string(u.Persona), "locale": u.Locale,
		"mfa_enabled": u.MFAEnabled, "created_at": u.CreatedAt, "email_verified_at": u.EmailVerifiedAt,
		"last_login_at": u.LastLoginAt,
	}
	if u.BirthDate != nil {
		acct["birth_date"] = u.BirthDate.String()
	}
	out.Items = append(out.Items, acct)
	sessions, err := s.d.Store.SessionsOf(ctx, userID)
	if err != nil {
		return out, err
	}
	for _, x := range sessions {
		out.Items = append(out.Items, map[string]any{
			"type": "session", "client_kind": string(x.ClientKind), "device_label": x.DeviceLabel, "ip": x.IP,
			"user_agent": x.UserAgent, "created_at": x.CreatedAt, "last_seen_at": x.LastSeenAt,
			"revoked_at": x.RevokedAt, "revoke_reason": string(x.RevokeReason),
		})
	}
	roles, err := s.d.Store.RoleAssignmentsOf(ctx, userID)
	if err != nil {
		return out, err
	}
	for _, r := range roles {
		out.Items = append(out.Items, map[string]any{
			"type": "platform_role", "role": string(r.Role), "status": string(r.Status), "granted_at": r.GrantedAt, "revoked_at": r.RevokedAt,
		})
	}
	return out, nil
}

// Erase implements privacy.Provider. It is idempotent: a tombstone stays
// one, and there is nothing left to delete.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	s := p.s
	res := privacy.EraseResult{Module: "identity"}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		ns, err := s.d.Store.DeleteUserSessions(ctx, userID)
		if err != nil {
			return err
		}
		nt, err := s.d.Store.DeleteUserEmailTokens(ctx, userID)
		if err != nil {
			return err
		}
		erased, err := s.d.Store.EraseUser(ctx, userID, s.now())
		if err != nil && !isNotFound(err) {
			return err
		}
		roles, err := s.d.Store.RoleAssignmentsOf(ctx, userID)
		if err != nil {
			return err
		}
		res.Deleted = ns + nt
		if erased {
			res.Anonymized = 1
		}
		if len(roles) > 0 {
			res.Retained = len(roles)
			res.RetainedReason = "platform_role_assignments: access review proof (role_history, 3 years)"
		}
		return nil
	})
	return res, err
}
