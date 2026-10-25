package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/notify"
	"koro/internal/platform/page"
	"koro/internal/platform/queue"
)

// Claim routing over time (06 H-58, T-230; BE-033). The university_rep
// decides a claim on a non-sensitive club of their university (E-21); so a
// claim does not wait forever on a silent rep:
//
//   - the reps get an in-app notice when the claim is made (mechanism A,
//     directory.notify_claim_created) and a reminder once it is
//     ClaimReminderAfter old (directory.claim_reminders, daily);
//   - from ClaimEscalateAfter on, platform staff decide it: approvalOf
//     names platform_admin, the rep's queue leaves it out, and the daily
//     job tells the platform admins once (dedupe key per claim and admin).
//
// The escalation is derived from created_at (no column, no migration), so
// it holds from the very second the period ends, whether the job ran or
// not. A sensitive club's claim and a claim of a university without a rep
// are platform staff's from the start: nobody is reminded, nothing
// escalates.
const (
	// ClaimReminderAfter is when the reps are reminded (the 5th day).
	ClaimReminderAfter = 5 * 24 * time.Hour
	// ClaimEscalateAfter is when an undecided claim goes to platform staff.
	ClaimEscalateAfter = 7 * 24 * time.Hour

	// ClaimCreatedTaskName tells the reps about a new claim (mechanism A).
	ClaimCreatedTaskName = "directory.notify_claim_created"
	// ClaimRemindersJobName reminds the reps and announces escalations.
	ClaimRemindersJobName = "directory.claim_reminders"

	templateClaimPending   = "directory.claim_pending"
	templateClaimReminder  = "directory.claim_reminder"
	templateClaimEscalated = "directory.claim_escalated"

	claimJobBatch = page.MaxLimit
)

// claimNotice is the payload of ClaimCreatedTaskName (IDs only).
type claimNotice struct {
	ClaimID uuid.UUID `json:"claim_id"`
}

// escalated reports whether a claim created at createdAt is platform
// staff's by now.
func escalated(createdAt, now time.Time) bool { return !now.Before(createdAt.Add(ClaimEscalateAfter)) }

// Jobs returns the scheduled jobs of directory (01 §7 addition, 06 H-58).
func (m Module) Jobs() []queue.Job {
	return []queue.Job{{Name: ClaimRemindersJobName, Cron: "0 9 * * *", Queue: queue.Default, Unique: 23 * time.Hour,
		Handle: m.svc.claimReminders}}
}

// handleClaimCreated is directory.notify_claim_created: every rep who
// decides the claim gets an in-app notice (none for a claim that is no
// longer pending or that platform staff decide).
func (s *service) handleClaimCreated(ctx context.Context, payload []byte) error {
	var n claimNotice
	if err := json.Unmarshal(payload, &n); err != nil || n.ClaimID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("directory: bad claim notice payload: %w", errors.Join(err, errors.New("claim_id required"))))
	}
	c, err := s.d.Store.GetClaim(ctx, n.ClaimID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil // erased meanwhile
	case err != nil:
		return err
	case c.Status != ClaimPending || escalated(c.CreatedAt, s.now()):
		return nil
	}
	reps, err := s.claimReps(ctx, c)
	if err != nil {
		return err
	}
	return s.tellAll(ctx, reps, c, templateClaimPending, "directory.claim_pending:")
}

// claimReminders is directory.claim_reminders: the pending claims at least
// ClaimReminderAfter old, oldest first. A rep-decided one younger than
// ClaimEscalateAfter reminds its reps; an older one tells the platform
// admins that it is theirs now. Each notice once (dedupe keys).
func (s *service) claimReminders(ctx context.Context) error {
	if s.d.Sender == nil {
		return errors.New("directory: notify is not wired")
	}
	now := s.now()
	cutoff := now.Add(-ClaimReminderAfter)
	var after *uuid.UUID
	reminded, escalations := 0, 0
	for {
		rows, err := s.d.Store.ListClaims(ctx, ClaimFilter{Status: ClaimPending, CreatedBefore: &cutoff}, after, claimJobBatch)
		if err != nil {
			return err
		}
		for _, c := range rows {
			reps, err := s.claimReps(ctx, c)
			if err != nil {
				return err
			}
			switch {
			case len(reps) == 0:
				continue // platform staff's from the start
			case !escalated(c.CreatedAt, now):
				if err := s.tellAll(ctx, reps, c, templateClaimReminder, "directory.claim_reminder:"); err != nil {
					return err
				}
				reminded++
			default:
				if s.d.PlatformAdmins == nil {
					return errors.New("directory: the platform admin lookup is not wired")
				}
				admins, err := s.d.PlatformAdmins(ctx)
				if err != nil {
					return fmt.Errorf("directory: platform admins: %w", err)
				}
				if err := s.tellAll(ctx, admins, c, templateClaimEscalated, "directory.claim_escalated:"); err != nil {
					return err
				}
				escalations++
			}
		}
		if len(rows) < claimJobBatch {
			break
		}
		after = new(rows[len(rows)-1].ID)
	}
	if reminded+escalations > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "club claim reminders", slog.Int("reminded", reminded),
			slog.Int("escalated", escalations))
	}
	return nil
}

// claimReps returns the reps who decide (or decided until the escalation)
// claim c: none for a sensitive club or a university without a rep.
func (s *service) claimReps(ctx context.Context, c ClubClaim) ([]uuid.UUID, error) {
	club, err := s.d.Store.GetClub(ctx, c.ClubOrgID)
	if err != nil {
		return nil, err
	}
	_, reps, err := s.repApproval(ctx, club, c.ClaimantUserID)
	return reps, err
}

// tellAll sends template to every recipient (one dedupe key per claim and
// recipient). A permanent failure for one recipient is logged and
// skipped; anything else fails the task (retried).
func (s *service) tellAll(ctx context.Context, to []uuid.UUID, c ClubClaim, tmpl, dedupe string) error {
	if s.d.Sender == nil {
		return errors.New("directory: notify is not wired")
	}
	club := c.ClubOrgID
	for _, id := range to {
		if id == c.ClaimantUserID {
			continue
		}
		user := id
		_, err := s.d.Sender.Send(ctx, notify.Message{
			UserID: &user, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: tmpl,
			SourceOrgID: &club, Data: map[string]any{"claim_id": c.ID, "club_org_id": c.ClubOrgID},
			LinkPath: notify.AdminClubClaimLink(c.ID), DedupeKey: dedupe + c.ID.String() + ":" + id.String(),
		})
		switch {
		case err == nil:
		case notify.IsPermanent(err):
			s.d.Log.LogAttrs(ctx, slog.LevelError, "club claim notice not deliverable", slog.String("template", tmpl),
				slog.String("claim_id", c.ID.String()), slog.String("err", err.Error()))
		default:
			return err
		}
	}
	return nil
}
