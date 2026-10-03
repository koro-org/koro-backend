package identity

import (
	"context"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/privacy"
)

// privacyEraseJob is the system actor of the document deletions of an
// erasure.
const privacyEraseJob = "identity.privacy_erase"

// privacyProvider is the KVKK provider of identity (01 §3.3). kvkk calls
// Erase last (01 §6.3 "en son identity anonimleştirme"): the account
// becomes a tombstone that stays the FK target of financial and audit rows
// (02 §0.6); sessions, e-mail tokens, TOTP, recovery codes and student
// verifications (with their documents) are deleted.
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "identity" }

// Export implements privacy.Provider: the account, its sessions (device,
// IP, user agent), its TOTP enrollment (dates only) and its platform role
// history. Hashes, secrets and tokens are never exported.
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
	if t, found, err := s.d.Store.TOTP(ctx, userID); err != nil {
		return out, err
	} else if found { // the secret is never exported
		left, err := s.d.Store.RecoveryCodesLeft(ctx, userID)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, map[string]any{
			"type": "mfa_totp", "enrolled_at": t.CreatedAt, "confirmed_at": t.ConfirmedAt, "recovery_codes_left": left,
		})
	}
	actions, err := s.d.Store.AccountActionsOf(ctx, userID)
	if err != nil {
		return out, err
	}
	for _, r := range actions { // four-eyes admin actions against the account (06 H-47)
		out.Items = append(out.Items, map[string]any{
			"type": "admin_account_action", "action": string(r.Action), "status": string(r.Status), "reason": r.Reason,
			"requested_at": r.RequestedAt, "approved_at": r.ApprovedAt,
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
	edu, err := s.d.Store.EduVerificationsOf(ctx, userID)
	if err != nil {
		return out, err
	}
	for _, v := range edu { // the subject's own address in full; the document is listed by ID (files serves it)
		item := map[string]any{
			"type": "student_verification", "method": string(v.Method), "status": string(v.Status), "edu_email": v.EduEmail,
			"verified_at": v.VerifiedAt, "expires_at": v.ExpiresAt, "review_note": v.ReviewNote, "created_at": v.CreatedAt,
		}
		if v.UniversityID != nil {
			item["university_id"] = v.UniversityID.String()
		}
		if v.DocumentFileID != nil {
			item["document_file_id"] = v.DocumentFileID.String()
		}
		out.Items = append(out.Items, item)
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
		nm, err := s.d.Store.DeleteTOTP(ctx, userID) // 02 §5.5–5.6: hard delete with the account
		if err != nil {
			return err
		}
		nr, err := s.d.Store.DeleteRecoveryCodes(ctx, userID)
		if err != nil {
			return err
		}
		// Student verifications go with their documents (02 §5.4: hard
		// delete); the files are deleted after the rows released them.
		edu, err := s.d.Store.DeleteUserEduVerifications(ctx, userID)
		if err != nil {
			return err
		}
		for _, v := range edu {
			if v.DocumentFileID != nil {
				if err := s.removeFile(ctx, authz.SystemActor(privacyEraseJob), *v.DocumentFileID); err != nil {
					return err
				}
			}
		}
		nr += len(edu)
		erased, err := s.d.Store.EraseUser(ctx, userID, s.now())
		if err != nil && !isNotFound(err) {
			return err
		}
		roles, err := s.d.Store.RoleAssignmentsOf(ctx, userID)
		if err != nil {
			return err
		}
		res.Deleted = ns + nt + nm + nr
		if erased {
			res.Anonymized = 1
		}
		actions, err := s.d.Store.AccountActionsOf(ctx, userID)
		if err != nil {
			return err
		}
		if n := len(roles) + len(actions); n > 0 {
			res.Retained = n
			res.RetainedReason = "platform_role_assignments, privileged_account_actions: access review and four-eyes proof (role_history, 3 years)"
		}
		return nil
	})
	return res, err
}
