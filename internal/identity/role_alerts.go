package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/applink"
	"koro/internal/platform/queue"
)

// Role alerts (03 §5.1 k3, 06 H-17): every step of a platform role
// assignment is mailed to every platform admin in effect, so a single
// admin cannot quietly request, approve or revoke a role. Mechanism A (no
// topic): the payload holds the assignment ID and the step only; the
// worker reads the rest and sends one e-mail per admin (deduplicated per
// admin, so a retried task never mails twice).
const (
	// SendRoleAlertTaskName is the task of the role alerts.
	SendRoleAlertTaskName = "identity.send_role_alert"
	roleAlertTemplate     = "identity.role_alert"
)

// Role alert kinds.
const (
	AlertRoleRequested       = "role_requested"
	AlertRoleGranted         = "role_granted"
	AlertRoleRejected        = "role_rejected"
	AlertRoleRevokeRequested = "role_revoke_requested"
	AlertRoleRevoked         = "role_revoked"
)

type roleAlert struct {
	AssignmentID uuid.UUID `json:"assignment_id"`
	Kind         string    `json:"kind"`
	Ref          uuid.UUID `json:"ref"` // deduplication key of the e-mails
}

// alertAdmins enqueues a role alert after the transaction commits.
func (s *service) alertAdmins(ctx context.Context, assignmentID uuid.UUID, kind string) {
	n := roleAlert{AssignmentID: assignmentID, Kind: kind, Ref: uuid.NewV7()}
	s.d.Queue.AfterCommit(ctx, SendRoleAlertTaskName, n, SendRoleAlertTaskName+":"+n.Ref.String())
}

// handleRoleAlert is the identity.send_role_alert handler.
func (s *service) handleRoleAlert(ctx context.Context, payload []byte) error {
	var n roleAlert
	if err := json.Unmarshal(payload, &n); err != nil || n.AssignmentID == uuid.Nil() || n.Ref == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("identity: role alert payload: %w", errors.Join(err, errors.New("missing ids"))))
	}
	switch n.Kind {
	case AlertRoleRequested, AlertRoleGranted, AlertRoleRejected, AlertRoleRevokeRequested, AlertRoleRevoked:
	default:
		return queue.Permanent(fmt.Errorf("identity: unknown role alert %q", n.Kind))
	}
	ra, err := s.roles().RoleAssignment(ctx, n.AssignmentID)
	switch {
	case isNotFound(err):
		return nil
	case err != nil:
		return err
	}
	admins, err := s.roles().UsersWithRole(ctx, authz.PlatformAdmin)
	if err != nil {
		return err
	}
	users, err := s.users().Users(ctx, admins)
	if err != nil {
		return err
	}
	var errs []error
	for _, u := range users {
		if u.Status != StatusActive || u.Email == "" {
			continue
		}
		msg := notify.Message{
			UserID: &u.ID, ToEmail: u.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
			Template: roleAlertTemplate, Locale: u.Locale,
			Data: map[string]any{"name": u.FirstName, "kind": n.Kind, "role": string(ra.Role),
				"link": applink.Page(s.d.LinkBaseURL, notify.AdminPlatformRolesLink().String())},
			DedupeKey: "identity.role_alert:" + n.Ref.String() + ":" + u.ID.String(),
		}
		if _, err := s.d.Mailer.Send(ctx, msg); err != nil {
			if notify.IsPermanent(err) {
				s.d.Log.LogAttrs(ctx, slog.LevelWarn, "role alert not sent", slog.String("user_id", u.ID.String()),
					slog.String("err", err.Error()))
				continue
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
