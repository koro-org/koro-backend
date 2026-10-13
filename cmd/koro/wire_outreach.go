package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/notify"
	"koro/internal/outreach"
	"koro/internal/platform/db"
)

// outreachWiring wires the outreach module (L8; 01 §4.13). First owner:
// BE-030. Only tickets of the outreach module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store  nil → outreach.NewGormStore over the app_rw pool; tests preset the in-memory store
//	tx     nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumed (built in lower layers): c.audit.svc, c.students.svc
// (Audience.Count: count_segment, k ≥ 10; BE-035: Stream, the recipient
// walk), c.directory.svc (sensitive clubs, a university's org), c.orgs.svc
// (the org's status; BE-035: a club's active members), the platform's rate
// limiter (the daily count budget per org, OUTREACH_COUNT_DAILY_LIMIT) and
// the idempotency keeper. BE-035 (the dispatch): c.infra.outbox
// (outreach.dispatch_scheduled), c.entitlements.svc (targeted_dispatch),
// c.identity.svc (active adult accounts, language, e-mail address),
// c.consent.svc (the commercial filter, FilterActive), c.notify.svc (read
// at call time), c.feed.svc (L9, above outreach: a post's facts, read at
// call time), OUTREACH_UNSUBSCRIBE_URL and IDENTITY_LINK_BASE_URL (the web
// origin of e-mail links).
//
// Provided, set in build: svc (outreach.Service: Targeting.Compile,
// ScheduleDispatch, DiscardDispatch and DispatchOfPost for feed's targeted
// posts, BE-032/BE-035) and the retention sweeper of outreach_dispatches
// (outreach_dispatch, 2 years).
type outreachWiring struct {
	store outreach.Store
	tx    db.TxManager

	mod  outreach.Module
	svc  outreach.Service // set by build
	late *container       // the container, for the matrix fixtures (the feed's posts)
}

func (w *outreachWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if c.infra.db != nil {
		if store == nil {
			store = outreach.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("outreach: no store (database not opened)")
	}
	d := outreach.Deps{
		Store: store, Tx: tx, Audit: c.audit.svc, Limiter: c.infra.limiter, Idempotency: c.infra.idempotency,
		Clock: c.infra.clock, Log: c.infra.log.With("module", "outreach"),
		DailyCountLimit: c.infra.cfg.Outreach.CountDailyLimit,
		Sender:          outreachSender{c: c}, Posts: outreachPosts{c: c},
		UnsubscribeURL: c.infra.cfg.Outreach.UnsubscribeURL, LinkBaseURL: c.infra.cfg.Identity.LinkBaseURL,
	}
	if c.infra.outbox != nil {
		d.Outbox = c.infra.outbox
	}
	if svc := c.students.svc; svc != nil {
		d.Audience, d.Recipients = svc, svc
	}
	if svc := c.directory.svc; svc != nil {
		d.Catalog = svc
	}
	if svc := c.orgs.svc; svc != nil {
		d.Orgs, d.Members = svc, svc
	}
	if svc := c.entitlements.svc; svc != nil {
		d.Entitlements = svc
	}
	if svc := c.identity.svc; svc != nil {
		d.Users = svc
	}
	if svc := c.consent.svc; svc != nil {
		d.Consents = svc
	}
	mod, err := outreach.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.late = mod, mod.Service, c
	c.sweepers = append(c.sweepers, mod.Sweeper)
	return nil
}

// outreachSender adapts notify (L3) to outreach.Sender, read at call time;
// a missing sender fails the run (retried), it never drops a notification.
type outreachSender struct{ c *container }

func (s outreachSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if s.c.notify.svc == nil {
		return uuid.Nil(), errors.New("outreach: notify is not wired")
	}
	return s.c.notify.svc.Send(ctx, m)
}

// outreachPosts reads a post's facts from the feed (L9, built after
// outreach: 01 §5.3 "feed ↔ outreach"), at call time. Without the feed no
// post can be dispatched (an error, not a 404: the run is retried).
type outreachPosts struct{ c *container }

func (p outreachPosts) DispatchPost(ctx context.Context, postID uuid.UUID) (outreach.PostInfo, error) {
	if p.c.feed.svc == nil {
		return outreach.PostInfo{}, errors.New("outreach: the feed is not wired")
	}
	s, err := p.c.feed.svc.Get(ctx, postID)
	if err != nil {
		return outreach.PostInfo{}, err
	}
	return s.DispatchInfo(), nil
}

// tasks schedules outreach.recount_segments (01 §7: 06:00, low) and
// outreach.resume_dispatches (every 10 minutes, default), and subscribes
// outreach.run_dispatch (outreach.dispatch_scheduled) and
// outreach.start_dispatch (post.published) (BE-035).
func (w *outreachWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 6 segment and audience count operations of BE-030 and the 4
// dispatch operations of BE-035 (04 §2.8); w.svc is nil under `koro
// openapi`, which Register tolerates.
func (w *outreachWiring) routes(c *container, api huma.API) { outreach.Register(api, w.svc) }
