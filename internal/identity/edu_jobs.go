package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/notify"
	"koro/internal/platform/applink"
	"koro/internal/platform/civil"
	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// Student verification jobs and notices (01 §7, §6.2; BE-018).
const (
	// EduExpireJobName lapses the verifications whose year ran out
	// (00:30; system action identity.edu_verification.expire).
	EduExpireJobName = "identity.edu_expire"
	// EduRenewalRemindersJobName reminds students 30, 7 and 1 days before
	// their verification ends (10:00).
	EduRenewalRemindersJobName = "identity.edu_renewal_reminders"
	// SendEduNoticeTaskName sends one student verification notice
	// (mechanism A: IDs and a kind only).
	SendEduNoticeTaskName = "identity.send_edu_notice"
	// RemoveRejectedDocumentTaskName consumes file.rejected: a student
	// document the virus scan (or the metadata removal) rejected leaves
	// its pending verification (01 §6.2: the owning module drops its
	// reference and deletes the file).
	RemoveRejectedDocumentTaskName = "identity.remove_rejected_document"

	eduNoticeTemplate = "identity.edu_notice"
	eduBatch          = 1000 // rows per transaction (01 §7)
)

// Notice kinds of identity.edu_notice.
const (
	eduNoticeReminder = "renewal_reminder" // days_left 30, 7 or 1
	eduNoticeApproved = "document_approved"
	eduNoticeRejected = "document_rejected"
	eduNoticeLapsed   = "lapsed"
)

// eduReminderDays are the reminder offsets of 01 §7 (days before the end).
var eduReminderDays = []int{30, 7, 1}

// eduNotice is the payload of identity.send_edu_notice (05 §8.4-1: IDs and
// a kind; the worker reads the rest).
type eduNotice struct {
	UserID         uuid.UUID `json:"user_id"`
	VerificationID uuid.UUID `json:"verification_id"`
	Kind           string    `json:"kind"`
	DaysLeft       int       `json:"days_left,omitempty"`
}

// dedupe is the task ID and the notify deduplication key of a notice:
// one e-mail per verification, kind and reminder day.
func (n eduNotice) dedupe() string {
	key := "identity.edu_notice:" + n.VerificationID.String() + ":" + n.Kind
	if n.DaysLeft > 0 {
		key += ":" + strconv.Itoa(n.DaysLeft)
	}
	return key
}

// notifyEdu enqueues a notice after the transaction commits (mechanism A:
// best effort).
func (s *service) notifyEdu(ctx context.Context, n eduNotice) {
	s.d.Queue.AfterCommit(ctx, SendEduNoticeTaskName, n, n.dedupe())
}

// handleEduNotice is the identity.send_edu_notice handler. Notices are
// account notices of the student role (transactional, not marketing: a
// minor gets them too, 03 §2.3).
func (s *service) handleEduNotice(ctx context.Context, payload []byte) error {
	var n eduNotice
	if err := json.Unmarshal(payload, &n); err != nil || n.UserID == uuid.Nil() || n.VerificationID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("identity: edu notice payload: %w", errors.Join(err, errors.New("missing ids"))))
	}
	switch n.Kind {
	case eduNoticeReminder, eduNoticeApproved, eduNoticeRejected, eduNoticeLapsed:
	default:
		return queue.Permanent(fmt.Errorf("identity: unknown edu notice %q", n.Kind))
	}
	u, err := s.d.Store.User(ctx, n.UserID)
	switch {
	case isNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if u.Status != StatusActive || u.Email == "" {
		return nil
	}
	if n.Kind == eduNoticeReminder { // renewed or gone meanwhile: nothing to remind
		v, err := s.d.Store.EduVerification(ctx, n.VerificationID)
		if isNotFound(err) || err == nil && !v.activeAt(s.now()) {
			return nil
		} else if err != nil {
			return err
		}
	}
	link := applink.Page(s.d.LinkBaseURL, notify.MyStudentVerificationLink().String())
	data := map[string]any{"name": u.FirstName, "kind": n.Kind, "link": link}
	if n.DaysLeft > 0 {
		data["days_left"] = strconv.Itoa(n.DaysLeft)
	}
	msg := notify.Message{
		UserID: &u.ID, ToEmail: u.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
		Template: eduNoticeTemplate, Locale: u.Locale, Data: data, DedupeKey: n.dedupe(),
	}
	if _, err := s.d.Mailer.Send(ctx, msg); err != nil {
		if notify.IsPermanent(err) {
			return queue.Permanent(err)
		}
		return err
	}
	return nil
}

