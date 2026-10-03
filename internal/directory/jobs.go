package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/notify"
	"koro/internal/platform/queue"
)

// SensitivityRequestTaskName is the worker task that tells the platform
// admins about a club admin's sensitivity request (T-67; mechanism A).
const SensitivityRequestTaskName = "directory.notify_sensitivity_request"

// templateSensitivityRequested is the in-app notice of the platform admins
// (internal/notify/templates/directory): a fixed text, the club's org ID
// and the request ID in the data.
const templateSensitivityRequested = "directory.sensitivity_requested"

// sensitivityRequest is the task payload (IDs only).
type sensitivityRequest struct {
	RequestID uuid.UUID `json:"request_id"`
	ClubOrgID uuid.UUID `json:"club_org_id"`
}

// Tasks returns the task handlers of directory for the worker.
func (m Module) Tasks() []queue.Task {
	return []queue.Task{{Name: SensitivityRequestTaskName, Handle: m.svc.handleSensitivityRequest}}
}

// handleSensitivityRequest sends one in-app notice per platform admin. The
// dedupe key makes a repeated task a no-op per admin; a permanent failure
// for one admin is logged and skipped, anything else is retried.
func (s *service) handleSensitivityRequest(ctx context.Context, payload []byte) error {
	var r sensitivityRequest
	if err := json.Unmarshal(payload, &r); err != nil || r.RequestID == uuid.Nil() || r.ClubOrgID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("directory: bad sensitivity request payload: %w", errors.Join(err, errors.New("ids required"))))
	}
	if s.d.Sender == nil || s.d.PlatformAdmins == nil {
		return errors.New("directory: notify or the platform admin lookup is not wired")
	}
	admins, err := s.d.PlatformAdmins(ctx)
	if err != nil {
		return fmt.Errorf("directory: platform admins: %w", err)
	}
	club := r.ClubOrgID
	for _, admin := range admins {
		to := admin
		_, err := s.d.Sender.Send(ctx, notify.Message{
			UserID: &to, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional,
			Template: templateSensitivityRequested, SourceOrgID: &club,
			Data:      map[string]any{"club_org_id": r.ClubOrgID, "request_id": r.RequestID},
			LinkPath:  "/admin/clubs/" + r.ClubOrgID.String(),
			DedupeKey: "directory.sensitivity_request:" + r.RequestID.String() + ":" + admin.String(),
		})
		switch {
		case err == nil:
		case notify.IsPermanent(err):
			s.d.Log.LogAttrs(ctx, slog.LevelError, "sensitivity request notice not deliverable",
				slog.String("request_id", r.RequestID.String()), slog.String("user_id", admin.String()),
				slog.String("err", err.Error()))
		default:
			return err
		}
	}
	return nil
}
