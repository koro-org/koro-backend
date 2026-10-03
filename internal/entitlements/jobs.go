package entitlements

import (
	"context"
	"log/slog"
	"time"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/queue"
)

// Job names (01 §7).
const (
	// ExpireJobName marks ended rights expired (hourly at :45, default queue).
	ExpireJobName = "entitlements.expire"

	expireCron   = "45 * * * *"
	expireUnique = 55 * time.Minute
	batchSize    = 1000 // 01 §7: ≤ 1000 rows per transaction
)

// expireJob is entitlements.expire (01 §7): active rights whose valid_to
// has passed become expired. Consumption already refuses them (the
// conditional UPDATE checks the window); the job makes the status and the
// lists honest. Each batch and its audit records share a transaction.
func (s *service) expireJob() queue.Job {
	return queue.Job{
		Name: ExpireJobName, Cron: expireCron, Queue: queue.Default, Unique: expireUnique,
		Handle: s.expire,
	}
}

func (s *service) expire(ctx context.Context) error {
	sys := authz.SystemActor(ExpireJobName)
	if err := authz.Can(ctx, sys, authz.EntitlementsEntitlementExpire, authz.Resource{Type: authz.TypeEntitlement}); err != nil {
		return queue.Permanent(err)
	}
	now, total := s.d.Clock.Now(), 0
	for {
		n := 0
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			rows, err := s.d.Store.Expire(ctx, now, batchSize)
			if err != nil {
				return err
			}
			n = len(rows)
			for _, e := range rows {
				if err := s.d.Audit.Log(ctx, audit.Event{
					Action: string(authz.EntitlementsEntitlementExpire), Outcome: audit.Success, OrgID: &e.OrgID,
					ResourceType: authz.TypeEntitlement, ResourceID: &e.ID,
					Metadata: map[string]any{"feature": string(e.Feature), "quantity_used": e.QuantityUsed},
				}.By(sys)); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		total += n
		if n < batchSize {
			break
		}
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "entitlements expired", slog.Int("count", total))
	return nil
}