// expireEdu is identity.edu_expire (01 §7): verified rows whose year ran
// out become expired, each with user.edu_verification_lapsed (O) and an
// audit record of the system action, in batches of eduBatch rows per
// transaction. Idempotent: an expired row is not selected again.
func (s *service) expireEdu(ctx context.Context) error {
	sys := authz.SystemActor(EduExpireJobName)
	if err := authz.Can(ctx, sys, authz.IdentityEduVerificationExpire, authz.Resource{Type: authz.TypeEduVerification}); err != nil {
		return err
	}
	total, err := db.Batches(ctx, s.d.Tx, eduBatch, func(ctx context.Context) (int, error) {
		rows, err := s.d.Store.ExpireEduVerifications(ctx, s.now(), eduBatch)
		if err != nil {
			return 0, err
		}
		for _, v := range rows {
			env, err := domainevent.New(domainevent.UserEduVerificationLapsed{UserID: v.UserID, VerificationID: v.ID}, eduAggType, v.ID)
			if err != nil {
				return 0, err
			}
			if err := s.d.Outbox.Add(ctx, env); err != nil {
				return 0, err
			}
			s.notifyEdu(ctx, eduNotice{UserID: v.UserID, VerificationID: v.ID, Kind: eduNoticeLapsed})
			if err := s.d.Audit.Log(ctx, event(authz.IdentityEduVerificationExpire, audit.Success, resourceEdu, v.ID,
				map[string]any{"subject_user_id": v.UserID.String()}).By(sys)); err != nil {
				return 0, err
			}
		}
		return len(rows), nil
	})
	if err != nil {
		return fmt.Errorf("%s: %w", EduExpireJobName, err)
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "student verifications expired", slog.String("job", EduExpireJobName), slog.Int("expired", total))
	return nil
}

// remindEdu is identity.edu_renewal_reminders (01 §7): a reminder to every
// verification that ends on the Europe/Istanbul day 30, 7 or 1 days from
// today. The task ID (and notify's dedupe key) is per verification and
// offset, so a rerun of the same day sends nothing twice.
func (s *service) remindEdu(ctx context.Context) error {
	today := civil.Today(s.now())
	total := 0
	for _, d := range eduReminderDays {
		from := today.AddDays(d).In(clock.Istanbul()).UTC()
		to := today.AddDays(d + 1).In(clock.Istanbul()).UTC()
		var after *uuid.UUID
		for {
			rows, err := s.d.Store.EduExpiringBetween(ctx, from, to, after, eduBatch)
			if err != nil {
				return fmt.Errorf("%s: %w", EduRenewalRemindersJobName, err)
			}
			for _, v := range rows {
				n := eduNotice{UserID: v.UserID, VerificationID: v.ID, Kind: eduNoticeReminder, DaysLeft: d}
				if err := s.d.Queue.Enqueue(ctx, SendEduNoticeTaskName, n, n.dedupe()); err != nil {
					return fmt.Errorf("%s: %w", EduRenewalRemindersJobName, err)
				}
				total++
			}
			if len(rows) < eduBatch || ctx.Err() != nil {
				break
			}
			last := rows[len(rows)-1].ID
			after = &last
		}
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "student verification reminders queued", slog.String("job", EduRenewalRemindersJobName),
		slog.Int("queued", total))
	return ctx.Err()
}

// eduJobs are the scheduled jobs of the student verification (01 §7).
func (s *service) eduJobs() []queue.Job {
	return []queue.Job{
		{Name: EduExpireJobName, Cron: "30 0 * * *", Queue: queue.Default, Unique: 23 * time.Hour, Handle: s.expireEdu},
		{Name: EduRenewalRemindersJobName, Cron: "0 10 * * *", Queue: queue.Default, Unique: 23 * time.Hour, Handle: s.remindEdu},
	}
}

// rejectedDocumentJob is the system actor of the clean-up after a virus
// finding.
const rejectedDocumentJob = "identity.remove_rejected_document"

// handleFileRejected consumes file.rejected (01 §6.2). A student document
// still in review is withdrawn: its pending verification is deleted (it was
// never decided, so no reviewer can be named) and the file is removed; the
// uploader already got files' notice. A decided verification keeps its row
// (an approval needs a clean file, so this is a document that was rejected
// or superseded); its file goes too.
func (s *service) handleFileRejected(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.FileRejected](e)
	if err != nil {
		return queue.Permanent(err)
	}
	if p.AttachedModule != eduOwner {
		return nil
	}
	sys := authz.SystemActor(rejectedDocumentJob)
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		v, found, err := s.d.Store.EduVerificationOfFile(ctx, p.FileID)
		if err != nil || !found {
			return err
		}
		v, err = s.d.Store.LockEduVerification(ctx, v.ID)
		if err != nil {
			return err
		}
		meta := map[string]any{"subject_user_id": v.UserID.String(), "file_id": p.FileID.String(), "reason": p.Reason,
			"operation": "document_rejected_by_scan"}
		if v.Status == EduPending {
			if _, err := s.d.Store.DeleteEduVerification(ctx, v.ID); err != nil {
				return err
			}
			meta["withdrawn"] = true
		} else {
			v.DocumentFileID = nil
			if err := s.d.Store.UpdateEduVerification(ctx, v); err != nil {
				return err
			}
		}
		if err := s.removeFile(ctx, sys, p.FileID); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, event(authz.IdentityEduVerificationSubmitDocument, audit.Failure, resourceEdu, v.ID, meta).By(sys))
	})
}

// removeFile deletes a student document through Deps.Files (nil: the file
// stays, logged without personal data).
func (s *service) removeFile(ctx context.Context, a authz.Actor, fileID uuid.UUID) error {
	if s.d.Files == nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "student document not removed: files not wired",
			slog.String("file_id", fileID.String()))
		return nil
	}
	return s.d.Files.Delete(ctx, a, fileID)
}
