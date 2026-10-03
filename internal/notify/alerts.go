package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/queue"
)

// The audit alert job (06 H-40, H-43). Some notifications follow from what
// other modules write to the audit log rather than from an event of their
// own: a break-glass read of a user's data (03 §6.3: the user is told) and
// an admin entitlement grant (06 H-40: every platform admin is told). The
// job reads the chained audit log in chain_seq order (gapless, never
// reordered once chained), sends in-app notifications for the matching
// records and keeps its position in a CursorStore. Each notification has the
// dedupe key "audit:<audit event id>:<recipient>", so a rerun after a crash
// or a lost cursor never notifies twice.
const (
	// AuditAlertJobName is the job and its SystemActor name.
	AuditAlertJobName = "notify.send_audit_alerts"
	auditAlertCron    = "* * * * *"
	auditAlertUnique  = 50 * time.Second
	auditCursorName   = "notify.audit_alerts"
	auditBatch        = 200
	auditMaxBatches   = 25 // per run; the next minute continues
	// AuditAlertLookback is where the job starts without a cursor (first
	// deploy, lost Redis): the chain head of one day ago.
	AuditAlertLookback = 24 * time.Hour

	// Templates and links of the alerts.
	templateSupportAccess      = "notify.support_access"
	templateEntitlementGranted = "notify.entitlement_granted"
	linkSecurityEvents         = "/me/security-events"
	linkAdminAudit             = "/admin/audit-events"
)

func (s *service) auditAlertJob() queue.Job {
	return queue.Job{Name: AuditAlertJobName, Cron: auditAlertCron, Queue: queue.Default, Unique: auditAlertUnique,
		Handle: s.sendAuditAlerts}
}

// sendAuditAlerts runs one pass of the audit alert job.
func (s *service) sendAuditAlerts(ctx context.Context) error {
	if err := authz.Can(ctx, authz.SystemActor(AuditAlertJobName), authz.NotifyMessageSend, authz.Resource{}); err != nil {
		return queue.Permanent(err)
	}
	pos, ok, err := s.d.Cursor.Load(ctx, auditCursorName)
	if err != nil {
		return fmt.Errorf("notify: audit alert cursor: %w", err)
	}
	if !ok {
		head, err := s.d.AuditFeed.HeadAt(ctx, s.d.Clock.Now().Add(-AuditAlertLookback))
		if err != nil {
			return fmt.Errorf("notify: audit alert start: %w", err)
		}
		pos = head.Seq
	}
	start, sent := pos, 0
	defer func() {
		if pos != start || sent > 0 {
			s.d.Log.LogAttrs(ctx, slog.LevelInfo, "audit alerts sent",
				slog.Int64("from_chain_seq", start), slog.Int64("to_chain_seq", pos), slog.Int("notifications", sent))
		}
	}()
	for range auditMaxBatches {
		rows, err := s.d.AuditFeed.Chained(ctx, pos, auditBatch)
		if err != nil {
			return fmt.Errorf("notify: audit alert read: %w", err)
		}
		for _, r := range rows {
			n, err := s.alertFor(ctx, r)
			sent += n
			if err != nil {
				return s.saveCursor(ctx, pos, err)
			}
			if r.ChainSeq != nil {
				pos = *r.ChainSeq
			}
		}
		if err := s.saveCursor(ctx, pos, nil); err != nil {
			return err
		}
		if len(rows) < auditBatch {
			return nil
		}
	}
	return nil
}

// saveCursor stores pos and returns cause (or the save error).
func (s *service) saveCursor(ctx context.Context, pos int64, cause error) error {
	if err := s.d.Cursor.Save(context.WithoutCancel(ctx), auditCursorName, pos); err != nil && cause == nil {
		return fmt.Errorf("notify: audit alert cursor: %w", err)
	}
	return cause
}

// alertFor sends the notifications one audit record calls for.
func (s *service) alertFor(ctx context.Context, r audit.Record) (int, error) {
	if r.Outcome != audit.Success {
		return 0, nil
	}
	meta, ok := metadataOf(r)
	if !ok {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "audit alert: unreadable metadata", slog.String("audit_event_id", r.ID.String()))
	}
	sent := 0
	if bg, _ := meta[audit.MetaBreakGlass].(bool); bg {
		if raw, _ := meta[audit.MetaSubjectUserID].(string); raw != "" {
			subject, err := uuid.Parse(raw)
			if err == nil && subject != uuid.Nil() && (r.ActorUserID == nil || *r.ActorUserID != subject) {
				if err := s.alert(ctx, r, subject, templateSupportAccess, linkSecurityEvents, nil); err != nil {
					return sent, err
				}
				sent++
			}
		}
	}
	if r.Action == string(authz.EntitlementsEntitlementGrant) {
		if s.d.Admins == nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "audit alert: no platform admin resolver wired; admin grant alert skipped (06 H-40)",
				slog.String("audit_event_id", r.ID.String()))
			return sent, nil
		}
		admins, err := s.d.Admins.PlatformAdmins(ctx)
		if err != nil {
			return sent, fmt.Errorf("notify: platform admins: %w", err)
		}
		data := map[string]any{}
		if r.ResourceID != nil {
			data["entitlement_id"] = *r.ResourceID
		}
		if r.OrgID != nil {
			data["org_id"] = *r.OrgID
		}
		for _, admin := range admins {
			if err := s.alert(ctx, r, admin, templateEntitlementGranted, linkAdminAudit, data); err != nil {
				return sent, err
			}
			sent++
		}
	}
	return sent, nil
}

// metadataOf decodes the metadata object of r; ok=false when it is not
// one (the record then matches on its action only).
func metadataOf(r audit.Record) (map[string]any, bool) {
	meta := map[string]any{}
	if len(r.Metadata) == 0 {
		return meta, true
	}
	if json.Unmarshal(r.Metadata, &meta) != nil {
		return map[string]any{}, false
	}
	return meta, true
}

// alert sends one in-app alert. A permanent failure (a recipient that can
// no longer receive) is logged and skipped so that one record cannot stall
// the job; anything transient stops the pass and is retried.
func (s *service) alert(ctx context.Context, r audit.Record, to uuid.UUID, template, link string, data map[string]any) error {
	d := map[string]any{"audit_event_id": r.ID}
	for k, v := range data {
		d[k] = v
	}
	_, err := s.Send(ctx, Message{
		UserID: &to, Channel: ChannelInApp, Category: CategoryTransactional, Template: template,
		Data: d, LinkPath: link, DedupeKey: "audit:" + r.ID.String() + ":" + to.String(),
	})
	if err != nil && IsPermanent(err) {
		s.d.Log.LogAttrs(ctx, slog.LevelError, "audit alert not deliverable",
			slog.String("audit_event_id", r.ID.String()), slog.String("user_id", to.String()), slog.String("error", err.Error()))
		return nil
	}
	return err
}
