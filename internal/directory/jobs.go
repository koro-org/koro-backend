package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// SensitivityRequestTaskName is the worker task that tells the platform
// admins about a club admin's sensitivity request (T-67; mechanism A).
const SensitivityRequestTaskName = "directory.notify_sensitivity_request"

// templateSensitivityRequested is the in-app notice of the platform admins
// (internal/notify/templates/directory): a fixed text, the club's org ID
// and the request ID in the data.
const templateSensitivityRequested = "directory.sensitivity_requested"

// NotifyClaimDecidedTaskName consumes club_claim.decided (01 §6.2): the
// claimant gets an in-app notice of the decision.
const NotifyClaimDecidedTaskName = "directory.notify_claim_decided"

// RemoveRejectedEvidenceTaskName consumes file.rejected for claim evidence
// (01 §6.2, T-176): the claim drops the file and the file is deleted.
const RemoveRejectedEvidenceTaskName = "directory.remove_rejected_evidence"

// rejectedEvidenceJob is the system actor of RemoveRejectedEvidenceTaskName.
const rejectedEvidenceJob = "directory.remove_rejected_evidence"

// Templates of the claimant's in-app notice (fixed texts; the data holds
// the claim and club IDs only). The decision note is never sent: it is for
// platform staff (BE-022).
const (
	templateClaimApproved = "directory.claim_approved"
	templateClaimRejected = "directory.claim_rejected"
)

// sensitivityRequest is the task payload (IDs only).
type sensitivityRequest struct {
	RequestID uuid.UUID `json:"request_id"`
	ClubOrgID uuid.UUID `json:"club_org_id"`
}

// Tasks returns the task handlers of directory for the worker.
func (m Module) Tasks() []queue.Task {
	return []queue.Task{{Name: SensitivityRequestTaskName, Handle: m.svc.handleSensitivityRequest}}
}

// Subscribe registers the outbox subscription of directory (01 §6.2):
// club_claim.decided → directory.notify_claim_decided.
func (m Module) Subscribe(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicClubClaimDecided, NotifyClaimDecidedTaskName, m.svc.handleClaimDecided,
		outbox.Queue(queue.Default))
}

// SubscribeFileRejected registers file.rejected →
// directory.remove_rejected_evidence (01 §6.2, T-176: the owning module of
// a file the scan rejected drops its reference). Like identity's (BE-018,
// T-222) it is not wired yet: files' wiring test pins file.rejected to its
// single subscriber. Until then a rejected evidence file stays referenced;
// it cannot be downloaded (the admin view says not_clean).
func (m Module) SubscribeFileRejected(r *outbox.Registry) {
	r.Subscribe(domainevent.TopicFileRejected, RemoveRejectedEvidenceTaskName, m.svc.handleFileRejected,
		outbox.Queue(queue.Default))
}

// handleClaimDecided sends the claimant one in-app notice per decision
// (dedupe key: the claim). A permanent failure (e.g. the account is gone)
// is logged and dropped; anything else is retried.
func (s *service) handleClaimDecided(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ClubClaimDecided](e)
	if err != nil {
		return queue.Permanent(err)
	}
	tmpl, link := templateClaimRejected, "/me/club-claims"
	switch p.Outcome {
	case OutcomeApproved:
		tmpl, link = templateClaimApproved, "/clubs/"+p.ClubOrgID.String()
	case OutcomeRejected:
	default:
		return queue.Permanent(fmt.Errorf("directory: club_claim.decided: unknown outcome %q", p.Outcome))
	}
	if s.d.Sender == nil {
		return errors.New("directory: notify is not wired")
	}
	to, club := p.UserID, p.ClubOrgID
	_, err = s.d.Sender.Send(ctx, notify.Message{
		UserID: &to, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
		SourceOrgID: &club, Data: map[string]any{"claim_id": p.ClaimID, "club_org_id": p.ClubOrgID},
		LinkPath: link, DedupeKey: "directory.claim_decided:" + p.ClaimID.String(),
	})
	if err != nil && notify.IsPermanent(err) {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "claim decision notice not deliverable",
			slog.String("claim_id", p.ClaimID.String()), slog.String("err", err.Error()))
		return nil
	}
	return err
}

// handleFileRejected consumes file.rejected (01 §6.2, T-176): the claim
// whose evidence the scan rejected drops the file reference and the file
// is deleted; the claim itself stays (a pending one may still have its
// evidence text; the reviewer decides). The uploader already got files'
// notice.
func (s *service) handleFileRejected(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.FileRejected](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.AttachedModule != claimOwner {
		return nil
	}
	sys := authz.SystemActor(rejectedEvidenceJob)
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		c, err := s.d.Store.ClaimOfFile(ctx, p.FileID)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil // already dropped (a repeated task) or erased
		case err != nil:
			return err
		}
		if c, err = s.d.Store.LockClaim(ctx, c.ID); err != nil {
			return err
		}
		c.EvidenceFileID = nil
		if _, err := s.d.Store.UpdateClaim(ctx, c); err != nil {
			return err
		}
		if err := s.removeFile(ctx, sys, p.FileID); err != nil {
			return err
		}
		ev := auditEvent(sys, authz.DirectoryClubClaimCreate, authz.TypeClubClaim, c.ID, nil, "", map[string]any{
			"operation": "evidence_rejected_by_scan", "file_id": p.FileID.String(), "reason": p.Reason,
			audit.MetaSubjectUserID: c.ClaimantUserID.String(),
		})
		ev.Outcome = audit.Failure
		return s.d.Audit.Log(ctx, ev)
	})
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
