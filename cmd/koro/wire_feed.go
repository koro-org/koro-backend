package main

import (
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/feed"
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
// name; the active memberships behind the automatic follows),
// c.directory.svc (sensitive clubs; nil → every club counts as sensitive
// and gets no automatic follow), c.students.svc (the readers' audience
// keys; nil → 'all' only), c.events.svc (event_share; nil → no event can
// be shared), c.files.svc (images; nil → no media) and c.infra.cfg.Feed
// (FEED_REVIEW_THRESHOLD).
//
// Provided, set in build: svc (feed.Posts for messaging) and the feed KVKK
// provider (posts.author_user_id; likes, saves, hides, follows, mutes).
type feedWiring struct {
	store    feed.Store
	newStore func(c *container) feed.Store
	tx       db.TxManager

	mod   feed.Module
	svc   feed.Service // set by build
	built feed.Store   // the store build used (wiring and matrix tests seed it)
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
		Store: store, Tx: tx, Outbox: c.infra.outbox, Audit: c.audit.svc, Orgs: c.orgs.svc,
		Idempotency: c.infra.idempotency, Clock: c.infra.clock, Log: c.infra.log.With("module", "feed"),
		ReviewThreshold: c.infra.cfg.Feed.ReviewThreshold,
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
	w.mod, w.svc, w.built = mod, mod.Service, store
	c.privacy = append(c.privacy, mod.Privacy)
	return nil
}

// tasks: post.published → feed.notify_followers (a no-op hook in P1,
// T-84); membership.started/ended → feed.auto_follow /
// feed.remove_auto_follow; club.sensitivity_changed →
// feed.drop_sensitive_auto_follows; the job feed.reconcile_counters (01 §7:
// hourly at :35, low). post.submitted_for_review's moderator notice and
// feed.purge_removed come with BE-032 and BE-057.
func (w *feedWiring) tasks(c *container) {
	w.mod.Subscribe(c.infra.subs)
	for _, j := range w.mod.Jobs() {
		c.infra.jobs.Schedule(j)
	}
}

// routes: the 9 operations of BE-027 and the 12 of BE-029 (04 §2.7);
// w.svc is nil under `koro openapi`, which Register tolerates.
func (w *feedWiring) routes(c *container, api huma.API) { feed.Register(api, w.svc) }
