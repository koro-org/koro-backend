package main

import (
	"context"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/platform/db"
)

// feedWiring wires the feed module (L9; 01 §4.14). First owner: BE-027.
// Only tickets of the feed module edit this file (BE-001 R4).
//
// Seams (nil in production):
//
//	store     nil → feed.NewGormStore over the app_rw pool
//	newStore  tests: builds the in-memory store over the container clock
//	tx        nil → the app_rw pool's TxManager; tests preset db.NopTx
//
// Consumed: c.audit.svc, c.orgs.svc (the author org's kind, status and
// name; the active memberships behind the automatic follows; the writers
// who receive the moderation notices), c.directory.svc (sensitive clubs;
// nil → every club counts as sensitive and gets no automatic follow),
// c.students.svc (the readers' audience keys; nil → 'all' only),
// c.events.svc (event_share; nil → no event can be shared), c.files.svc
// (images; nil → no media), c.outreach.svc (Compile of targeted posts; nil
// → 503), c.entitlements.svc (the targeted_post quota; nil → a company's
// targeted post is 503), c.notify.svc (the moderation notices, read at
// call time), identity's store (the moderators in effect, 06 H-33; T-249)
// and c.notify.platformAdmins (the fallback recipients), c.infra.queue
// (the org moderation and report outcome notice tasks) and
// c.infra.cfg.Feed (FEED_REVIEW_THRESHOLD, FEED_REPORT_HIDE_THRESHOLD).
//
// Provided, set in build: svc (feed.Posts for messaging) and the feed KVKK
// provider (posts.author_user_id; likes, saves, hides, follows, mutes;
// reports; a moderator's decisions).
type feedWiring struct {
	store    feed.Store
	newStore func(c *container) feed.Store
	tx       db.TxManager

	mod   feed.Module
	svc   feed.Service // set by build
	built feed.Store   // the store build used (wiring and matrix tests seed it)
	late  *container   // the container, for the matrix fixtures (students' audience)
}

func (w *feedWiring) build(c *container) error {
	store, tx := w.store, w.tx
	if w.newStore != nil {
		store = w.newStore(c)
	}
	if c.infra.db != nil {
		if store == nil {
			store = feed.NewGormStore(c.infra.db.ORM)
		}
		if tx == nil {
			tx = c.infra.db.Tx
		}
	}
	if store == nil || tx == nil {
		return errors.New("feed: no store (database not opened)")
	}
	if c.orgs.svc == nil {
		return errors.New("feed: orgs is not built")
	}
	d := feed.Deps{
		Store: store, Tx: tx, Outbox: c.infra.outbox, Queue: c.infra.queue, Audit: c.audit.svc, Orgs: c.orgs.svc,
		Idempotency: c.infra.idempotency, Clock: c.infra.clock, Log: c.infra.log.With("module", "feed"),
		ReviewThreshold: c.infra.cfg.Feed.ReviewThreshold, ReportHideThreshold: c.infra.cfg.Feed.ReportHideThreshold,
		Sender: feedSender{c: c}, Staff: feedStaff{c: c},
	}
	if svc := c.outreach.svc; svc != nil {
		d.Targeting = svc
	}
	if svc := c.entitlements.svc; svc != nil {
		d.Entitlements = svc
	}
	if svc := c.students.svc; svc != nil {
		d.Viewers = svc
	}
	if svc := c.directory.svc; svc != nil {
		d.Clubs = svc
	}
	if svc := c.events.svc; svc != nil {
		d.Events = svc
	}
	if svc := c.files.svc; svc != nil {
		d.Files = svc
	}
	mod, err := feed.New(d)
	if err != nil {
		return err
	}
	w.mod, w.svc, w.built, w.late = mod, mod.Service, store, c
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// feedSender adapts notify (L3) to feed.Sender, read at call time; a
// missing sender fails the task (retried), it never drops a notice.
type feedSender struct{ c *container }

func (n feedSender) Send(ctx context.Context, m notify.Message) (uuid.UUID, error) {
	if n.c.notify.svc == nil {
		return uuid.Nil(), errors.New("feed: notify is not wired")
	}
	return n.c.notify.svc.Send(ctx, m)
}

// feedStaff lists the moderation staff in effect (06 H-33: active or
// revoke_pending; a pending request does not count) for feed.Staff
// (T-249). Moderators come from identity's store through the composition
// root (identity exposes PlatformAdmins only), the platform admins from
// the notify hook identity fills.
type feedStaff struct{ c *container }

func (s feedStaff) Moderators(ctx context.Context) ([]uuid.UUID, error) {
	st := s.c.identity.store
	if st == nil {
		if s.c.infra.db == nil {
			return nil, errors.New("feed: the moderator lookup has no identity store")
		}
		st = identity.NewGormStore(s.c.infra.db.ORM)
	}
	return st.UsersWithRole(ctx, authz.PlatformModerator)
}

func (s feedStaff) PlatformAdmins(ctx context.Context) ([]uuid.UUID, error) {
	if h := s.c.notify.platformAdmins; h != nil {
		return h(ctx)
	}
	return nil, errors.New("feed: the platform admin lookup is not wired")
}

// tasks: post.published → feed.notify_followers (a no-op hook in P1,
// T-84); membership.started/ended → feed.auto_follow /
// feed.remove_auto_follow; club.sensitivity_changed →
// feed.drop_sensitive_auto_follows; post.submitted_for_review →
// feed.notify_review_queue, content_report.created →
// feed.notify_report_created, post.moderated → feed.notify_post_moderated
// (BE-032); the tasks feed.notify_org_moderated and
// feed.notify_report_resolved; the job feed.reconcile_counters (01 §7:
// hourly at :35, low). feed.purge_removed comes with BE-057.
func (w *feedWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
	for _, t := range w.mod.Tasks() {
		c.infra.jobs.Handle(t)
	}
}

// routes: the 9 operations of BE-027, the 12 of BE-029 and the 9 of
// BE-032 (04 §2.7); w.svc is nil under `koro openapi`, which Register
// tolerates.
func (w *feedWiring) routes(c *container, api huma.API) { feed.Register(api, w.svc) }
